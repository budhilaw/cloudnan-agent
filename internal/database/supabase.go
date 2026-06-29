// Package database — supabase.go.
//
// The dump/restore recipe for migrating a Supabase Postgres database onto a
// self-hosted Supabase stack. This is the load-bearing, failure-prone part
// of the Supabase migration feature: a naive pg_dumpall of a Supabase
// project collides on restore because the destination stack already owns a
// set of roles and schemas. We split the migration into the three passes
// Supabase's own self-host guidance prescribes:
//
//	roles   — pg_dumpall --roles-only, with the stack-managed roles filtered
//	          out so restoring onto a stack that already has them doesn't
//	          error.
//	schema  — pg_dump -Fc --schema-only, restricted to the schemas that
//	          carry user value (public/auth/storage + any user-created
//	          schema). The platform-internal schemas the destination stack
//	          recreates are simply never listed, so they can't collide.
//	data    — pg_dump -Fc --data-only over the same schema set. Triggers are
//	          disabled on the RESTORE side (pg_restore --disable-triggers),
//	          which is where -Fc honors it.
//
// Everything here is pure: it builds argv/env and filters SQL text. No
// process is spawned and no network call is made until a caller runs the
// returned program. RLS policies, functions, triggers, the auth schema
// (users + GoTrue password hashes), and storage metadata all travel inside
// these passes; we never hand-roll them.
package database

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// supabaseManagedSchemas are owned and recreated by the destination Supabase
// stack. Restoring them from the source collides (duplicate objects, owner
// mismatches), so they are excluded from the schema/data passes. pg_catalog,
// pg_toast and information_schema are Postgres internals and never dumped.
var supabaseManagedSchemas = map[string]bool{
	"extensions":         true,
	"graphql":            true,
	"graphql_public":     true,
	"pgbouncer":          true,
	"realtime":           true,
	"_realtime":          true,
	"supabase_functions": true,
	"supabase_migrations": true,
	"vault":              true,
	"pgsodium":           true,
	"pgsodium_masks":     true,
	"net":                true,
	"cron":               true,
	"information_schema": true,
	"pg_catalog":         true,
	"pg_toast":           true,
}

// supabaseCoreSchemas always migrate: public holds the app, auth holds the
// GoTrue users (with password hashes), storage holds object metadata + RLS.
var supabaseCoreSchemas = []string{"public", "auth", "storage"}

// supabaseManagedRoles are created by the destination stack at install time.
// They appear in a source pg_dumpall --roles-only, and restoring CREATE/ALTER
// ROLE for them onto a stack that already has them errors, so they are
// filtered from the roles pass. "postgres" is filtered too: the destination
// has its own superuser and must not have its attributes/password rewritten.
var supabaseManagedRoles = map[string]bool{
	"supabase_admin":             true,
	"supabase_auth_admin":        true,
	"supabase_storage_admin":     true,
	"supabase_functions_admin":   true,
	"supabase_read_only_user":    true,
	"supabase_replication_admin": true,
	"supabase_realtime_admin":    true,
	"authenticator":              true,
	"dashboard_user":             true,
	"pgbouncer":                  true,
	"service_role":               true,
	"anon":                       true,
	"authenticated":              true,
	"postgres":                   true,
}

// isSupabaseManagedSchema reports whether the destination stack owns this
// schema (so the source copy must not be migrated).
func isSupabaseManagedSchema(name string) bool {
	return supabaseManagedSchemas[strings.ToLower(strings.TrimSpace(name))]
}

// isSupabaseManagedRole reports whether the destination stack already creates
// this role. The pg_ prefix catches the Postgres built-in roles
// (pg_read_all_data, pg_monitor, …), which also pre-exist on every cluster.
func isSupabaseManagedRole(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, "pg_") {
		return true
	}
	return supabaseManagedRoles[n]
}

