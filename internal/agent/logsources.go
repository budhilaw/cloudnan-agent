package agent

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// This file decides WHICH systemd units and WHICH application logs the shipper
// follows. The guiding rule: never enumerate the whole journal (it is mostly
// kernel/udev/dbus/systemd-internal noise) and never key selection on "what
// Cloudnan installed" (that misses servers a user built by hand before adopting
// Cloudnan). Instead we match against a catalog of real, known services that
// happen to be running, and we discover per-site app logs from the web server's
// own vhost config — both of which are identical whether we or the user set the
// box up.

const (
	sourceDiscoveryTimeout = 8 * time.Second
	// Re-discovery cadence: new sites and newly-installed services should start
	// shipping without an agent restart. Deliberately slow — units/vhosts change
	// far less often than containers start/stop, and each pass spawns systemctl
	// (list-units + show), so a tight interval is wasted CPU on small VMs.
	sourceRediscoverInterval = 120 * time.Second
)

// knownServiceUnit matches a systemd unit NAME against the catalog. A unit is
// followed only if one of these matches AND the unit is actually loaded on the
// box. Patterns are matched on the unit's base name (the part before ".service"
// / instance "@" suffix) so instanced units (php8.3-fpm, postgresql@15-main)
// are caught. Ordered roughly web → runtime → datastore → infra → security,
// covering the "managed services + security" scope.
var knownServiceUnitPatterns = []*regexp.Regexp{
	// Web servers
	regexp.MustCompile(`^nginx$`),
	regexp.MustCompile(`^(apache2|httpd)$`),
	regexp.MustCompile(`^(lshttpd|lsws|openlitespeed)$`),
	regexp.MustCompile(`^caddy$`),
	// PHP-FPM (versioned + plain)
	regexp.MustCompile(`^php[0-9.]*-?fpm$`),
	// App runtimes commonly run as units
	regexp.MustCompile(`^(gunicorn|uwsgi|puma|unicorn)$`),
	// Datastores
	regexp.MustCompile(`^(mysql|mariadb)$`),
	regexp.MustCompile(`^postgresql`),
	regexp.MustCompile(`^(mongod|mongodb)$`),
	regexp.MustCompile(`^(redis-server|redis|valkey|keydb)$`),
	regexp.MustCompile(`^(memcached)$`),
	regexp.MustCompile(`^(rabbitmq-server|rabbitmq)$`),
	// Container + process infra
	regexp.MustCompile(`^(docker|containerd)$`),
	regexp.MustCompile(`^(supervisor|supervisord)$`),
	regexp.MustCompile(`^(cron|crond)$`),
	// Security-relevant units (this scope opts these in)
	regexp.MustCompile(`^(ssh|sshd)$`),
	regexp.MustCompile(`^fail2ban$`),
	regexp.MustCompile(`^ufw$`),
}

// baseUnitName strips ".service" and any "@instance" suffix so instanced /
// templated units match the catalog by their family name.
func baseUnitName(unit string) string {
	u := strings.TrimSuffix(unit, ".service")
	if at := strings.IndexByte(u, '@'); at >= 0 {
		u = u[:at]
	}
	return u
}

func isKnownServiceUnit(unit string) bool {
	base := baseUnitName(unit)
	for _, re := range knownServiceUnitPatterns {
		if re.MatchString(base) {
			return true
		}
	}
	return false
}

// discoverSystemdUnits returns the loaded service units worth following: the
// catalog-matched set on the box, unioned with any operator-configured extra
// units. Best-effort; a missing/failing systemctl yields just the configured
// extras so manual config still works on non-systemd hosts.
func discoverSystemdUnits(ctx context.Context, extra []string) []string {
	set := make(map[string]struct{}, len(extra)+8)
	for _, u := range extra {
		if u = strings.TrimSpace(u); u != "" {
			set[u] = struct{}{}
		}
	}
	for _, unit := range listLoadedServiceUnits(ctx) {
		if isKnownServiceUnit(unit) {
			set[unit] = struct{}{}
		}
	}
	units := make([]string, 0, len(set))
	for u := range set {
		units = append(units, u)
	}
	sort.Strings(units)
	return units
}

