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

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
