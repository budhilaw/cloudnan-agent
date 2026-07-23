package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsKnownServiceUnit(t *testing.T) {
	// Must MATCH: real services we ship or users bring (including a hand-built
	// WordPress stack adopted into Cloudnan), plus security units.
	follow := []string{
		"nginx.service", "apache2.service", "httpd.service", "lshttpd.service",
		"php8.3-fpm.service", "php7.4-fpm.service", "php-fpm.service",
		"mysql.service", "mariadb.service", "postgresql.service",
		"postgresql@15-main.service", "redis-server.service", "valkey.service",
		"mongod.service", "docker.service", "containerd.service",
		"supervisor.service", "cron.service", "crond.service",
		"ssh.service", "sshd.service", "fail2ban.service", "ufw.service",
	}
	for _, u := range follow {
		if !isKnownServiceUnit(u) {
			t.Errorf("expected %q to be followed (catalog match)", u)
		}
	}

	// Must NOT match: kernel/udev/dbus/systemd-internal noise. Following these
	// is exactly the failure mode the catalog exists to prevent.
	skip := []string{
		"systemd-journald.service", "systemd-udevd.service", "systemd-logind.service",
		"dbus.service", "systemd-resolved.service", "systemd-timesyncd.service",
		"polkit.service", "accounts-daemon.service", "snapd.service",
		"getty@tty1.service", "user@1000.service", "cloud-init.service",
		"NetworkManager.service", "rsyslog.service", "unattended-upgrades.service",
	}
	for _, u := range skip {
		if isKnownServiceUnit(u) {
			t.Errorf("expected %q to be SKIPPED (not a known service)", u)
		}
	}
}

func TestBaseUnitName(t *testing.T) {
	cases := map[string]string{
		"nginx.service":              "nginx",
		"php8.3-fpm.service":         "php8.3-fpm",
		"postgresql@15-main.service": "postgresql",
		"getty@tty1.service":         "getty",
		"redis-server":               "redis-server",
	}
	for in, want := range cases {
		if got := baseUnitName(in); got != want {
			t.Errorf("baseUnitName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstServerName(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"server_name example.com www.example.com;", "example.com www.example.com"}, "example.com"},
		{[]string{"server_name _;", "_"}, ""},
		{[]string{"server_name *.example.com example.com;", "*.example.com example.com"}, "example.com"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := firstServerName(c.in); got != c.want {
			t.Errorf("firstServerName(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScanNginxVhost(t *testing.T) {
	vhost := `
server {
    listen 80;
    server_name blog.example.com www.blog.example.com;
    root /var/www/blog/public;
    access_log /var/log/nginx/blog.access.log;
    error_log /var/log/nginx/blog.error.log warn;
}`
	site, files := scanNginxVhost(vhost)
	if site != "blog.example.com" {
		t.Fatalf("site = %q, want blog.example.com", site)
	}
	got := map[string]bool{} // path -> isError
	for _, f := range files {
		got[f.path] = f.isError
	}
	if isErr, ok := got["/var/log/nginx/blog.access.log"]; !ok || isErr {
		t.Errorf("access log missing or wrongly marked error: %v", got)
	}
	if isErr, ok := got["/var/log/nginx/blog.error.log"]; !ok || !isErr {
		t.Errorf("error log missing or not marked error: %v", got)
	}
}

func TestScanNginxVhost_SyslogAndOffSurfacedForDiscoveryToReject(t *testing.T) {
	// scanNginxVhost returns raw directive values (directives are line-anchored,
	// as in real vhost files); the syslog:/off/relative filtering happens in
	// discoverAppLogTargets.add. Confirm the scanner surfaces them verbatim.
	vhost := `
server {
    server_name x.test;
    access_log off;
    error_log syslog:server=unix:/dev/log;
}`
	_, files := scanNginxVhost(vhost)
	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, f.path)
	}
	if len(got) != 2 || got[0] != "off" || got[1] != "syslog:server=unix:/dev/log" {
		t.Fatalf("raw directive values = %v, want [off syslog:server=unix:/dev/log]", got)
	}
}

func TestScanApacheVhost(t *testing.T) {
	vhost := `
<VirtualHost *:80>
    ServerName shop.example.com
    DocumentRoot /var/www/shop
    CustomLog /var/log/apache2/shop.access.log combined
    ErrorLog /var/log/apache2/shop.error.log
</VirtualHost>`
	site, files := scanApacheVhost(vhost)
	if site != "shop.example.com" {
		t.Fatalf("site = %q, want shop.example.com", site)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.path] = f.isError
	}
	if isErr, ok := got["/var/log/apache2/shop.access.log"]; !ok || isErr {
		t.Errorf("apache access log wrong: %v", got)
	}
	if isErr, ok := got["/var/log/apache2/shop.error.log"]; !ok || !isErr {
		t.Errorf("apache error log wrong: %v", got)
	}
}

func TestFrameworkLogs(t *testing.T) {
	root := t.TempDir()
	// WordPress debug log directly under docroot/wp-content.
	wpDir := filepath.Join(root, "wp-content")
	if err := os.MkdirAll(wpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wpLog := filepath.Join(wpDir, "debug.log")
	if err := os.WriteFile(wpLog, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Laravel logs one level ABOVE docroot (docroot is the public/ dir).
	appRoot := t.TempDir()
	public := filepath.Join(appRoot, "public")
	storageLogs := filepath.Join(appRoot, "storage", "logs")
	if err := os.MkdirAll(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storageLogs, 0o755); err != nil {
		t.Fatal(err)
	}
	laravelLog := filepath.Join(storageLogs, "laravel.log")
	if err := os.WriteFile(laravelLog, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	wp := frameworkLogs(root)
	if len(wp) != 1 || wp[0].path != wpLog || !wp[0].isError {
		t.Errorf("WordPress detection = %+v, want %s (error)", wp, wpLog)
	}
	lv := frameworkLogs(public)
	found := false
	for _, f := range lv {
		if f.path == laravelLog {
			found = true
		}
	}
	if !found {
		t.Errorf("Laravel detection = %+v, want to include %s", lv, laravelLog)
	}

	// A docroot with no framework logs yields nothing.
	if got := frameworkLogs(t.TempDir()); len(got) != 0 {
		t.Errorf("empty docroot = %+v, want none", got)
	}
	// A relative/empty docroot is rejected.
	if got := frameworkLogs("relative/path"); got != nil {
		t.Errorf("relative docroot should be nil, got %+v", got)
	}
}