// listLoadedServiceUnits returns every loaded service unit name on the box.
// Best-effort: empty on a non-systemd host or a failing systemctl.
func listLoadedServiceUnits(ctx context.Context) []string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, sourceDiscoveryTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "systemctl",
		"list-units", "--type=service", "--no-legend", "--state=loaded", "--plain")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var units []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(strings.TrimSpace(sc.Text()))
		if len(fields) == 0 {
			continue
		}
		units = append(units, fields[0])
	}
	return units
}

// appUnitPathPrefixes are the filesystem locations a customer's own application
// binary lives in. An ExecStart under one of these (combined with a locally
// authored unit file) marks a service as a user app rather than an OS daemon.
var appUnitPathPrefixes = []string{
	"/home/", "/opt/", "/srv/", "/usr/local/", "/var/www/", "/app/", "/root/",
}

// localUnitFragmentDir is where admin/locally-authored (and Cloudnan-deployed)
// unit files land. Vendor OS daemons live under /lib or /usr/lib instead.
const localUnitFragmentDir = "/etc/systemd/system/"

// agentSystemdUnit is the agent's own unit (installed by scripts/install.sh at
// /etc/systemd/system/cloudnan-agent.service with ExecStart in /usr/local/bin).
// It matches isAppUnit, so it must be excluded from App discovery — its lines
// already ship under the dedicated "agent" source.
const agentSystemdUnit = "cloudnan-agent.service"

// isAppUnit classifies a service as a customer application from two filesystem
// facts, independent of the app's language: the unit file is locally authored
// (FragmentPath under /etc/systemd/system) AND its ExecStart binary lives in an
// app path (/opt, /home, /srv, ...). This catches a Go/Node/Python/Rust binary
// run as its own service, which the known-service catalog and the vhost scanner
// both miss.
func isAppUnit(fragmentPath, execStartPath string) bool {
	if !strings.HasPrefix(fragmentPath, localUnitFragmentDir) {
		return false
	}
	for _, p := range appUnitPathPrefixes {
		if strings.HasPrefix(execStartPath, p) {
			return true
		}
	}
	return false
}

// discoverAppUnits returns loaded service units that are customer applications
// (see isAppUnit) and therefore ship under the App source. The known-service
// catalog units and the agent's own unit are excluded so a unit is never
// double-followed or mis-bucketed. Best-effort and read-only.
func discoverAppUnits(ctx context.Context, selfUnit string) []string {
	loaded := listLoadedServiceUnits(ctx)
	candidates := make([]string, 0, len(loaded))
	for _, u := range loaded {
		if u == selfUnit || isKnownServiceUnit(u) {
			continue
		}
		candidates = append(candidates, u)
	}
	if len(candidates) == 0 {
		return nil
	}
	props := showUnitProps(ctx, candidates)
	var apps []string
	for _, unit := range candidates {
		p := props[unit]
		if p.fragmentPath == "" {
			continue
		}
		if isAppUnit(p.fragmentPath, p.execStartPath) {
			apps = append(apps, unit)
		}
	}
	sort.Strings(apps)
	return apps
}

type unitProps struct {
	fragmentPath  string
	execStartPath string
}

