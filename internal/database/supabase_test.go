package database

import (
	"strings"
	"testing"
)

func TestIsSupabaseManagedSchema(t *testing.T) {
	managed := []string{"vault", "realtime", "_realtime", "supabase_functions", "extensions", "graphql", "pgsodium", "PG_CATALOG"}
	for _, s := range managed {
		if !isSupabaseManagedSchema(s) {
			t.Errorf("schema %q should be managed (excluded from migration)", s)
		}
	}
	for _, s := range []string{"public", "auth", "storage", "app", "billing"} {
		if isSupabaseManagedSchema(s) {
			t.Errorf("schema %q should NOT be managed (it carries user data)", s)
		}
	}
}

func TestIsSupabaseManagedRole(t *testing.T) {
	for _, r := range []string{"anon", "authenticated", "service_role", "supabase_admin", "authenticator", "postgres", "pg_read_all_data", "PG_MONITOR"} {
		if !isSupabaseManagedRole(r) {
			t.Errorf("role %q should be managed (filtered from roles pass)", r)
		}
	}
	for _, r := range []string{"app_user", "readonly", "analytics"} {
		if isSupabaseManagedRole(r) {
			t.Errorf("role %q should NOT be managed (it is a user role to migrate)", r)
		}
	}
}

func TestSupabaseMigrateSchemas(t *testing.T) {
	// Source reports a mix of core, user, and platform-internal schemas.
	all := []string{"public", "auth", "storage", "vault", "realtime", "app", "billing", "extensions", "app"}
	got := supabaseMigrateSchemas(all)
	want := []string{"public", "auth", "storage", "app", "billing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("supabaseMigrateSchemas = %v; want %v", got, want)
	}

	// Empty input yields just the core schemas.
	core := supabaseMigrateSchemas(nil)
	if strings.Join(core, ",") != "public,auth,storage" {
		t.Errorf("empty input should give core schemas, got %v", core)
	}
}

func TestSupabaseSchemaDumpArgs(t *testing.T) {
	cred := &CredEntry{Host: "db.example.supabase.co", Port: 5432, Username: "postgres", Password: "secret"}
	schemas := supabaseMigrateSchemas([]string{"app"})

	// Schema-only pass.
	prog, args, env, err := supabaseSchemaDumpArgs(cred, "postgres", schemas, false)
	if err != nil {
		t.Fatalf("schema pass: unexpected error: %v", err)
	}
	if prog != "pg_dump" {
		t.Errorf("program = %q; want pg_dump", prog)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--schema-only") {
		t.Errorf("schema pass must include --schema-only: %v", args)
	}
	if strings.Contains(joined, "--data-only") {
		t.Errorf("schema pass must NOT include --data-only: %v", args)
	}
	for _, s := range []string{"--schema=public", "--schema=auth", "--schema=storage", "--schema=app"} {
		if !strings.Contains(joined, s) {
			t.Errorf("schema pass missing %s: %v", s, args)
		}
	}
	if strings.Contains(joined, "--schema=vault") || strings.Contains(joined, "--schema=realtime") {
		t.Errorf("schema pass must never include a managed schema: %v", args)
	}
	if !strings.Contains(joined, "-Fc") {
		t.Errorf("schema pass must use custom format -Fc: %v", args)
	}
	// Password rides the environment, never argv.
	if strings.Contains(joined, "secret") {
		t.Errorf("password leaked into argv: %v", args)
	}
	if want := "PGPASSWORD=secret"; !contains(env, want) {
		t.Errorf("env must carry %q, got %v", want, env)
	}

	// Data-only pass flips the flags.
	_, dargs, _, err := supabaseSchemaDumpArgs(cred, "postgres", schemas, true)
	if err != nil {
		t.Fatalf("data pass: unexpected error: %v", err)
	}
	dj := strings.Join(dargs, " ")
	if !strings.Contains(dj, "--data-only") || strings.Contains(dj, "--schema-only") {
		t.Errorf("data pass must be --data-only and not --schema-only: %v", dargs)
	}
}

func TestSupabaseSchemaDumpArgsValidation(t *testing.T) {
	cred := &CredEntry{Username: "postgres"}
	if _, _, _, err := supabaseSchemaDumpArgs(nil, "db", []string{"public"}, false); err == nil {
		t.Error("nil cred should error")
	}
	if _, _, _, err := supabaseSchemaDumpArgs(cred, "", []string{"public"}, false); err == nil {
		t.Error("empty db should error")
	}
	if _, _, _, err := supabaseSchemaDumpArgs(cred, "db", nil, false); err == nil {
		t.Error("empty schema set should error")
	}
}

func TestSupabaseRolesDumpArgs(t *testing.T) {
	cred := &CredEntry{Host: "h", Port: 5432, Username: "postgres", Password: "pw"}
	prog, args, env, err := supabaseRolesDumpArgs(cred)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prog != "pg_dumpall" {
		t.Errorf("program = %q; want pg_dumpall", prog)
	}
	if !contains(args, "--roles-only") {
		t.Errorf("roles pass must include --roles-only: %v", args)
	}
	if !contains(env, "PGPASSWORD=pw") {
		t.Errorf("env must carry the password, got %v", env)
	}
}

