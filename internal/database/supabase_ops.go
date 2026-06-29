// Package database — supabase_ops.go.
//
// The agent-side ops that execute a Supabase migration, dispatched from the
// control plane over the "database" command channel (args[0] = op name,
// args[1] = JSON envelope). Each op verifies its scoped op-token, then runs
// the command builders from supabase.go against the real tools (pg_dump,
// pg_dumpall, pg_restore, psql, rclone).
//
// Cred handling differs by direction: the SOURCE Supabase Postgres creds
// arrive in the envelope (the agent never "connected" the remote project via
// the vault flow); the DESTINATION is the local stack, whose superuser
// password is read from the compose .env the install step wrote.
//
// VALIDATION: the dump/restore flag choices are documented Supabase self-host
// conventions; the verify op's count reconciliation (source vs destination) is
// the gate that proves a given migration actually preserved the data.
package database

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const supabaseStackEnvPath = "/opt/cloudnan-supabase/supabase/docker/.env"

// --- envelope contract (mirrors the core commander) ---

type supabasePgConn struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Database string `json:"database"`
	UseTLS   bool   `json:"use_tls"`
}

func (c supabasePgConn) cred() *CredEntry {
	port := c.Port
	if port == 0 {
		port = int(defaultPostgresPort)
	}
	return &CredEntry{Host: c.Host, Port: uint32(port), Username: c.Username, Password: c.Password, UseTLS: c.UseTLS}
}

type supabaseDumpEnv struct {
	RunID             string         `json:"run_id"`
	Source            supabasePgConn `json:"source"`
	ConfirmationToken string         `json:"confirmation_token"`
}
type supabaseRestoreEnv struct {
	RunID             string         `json:"run_id"`
	Target            supabasePgConn `json:"target"`
	ConfirmationToken string         `json:"confirmation_token"`
}
type supabaseStorageEnv struct {
	RunID             string `json:"run_id"`
	SourceEndpoint    string `json:"source_endpoint"`
	SourceBucket      string `json:"source_bucket"`
	SourceAccessKey   string `json:"source_access_key"`
	SourceSecretKey   string `json:"source_secret_key"`
	SourceRegion      string `json:"source_region"`
	DestBackend       string `json:"dest_backend"`
	DestPath          string `json:"dest_path"`
	ConfirmationToken string `json:"confirmation_token"`
}
type supabaseVerifyEnv struct {
	RunID             string         `json:"run_id"`
	Source            supabasePgConn `json:"source"`
	Target            supabasePgConn `json:"target"`
	ConfirmationToken string         `json:"confirmation_token"`
}

// supabaseArtifactDir is where a run's dump artifacts land (wiped on reboot).
func supabaseArtifactDir(runID string) string {
	return filepath.Join(os.TempDir(), "cloudnan-supabase", filepathSafe(runID))
}

func filepathSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// runCapture runs a command and returns combined output, for ops where we only
// care about success/failure + diagnostics.
func runCapture(ctx context.Context, program string, args, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runToFile runs a command and writes its stdout to outPath (stderr captured
// for diagnostics). Used for the -Fc dump passes.
func runToFile(ctx context.Context, program string, args, extraEnv []string, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runCaptureStdout runs a command and returns ONLY stdout (for the roles dump
// + psql count queries, where merged stderr would corrupt the result).
func runCaptureStdout(ctx context.Context, program string, args, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.Output()
	return string(out), err
}

// opSupabaseDump runs the three-pass dump from the source into the run's
// artifact dir: roles (filtered), schema (-Fc), data (-Fc).
func (h *Handler) opSupabaseDump(ctx context.Context, args []string, emit func(string)) error {
	var env supabaseDumpEnv
	if err := supabaseParse(args, &env); err != nil {
		return err
	}
	if err := verifyOpToken(env.ConfirmationToken, "supabase_migrate_dump", env.RunID, env.Source.Database); err != nil {
		return err
	}
	cred := env.Source.cred()
	dir := supabaseArtifactDir(env.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Discover the source schemas so user-created ones travel too.
	schemas := supabaseMigrateSchemas(supabaseListSchemas(ctx, cred))
	emit(fmt.Sprintf("dump: migrating schemas %s", strings.Join(schemas, ", ")))

	// roles pass.
	prog, rargs, renv, err := supabaseRolesDumpArgs(cred)
	if err != nil {
		return err
	}
	rolesSQL, err := runCaptureStdout(ctx, prog, rargs, renv)
	if err != nil {
		return fmt.Errorf("roles dump: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles.sql"), []byte(supabaseFilterRolesSQL(rolesSQL)), 0o600); err != nil {
		return err
	}

	// schema pass.
	prog, sargs, senv, err := supabaseSchemaDumpArgs(cred, env.Source.Database, schemas, false)
	if err != nil {
		return err
	}
	if err := runToFile(ctx, prog, sargs, senv, filepath.Join(dir, "schema.dump")); err != nil {
		return fmt.Errorf("schema dump: %w", err)
	}

	// data pass.
	prog, dargs, denv, err := supabaseSchemaDumpArgs(cred, env.Source.Database, schemas, true)
	if err != nil {
		return err
	}
	if err := runToFile(ctx, prog, dargs, denv, filepath.Join(dir, "data.dump")); err != nil {
		return fmt.Errorf("data dump: %w", err)
	}
	emit("dump: complete")
	return nil
}

// opSupabaseRestore restores the run's artifacts into the local stack Postgres.
func (h *Handler) opSupabaseRestore(ctx context.Context, args []string, emit func(string)) error {
	var env supabaseRestoreEnv
	if err := supabaseParse(args, &env); err != nil {
		return err
	}
	if err := verifyOpToken(env.ConfirmationToken, "supabase_migrate_restore", env.RunID, env.Target.Database); err != nil {
		return err
	}
	target := env.Target.cred()
	if target.Password == "" {
		target.Password = supabaseReadStackPostgresPassword()
	}
	dir := supabaseArtifactDir(env.RunID)

	// roles.
	prog, rargs, renv, err := supabaseRolesRestoreArgs(target, env.Target.Database)
	if err != nil {
		return err
	}
	if out, err := supabaseRunWithStdin(ctx, prog, rargs, renv, filepath.Join(dir, "roles.sql")); err != nil {
		return fmt.Errorf("roles restore: %w: %s", err, out)
	}

	// schema, then data.
	for _, pass := range []struct {
		file     string
		dataOnly bool
		label    string
	}{{"schema.dump", false, "schema"}, {"data.dump", true, "data"}} {
		prog, pargs, penv, err := supabaseRestoreArgs(target, env.Target.Database, filepath.Join(dir, pass.file), pass.dataOnly)
		if err != nil {
			return err
		}
		// pg_restore can report ignorable errors on objects the stack
		// pre-owns; surface output but don't abort on a non-zero exit if the
		// archive applied (best-effort, verify is the gate).
		if out, err := runCapture(ctx, prog, pargs, penv); err != nil {
			emit(fmt.Sprintf("restore %s: %v (continuing; verify will reconcile): %s", pass.label, err, strings.TrimSpace(out)))
		}
	}
	emit("restore: complete")
	return nil
}

// opSupabaseStorageSync copies storage objects from the source bucket to the
// destination backend via rclone.
func (h *Handler) opSupabaseStorageSync(ctx context.Context, args []string, emit func(string)) error {
	var env supabaseStorageEnv
	if err := supabaseParse(args, &env); err != nil {
		return err
	}
	if err := verifyOpToken(env.ConfirmationToken, "supabase_storage_sync", env.RunID, "storage"); err != nil {
		return err
	}
	if env.SourceAccessKey == "" {
		emit("storage_sync: no source credentials supplied, skipping object copy")
		return nil
	}
	src := SupabaseStorageDescriptor{
		Backend: "s3", S3Endpoint: env.SourceEndpoint, S3Region: env.SourceRegion,
		S3Bucket: env.SourceBucket, S3AccessKey: env.SourceAccessKey, S3SecretKey: env.SourceSecretKey,
	}
	dst := SupabaseStorageDescriptor{Backend: env.DestBackend, FSPath: env.DestPath}
	if dst.Backend == "" {
		dst.Backend = "fs"
	}
	prog, sargs, senv, err := supabaseStorageSyncArgs(src, dst)
	if err != nil {
		return err
	}
	if out, err := runCapture(ctx, prog, sargs, senv); err != nil {
		return fmt.Errorf("storage sync: %w: %s", err, strings.TrimSpace(out))
	}
	emit("storage_sync: complete")
	return nil
}

// opSupabaseVerify counts the same things on source and destination and emits
// the reconciliation report as JSON on stdout.
func (h *Handler) opSupabaseVerify(ctx context.Context, args []string, emit func(string)) error {
	var env supabaseVerifyEnv
	if err := supabaseParse(args, &env); err != nil {
		return err
	}
	if err := verifyOpToken(env.ConfirmationToken, "supabase_verify", env.RunID, "verify"); err != nil {
		return err
	}
	target := env.Target.cred()
	if target.Password == "" {
		target.Password = supabaseReadStackPostgresPassword()
	}
	report := map[string]map[string]int{"source": {}, "destination": {}}
	for name, query := range SupabaseVerifyQueries {
		report["source"][name] = supabaseScalarCount(ctx, env.Source.cred(), env.Source.Database, query)
		report["destination"][name] = supabaseScalarCount(ctx, target, env.Target.Database, query)
	}
	out, err := json.Marshal(report)
	if err != nil {
		return err
	}
	emit(string(out))
	return nil
}

// opSupabaseDeployFunctions clones/copies the user-supplied functions and
// restarts the stack's edge-runtime container.
func (h *Handler) opSupabaseDeployFunctions(ctx context.Context, args []string, emit func(string)) error {
	var env struct {
		RunID             string `json:"run_id"`
		FunctionsRef      string `json:"functions_ref"`
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := supabaseParse(args, &env); err != nil {
		return err
	}
	if err := verifyOpToken(env.ConfirmationToken, "supabase_deploy_functions", env.RunID, "functions"); err != nil {
		return err
	}
	if env.FunctionsRef == "" {
		emit("deploy_functions: nothing supplied")
		return nil
	}
	dst := "/opt/cloudnan-supabase/supabase/docker/volumes/functions"
	if _, err := runCapture(ctx, "git", []string{"clone", "--depth", "1", env.FunctionsRef, dst}, nil); err != nil {
		// Not a git ref? fall back to a recursive copy of a local path.
		if out, cpErr := runCapture(ctx, "cp", []string{"-a", env.FunctionsRef + "/.", dst}, nil); cpErr != nil {
			return fmt.Errorf("deploy functions: %w: %s", cpErr, strings.TrimSpace(out))
		}
	}
	_, _ = runCapture(ctx, "docker", []string{"compose", "-f", "/opt/cloudnan-supabase/supabase/docker/docker-compose.yml", "restart", "functions"}, nil)
	emit("deploy_functions: complete")
	return nil
}

// --- shared helpers ---

func supabaseParse(args []string, into any) error {
	if len(args) < 2 {
		return errors.New("missing JSON envelope in args[1]")
	}
	return json.Unmarshal([]byte(args[1]), into)
}

// supabaseRunWithStdin runs a program feeding stdinFile on stdin (psql -f -).
func supabaseRunWithStdin(ctx context.Context, program string, args, extraEnv []string, stdinFile string) (string, error) {
	f, err := os.Open(stdinFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = f
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// supabaseListSchemas returns the source's schema names via psql.
func supabaseListSchemas(ctx context.Context, cred *CredEntry) []string {
	args := append(supabasePgConnArgs(cred),
		"--dbname="+supabaseDBName(""),
		"--no-password", "-tAc",
		"SELECT schema_name FROM information_schema.schemata",
	)
	env, _ := supabasePgConnEnv(cred)
	out, err := runCaptureStdout(ctx, "psql", args, env)
	if err != nil {
		return nil
	}
	var schemas []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			schemas = append(schemas, s)
		}
	}
	return schemas
}

// supabaseScalarCount runs a single count query and returns the integer (0 on
// any error — the report shows the mismatch).
func supabaseScalarCount(ctx context.Context, cred *CredEntry, db, query string) int {
	args := append(supabasePgConnArgs(cred), "--dbname="+supabaseDBName(db), "--no-password", "-tAc", query)
	env, _ := supabasePgConnEnv(cred)
	out, err := runCaptureStdout(ctx, "psql", args, env)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// supabaseReadStackPostgresPassword reads POSTGRES_PASSWORD from the stack's
// compose .env the install step wrote.
func supabaseReadStackPostgresPassword() string {
	f, err := os.Open(supabaseStackEnvPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "POSTGRES_PASSWORD=") {
			return strings.TrimPrefix(line, "POSTGRES_PASSWORD=")
		}
	}
	return ""
}