// showUnitProps batches `systemctl show` for the given units and parses the
// FragmentPath + ExecStart binary path of each. One block per unit, keyed by
// the unit's Id.
func showUnitProps(ctx context.Context, units []string) map[string]unitProps {
	out := make(map[string]unitProps, len(units))
	if _, err := exec.LookPath("systemctl"); err != nil {
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, sourceDiscoveryTimeout)
	defer cancel()
	args := append([]string{"show", "--no-pager",
		"--property=Id", "--property=FragmentPath", "--property=ExecStart"}, units...)
	data, err := exec.CommandContext(cctx, "systemctl", args...).Output()
	if err != nil {
		return out
	}
	var id string
	var p unitProps
	flush := func() {
		if id != "" {
			out[id] = p
		}
		id, p = "", unitProps{}
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // blank line separates unit blocks
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "Id="):
			id = strings.TrimPrefix(line, "Id=")
		case strings.HasPrefix(line, "FragmentPath="):
			p.fragmentPath = strings.TrimPrefix(line, "FragmentPath=")
		case strings.HasPrefix(line, "ExecStart=") && p.execStartPath == "":
			p.execStartPath = parseExecStartPath(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	flush()
	return out
}

// parseExecStartPath pulls the binary path out of systemctl's ExecStart value,
// which looks like `{ path=/opt/app/bin/server ; argv[]=... ; ... }`. Falls back
// to the first whitespace-delimited token for the rare plain form.
func parseExecStartPath(v string) string {
	if i := strings.Index(v, "path="); i >= 0 {
		rest := v[i+len("path="):]
		if j := strings.IndexAny(rest, " ;"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	if fields := strings.Fields(v); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// appLogFile is one log file to tail for a site, and whether it is an error
// stream (so buildLogEntry starts it at WARN rather than INFO).
type appLogFile struct {
	path    string
	isError bool
}

// appLogTarget groups a site's application log files under its domain, which
// becomes the `app:<site>` source tag.
type appLogTarget struct {
	site  string
	files []appLogFile
}

var (
	// nginx directives: `access_log /path [params];`, `error_log /path [level];`,
	// `server_name a b c;`, `root /var/www/site;`. Captured up to the first
	// whitespace/semicolon after the path.
	reNginxAccessLog = regexp.MustCompile(`(?m)^\s*access_log\s+([^;\s]+)`)
	reNginxErrorLog  = regexp.MustCompile(`(?m)^\s*error_log\s+([^;\s]+)`)
	reNginxServerNm  = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	reNginxRoot      = regexp.MustCompile(`(?m)^\s*root\s+(\S+)\s*;`)
	// apache: CustomLog/ErrorLog/ServerName/DocumentRoot.
	reApacheCustom = regexp.MustCompile(`(?mi)^\s*CustomLog\s+"?(\S+?)"?(\s|$)`)
	reApacheError  = regexp.MustCompile(`(?mi)^\s*ErrorLog\s+"?(\S+?)"?(\s|$)`)
	reApacheServer = regexp.MustCompile(`(?mi)^\s*ServerName\s+(\S+)`)
	reApacheDocRt  = regexp.MustCompile(`(?mi)^\s*DocumentRoot\s+"?(\S+?)"?(\s|$)`)
)

// vhost config directories scanned for per-site log paths. sites-enabled first
// (Debian/Ubuntu), then conf.d (RHEL/custom).
var (
	nginxVhostGlobs  = []string{"/etc/nginx/sites-enabled/*", "/etc/nginx/conf.d/*.conf"}
	apacheVhostGlobs = []string{"/etc/apache2/sites-enabled/*", "/etc/httpd/conf.d/*.conf", "/etc/apache2/conf.d/*"}
)

// discoverAppLogTargets reads the web-server vhosts to find each site's log
// files, then adds framework-convention logs under each docroot. Returns one
// target per resolved site domain. Best-effort and read-only.
func discoverAppLogTargets() []appLogTarget {
	byName := make(map[string]map[string]bool) // site -> path -> isError

	add := func(site, path string, isError bool) {
		path = strings.Trim(path, `"'`)
		if site == "" || path == "" || path == "off" || !filepath.IsAbs(path) {
			return
		}
		// Skip nginx's syslog/stderr pseudo-targets and non-file destinations.
		if strings.HasPrefix(path, "syslog:") || strings.HasPrefix(path, "stderr") {
			return
		}
		if byName[site] == nil {
			byName[site] = make(map[string]bool)
		}
		// A path used as both access and error stays error=false (access wins
		// only if it was already recorded non-error); prefer the error flag when
		// any source marks it so.
		byName[site][path] = byName[site][path] || isError
	}

	for _, glob := range nginxVhostGlobs {
		for _, text := range readGlob(glob) {
			site, files := scanNginxVhost(text)
			for _, f := range files {
				add(site, f.path, f.isError)
			}
		}
	}
	for _, glob := range apacheVhostGlobs {
		for _, text := range readGlob(glob) {
			site, files := scanApacheVhost(text)
			for _, f := range files {
				add(site, f.path, f.isError)
			}
		}
	}

	targets := make([]appLogTarget, 0, len(byName))
	for site, paths := range byName {
		files := make([]appLogFile, 0, len(paths))
		for p, isErr := range paths {
			files = append(files, appLogFile{path: p, isError: isErr})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
		targets = append(targets, appLogTarget{site: site, files: files})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].site < targets[j].site })
	return targets
}

// scanNginxVhost extracts the site tag and log files from one nginx vhost's
// text: its access_log/error_log paths plus framework-convention logs under
// the `root` docroot. Pure (no I/O beyond the docroot existence check in
// frameworkLogs) so it is directly testable.
func scanNginxVhost(text string) (site string, files []appLogFile) {
	site = firstServerName(reNginxServerNm.FindStringSubmatch(text))
	for _, m := range reNginxAccessLog.FindAllStringSubmatch(text, -1) {
		files = append(files, appLogFile{path: m[1], isError: false})
	}
	for _, m := range reNginxErrorLog.FindAllStringSubmatch(text, -1) {
		files = append(files, appLogFile{path: m[1], isError: true})
	}
	if root := reNginxRoot.FindStringSubmatch(text); root != nil {
		files = append(files, frameworkLogs(root[1])...)
	}
	return site, files
}

// scanApacheVhost is scanNginxVhost's Apache counterpart (CustomLog/ErrorLog/
// ServerName/DocumentRoot).
func scanApacheVhost(text string) (site string, files []appLogFile) {
	if m := reApacheServer.FindStringSubmatch(text); m != nil {
		site = m[1]
	}
	for _, m := range reApacheCustom.FindAllStringSubmatch(text, -1) {
		files = append(files, appLogFile{path: m[1], isError: false})
	}
	for _, m := range reApacheError.FindAllStringSubmatch(text, -1) {
		files = append(files, appLogFile{path: m[1], isError: true})
	}
	if m := reApacheDocRt.FindStringSubmatch(text); m != nil {
		files = append(files, frameworkLogs(m[1])...)
	}
	return site, files
}

// firstServerName returns the first non-wildcard token of a server_name
// directive (`server_name example.com www.example.com;`), which becomes the
// site tag. `_`/`*` catch-alls are skipped in favour of a real name.
func firstServerName(m []string) string {
	if m == nil {
		return ""
	}
	for _, name := range strings.Fields(m[1]) {
		name = strings.TrimSpace(name)
		if name == "" || name == "_" || strings.HasPrefix(name, "*") {
			continue
		}
		return name
	}
	return ""
}

// frameworkLogs returns convention-based application log files under a docroot
// that exist on disk. Covers the stacks Cloudnan deploys and the ones users
// most commonly bring: WordPress (wp-content/debug.log) and Laravel
// (storage/logs/*.log). The docroot may be the public/ dir, so Laravel's
// storage is checked one level up too.
func frameworkLogs(docroot string) []appLogFile {
	docroot = strings.TrimRight(strings.Trim(docroot, `"'`), "/")
	if docroot == "" || !filepath.IsAbs(docroot) {
		return nil
	}
	var out []appLogFile
	// WordPress WP_DEBUG_LOG.
	if p := filepath.Join(docroot, "wp-content", "debug.log"); fileExists(p) {
		out = append(out, appLogFile{path: p, isError: true})
	}
	// Laravel: docroot is typically <app>/public, logs live in <app>/storage/logs.
	for _, base := range []string{docroot, filepath.Dir(docroot)} {
		dir := filepath.Join(base, "storage", "logs")
		for _, p := range readGlobPaths(filepath.Join(dir, "*.log")) {
			out = append(out, appLogFile{path: p, isError: true})
		}
	}
	return out
}

// readGlob expands a glob and returns the text of each readable regular file.
// Vhost files are small and root-owned; a cap guards against a pathological
// include. Symlinks (sites-enabled entries) are followed by os.ReadFile.
func readGlob(glob string) []string {
	var texts []string
	for _, path := range readGlobPaths(glob) {
		if b, err := os.ReadFile(path); err == nil && len(b) <= 512*1024 {
			texts = append(texts, string(b))
		}
	}
	return texts
}

func readGlobPaths(glob string) []string {
	matches, err := filepath.Glob(glob)
	if err != nil {
		return nil
	}
	out := matches[:0]
	for _, p := range matches {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