func TestSupabaseFilterRolesSQL(t *testing.T) {
	in := strings.Join([]string{
		"--",
		"-- Roles",
		"--",
		"CREATE ROLE anon;",
		"ALTER ROLE anon WITH NOLOGIN NOSUPERUSER;",
		"CREATE ROLE service_role;",
		"CREATE ROLE postgres;",
		"ALTER ROLE postgres WITH SUPERUSER;",
		"CREATE ROLE app_user;",
		"ALTER ROLE app_user WITH LOGIN PASSWORD 'x';",
		"GRANT anon TO authenticated;",
		"GRANT app_user TO app_admin;",
		"",
	}, "\n")

	out := supabaseFilterRolesSQL(in)

	// Managed roles and their statements are gone.
	for _, gone := range []string{"CREATE ROLE anon", "service_role", "ALTER ROLE postgres", "GRANT anon TO authenticated"} {
		if strings.Contains(out, gone) {
			t.Errorf("filtered SQL still contains managed-role statement %q:\n%s", gone, out)
		}
	}
	// User role and its grant survive.
	for _, kept := range []string{"CREATE ROLE app_user", "ALTER ROLE app_user", "GRANT app_user TO app_admin"} {
		if !strings.Contains(out, kept) {
			t.Errorf("filtered SQL dropped user-role statement %q:\n%s", kept, out)
		}
	}
	// Comment lines pass through.
	if !strings.Contains(out, "-- Roles") {
		t.Errorf("non-role lines must pass through:\n%s", out)
	}
}

func TestSupabaseRestoreArgs(t *testing.T) {
	cred := &CredEntry{Host: "127.0.0.1", Port: 5432, Username: "postgres", Password: "pw"}

	// Schema restore: no --data-only, no --disable-triggers, owners preserved.
	prog, args, _, err := supabaseRestoreArgs(cred, "postgres", "/tmp/schema.dump", false)
	if err != nil {
		t.Fatalf("schema restore: %v", err)
	}
	if prog != "pg_restore" {
		t.Errorf("program = %q; want pg_restore", prog)
	}
	j := strings.Join(args, " ")
	if !strings.Contains(j, "--schema-only") || strings.Contains(j, "--data-only") {
		t.Errorf("schema restore flags wrong: %v", args)
	}
	if strings.Contains(j, "--no-owner") {
		t.Errorf("must preserve ownership (no --no-owner) so RLS/grants stay correct: %v", args)
	}
	if !strings.Contains(j, "/tmp/schema.dump") {
		t.Errorf("artifact path missing: %v", args)
	}

	// Data restore: --data-only + --disable-triggers.
	_, dargs, _, err := supabaseRestoreArgs(cred, "postgres", "/tmp/data.dump", true)
	if err != nil {
		t.Fatalf("data restore: %v", err)
	}
	dj := strings.Join(dargs, " ")
	if !strings.Contains(dj, "--data-only") || !strings.Contains(dj, "--disable-triggers") {
		t.Errorf("data restore must be --data-only --disable-triggers: %v", dargs)
	}

	if _, _, _, err := supabaseRestoreArgs(cred, "postgres", "", false); err == nil {
		t.Error("empty artifact path should error")
	}
}

func TestSupabaseRolesRestoreArgs(t *testing.T) {
	cred := &CredEntry{Username: "postgres", Password: "pw"}
	prog, args, env, err := supabaseRolesRestoreArgs(cred, "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if prog != "psql" {
		t.Errorf("program = %q; want psql", prog)
	}
	j := strings.Join(args, " ")
	if !strings.Contains(j, "ON_ERROR_STOP=0") {
		t.Errorf("roles restore must tolerate errors: %v", args)
	}
	if !strings.Contains(j, "--dbname=postgres") {
		t.Errorf("empty db should default to postgres: %v", args)
	}
	if !contains(env, "PGPASSWORD=pw") {
		t.Errorf("password must ride env: %v", env)
	}
}

func TestSupabaseStorageSyncArgs(t *testing.T) {
	src := SupabaseStorageDescriptor{
		Backend: "s3", S3Endpoint: "https://x.supabase.co/storage/v1/s3",
		S3Region: "ap-southeast-1", S3Bucket: "objects",
		S3AccessKey: "AK", S3SecretKey: "SK",
	}
	dst := SupabaseStorageDescriptor{Backend: "fs", FSPath: "/var/lib/storage"}

	prog, args, env, err := supabaseStorageSyncArgs(src, dst)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if prog != "rclone" {
		t.Errorf("program = %q; want rclone", prog)
	}
	j := strings.Join(args, " ")
	if !strings.HasPrefix(j, "sync ") {
		t.Errorf("must be an rclone sync: %v", args)
	}
	if !strings.Contains(j, "SRC:objects") || !strings.Contains(j, "DST:/var/lib/storage") {
		t.Errorf("remotes wrong: %v", args)
	}
	// Secret key rides env, never argv.
	if strings.Contains(j, "SK") {
		t.Errorf("secret leaked into argv: %v", args)
	}
	if !contains(env, "RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY=SK") {
		t.Errorf("env must carry the s3 secret: %v", env)
	}
	if !contains(env, "RCLONE_CONFIG_DST_TYPE=local") {
		t.Errorf("destination fs remote misconfigured: %v", env)
	}

	if _, _, _, err := supabaseStorageSyncArgs(SupabaseStorageDescriptor{Backend: "bogus"}, dst); err == nil {
		t.Error("unknown backend should error")
	}
}

func TestSupabaseVerifyQueries(t *testing.T) {
	for _, k := range []string{"tables", "rls_policies", "auth_users", "storage_objects"} {
		q, ok := SupabaseVerifyQueries[k]
		if !ok || !strings.Contains(strings.ToLower(q), "count(") {
			t.Errorf("verify query %q missing or not a count: %q", k, q)
		}
	}
	if !strings.Contains(SupabaseVerifyQueries["auth_users"], "auth.users") {
		t.Error("auth_users must count auth.users")
	}
	if !strings.Contains(SupabaseVerifyQueries["rls_policies"], "pg_policies") {
		t.Error("rls_policies must read pg_policies")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