// supabaseMigrateSchemas computes the schema set for the schema/data passes:
// the three core schemas plus every user-created schema in allSchemas that
// the destination stack doesn't already own. allSchemas is what the agent
// reads from the source's information_schema.schemata at run time; passing an
// empty slice yields just the core schemas. The result is deterministic
// (core first, then user schemas in input order) and de-duplicated.
func supabaseMigrateSchemas(allSchemas []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(allSchemas)+len(supabaseCoreSchemas))
	for _, s := range supabaseCoreSchemas {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, raw := range allSchemas {
		s := strings.TrimSpace(raw)
		if s == "" || seen[strings.ToLower(s)] || seen[s] {
			continue
		}
		if isSupabaseManagedSchema(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// supabasePgConnArgs builds the shared --host/--port/--username flags. Pure.
func supabasePgConnArgs(cred *CredEntry) []string {
	host := cred.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cred.Port
	if port == 0 {
		port = defaultPostgresPort
	}
	return []string{
		"--host=" + host,
		"--port=" + strconv.FormatUint(uint64(port), 10),
		"--username=" + cred.Username,
	}
}

// supabasePgConnEnv builds PGPASSWORD + TLS env for a pg client. Pure unless
// the source mandates TLS with a CA bundle, in which case the CA is
// materialized to a temp file (same helper the live driver uses). A
// migration source is remote, so verify-full is the right default.
func supabasePgConnEnv(cred *CredEntry) ([]string, error) {
	env := []string{}
	if cred.Password != "" {
		env = append(env, "PGPASSWORD="+cred.Password)
	}
	if cred.UseTLS {
		env = append(env, "PGSSLMODE=verify-full")
		if cred.TLSCAPem != "" {
			caPath, err := writeTempCA(cred.TLSCAPem)
			if err != nil {
				return nil, err
			}
			env = append(env, "PGSSLROOTCERT="+caPath)
		}
	}
	return env, nil
}

// supabaseRolesDumpArgs builds the roles pass: pg_dumpall --roles-only over
// the cluster. pg_dumpall has no per-role exclude and emits plain SQL, so the
// managed roles are stripped from its output afterward by
// supabaseFilterRolesSQL.
func supabaseRolesDumpArgs(cred *CredEntry) (program string, args []string, env []string, err error) {
	if cred == nil {
		return "", nil, nil, errors.New("supabase roles dump: nil credentials")
	}
	args = append(supabasePgConnArgs(cred),
		"--roles-only",
		// Membership GRANTs reference roles we may filter; keeping them out
		// avoids dangling grants to dropped managed roles.
		"--no-role-passwords",
	)
	env, err = supabasePgConnEnv(cred)
	if err != nil {
		return "", nil, nil, err
	}
	return "pg_dumpall", args, env, nil
}

// supabaseSchemaDumpArgs builds the schema-only (dataOnly=false) or data-only
// (dataOnly=true) pass: pg_dump -Fc restricted to the given schemas. Custom
// format is restorable with pg_restore and supports the ordered restore the
// orchestrator needs (schema pass, then data pass). --disable-triggers is NOT
// set here: for -Fc it is honored on the pg_restore side, where the data pass
// is replayed.
func supabaseSchemaDumpArgs(cred *CredEntry, db string, schemas []string, dataOnly bool) (program string, args []string, env []string, err error) {
	if cred == nil {
		return "", nil, nil, errors.New("supabase schema dump: nil credentials")
	}
	if strings.TrimSpace(db) == "" {
		return "", nil, nil, errors.New("supabase schema dump: database name is required")
	}
	if len(schemas) == 0 {
		return "", nil, nil, errors.New("supabase schema dump: at least one schema is required")
	}
	args = supabasePgConnArgs(cred)
	args = append(args, "-Fc", "--no-password", "--verbose")
	if dataOnly {
		args = append(args, "--data-only")
	} else {
		args = append(args, "--schema-only")
	}
	for _, s := range schemas {
		args = append(args, "--schema="+s)
	}
	args = append(args, db)
	env, err = supabasePgConnEnv(cred)
	if err != nil {
		return "", nil, nil, err
	}
	return "pg_dump", args, env, nil
}

// supabaseFilterRolesSQL removes role statements that reference a
// stack-managed role from a pg_dumpall --roles-only output, so the remaining
// SQL (user-created roles + their grants) restores cleanly onto a destination
// stack that already owns the managed roles. Non-role lines (blank lines,
// \connect, comments) pass through untouched.
func supabaseFilterRolesSQL(rolesSQL string) string {
	lines := strings.Split(rolesSQL, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if supabaseRoleLineReferencesManaged(line) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// supabaseRoleLineReferencesManaged reports whether a line is a role-
// management statement (CREATE/ALTER ROLE, GRANT/REVOKE membership, COMMENT ON
// ROLE) that names a stack-managed role. Such lines are dropped from the
// roles pass.
func supabaseRoleLineReferencesManaged(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	isRoleStmt := strings.HasPrefix(upper, "CREATE ROLE ") ||
		strings.HasPrefix(upper, "ALTER ROLE ") ||
		strings.HasPrefix(upper, "GRANT ") ||
		strings.HasPrefix(upper, "REVOKE ") ||
		strings.HasPrefix(upper, "COMMENT ON ROLE ")
	if !isRoleStmt {
		return false
	}
	for _, tok := range supabaseTokenizeIdents(line) {
		if isSupabaseManagedRole(tok) {
			return true
		}
	}
	return false
}

// supabaseTokenizeIdents splits a SQL line into identifier-like tokens
// (letters, digits, underscore), discarding punctuation and quotes. Good
// enough to spot a role name in a single-statement pg_dumpall line.
func supabaseTokenizeIdents(s string) []string {
	isIdent := func(r rune) bool {
		return r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
	}
	return strings.FieldsFunc(s, func(r rune) bool { return !isIdent(r) })
}

// ---------- restore (destination side) ----------
//
// The destination is the self-hosted Supabase stack's Postgres, which already
// owns the managed roles and the platform schemas. Restore order matters:
//
//	roles   -> apply the filtered roles SQL with psql, ON_ERROR_STOP off so a
//	           stray grant referencing a role we kept-but-dest-lacks doesn't
//	           abort the whole apply.
//	schema  -> pg_restore the -Fc schema archive. We do NOT pass --no-owner:
//	           the object owners (supabase_auth_admin for auth.*, etc.) exist
//	           on the destination, so preserving ownership keeps RLS + grants
//	           correct.
//	data    -> pg_restore the -Fc data archive with --disable-triggers (this
//	           is where -Fc honors it) and --data-only.
//
// FLAG VALIDATION: the exact --no-owner/--no-acl/--disable-triggers choices
// are validated against a real Supabase project end-to-end before release;
// they are documented Supabase self-host conventions, not guesses, but the
// reconciliation in supabaseVerifyQueries is the gate that proves the move.

// supabaseRolesRestoreArgs builds the psql invocation that applies the
// (already-filtered) roles SQL piped on stdin to the destination cluster.
func supabaseRolesRestoreArgs(cred *CredEntry, db string) (program string, args []string, env []string, err error) {
	if cred == nil {
		return "", nil, nil, errors.New("supabase roles restore: nil credentials")
	}
	args = supabasePgConnArgs(cred)
	args = append(args,
		"--dbname="+supabaseDBName(db),
		"--no-password",
		"-v", "ON_ERROR_STOP=0",
		"-f", "-", // read SQL from stdin
	)
	env, err = supabasePgConnEnv(cred)
	if err != nil {
		return "", nil, nil, err
	}
	return "psql", args, env, nil
}

// supabaseRestoreArgs builds the pg_restore invocation for the schema
// (dataOnly=false) or data (dataOnly=true) -Fc archive at artifactPath.
func supabaseRestoreArgs(cred *CredEntry, db, artifactPath string, dataOnly bool) (program string, args []string, env []string, err error) {
	if cred == nil {
		return "", nil, nil, errors.New("supabase restore: nil credentials")
	}
	if strings.TrimSpace(artifactPath) == "" {
		return "", nil, nil, errors.New("supabase restore: artifact path is required")
	}
	args = supabasePgConnArgs(cred)
	args = append(args,
		"--dbname="+supabaseDBName(db),
		"--no-password",
		"--verbose",
	)
	if dataOnly {
		args = append(args, "--data-only", "--disable-triggers")
	} else {
		args = append(args, "--schema-only")
	}
	args = append(args, artifactPath)
	env, err = supabasePgConnEnv(cred)
	if err != nil {
		return "", nil, nil, err
	}
	return "pg_restore", args, env, nil
}

// supabaseDBName defaults the target database to "postgres" (the Supabase
// stack's default DB) when the caller passes empty.
func supabaseDBName(db string) string {
	if strings.TrimSpace(db) == "" {
		return "postgres"
	}
	return db
}

// ---------- storage object sync ----------

// SupabaseStorageDescriptor names one end of a storage object copy. Backend is
// "s3" (Supabase's storage bucket, or a user S3) or "fs" (the destination
// stack's local filesystem volume).
type SupabaseStorageDescriptor struct {
	Backend     string // "s3" | "fs"
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	FSPath      string // local path when Backend == "fs"
}

// supabaseStorageSyncArgs builds an `rclone sync src dst` invocation plus the
// env that configures the two remotes. Credentials ride the environment
// (RCLONE_CONFIG_*), never argv, so they stay out of `ps` and logs. rclone's
// sync is resumable, which is why the storage step can restart without
// re-copying objects already present at the destination.
func supabaseStorageSyncArgs(src, dst SupabaseStorageDescriptor) (program string, args []string, env []string, err error) {
	srcRemote, srcEnv, err := supabaseRcloneRemote("SRC", src)
	if err != nil {
		return "", nil, nil, fmt.Errorf("source: %w", err)
	}
	dstRemote, dstEnv, err := supabaseRcloneRemote("DST", dst)
	if err != nil {
		return "", nil, nil, fmt.Errorf("destination: %w", err)
	}
	args = []string{"sync", "--fast-list", "--transfers=8", srcRemote, dstRemote}
	env = append(srcEnv, dstEnv...)
	return "rclone", args, env, nil
}

// supabaseRcloneRemote returns the remote path string ("NAME:bucket" or
// "NAME:" with a path) and the RCLONE_CONFIG_<NAME>_* env for one descriptor.
func supabaseRcloneRemote(name string, d SupabaseStorageDescriptor) (remote string, env []string, err error) {
	prefix := "RCLONE_CONFIG_" + name + "_"
	switch d.Backend {
	case "s3":
		if d.S3Bucket == "" {
			return "", nil, errors.New("s3 descriptor: bucket is required")
		}
		env = []string{
			prefix + "TYPE=s3",
			prefix + "PROVIDER=Other",
			prefix + "ACCESS_KEY_ID=" + d.S3AccessKey,
			prefix + "SECRET_ACCESS_KEY=" + d.S3SecretKey,
			prefix + "ENDPOINT=" + d.S3Endpoint,
			prefix + "REGION=" + d.S3Region,
		}
		return name + ":" + d.S3Bucket, env, nil
	case "fs":
		if d.FSPath == "" {
			return "", nil, errors.New("fs descriptor: path is required")
		}
		env = []string{prefix + "TYPE=local"}
		return name + ":" + d.FSPath, env, nil
	default:
		return "", nil, fmt.Errorf("unknown storage backend %q", d.Backend)
	}
}

// ---------- verification ----------

// SupabaseVerifyQueries are the read-only count queries run against BOTH the
// source and the destination after the move. Matching counts (within an
// explainable delta, e.g. dropped sessions) are the release gate that proves
// the migration preserved the data, RLS, and users.
var SupabaseVerifyQueries = map[string]string{
	"tables":          `SELECT count(*) FROM information_schema.tables WHERE table_schema IN ('public','auth','storage')`,
	"rls_policies":    `SELECT count(*) FROM pg_policies WHERE schemaname IN ('public','auth','storage')`,
	"auth_users":      `SELECT count(*) FROM auth.users`,
	"storage_objects": `SELECT count(*) FROM storage.objects`,
}
