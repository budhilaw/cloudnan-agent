package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// hardBlockPatterns are regular expressions that match catastrophically
// destructive shell idioms regardless of how the command was assembled.
// Even if the AI generates one, even if the operator typed one, even
// if the control plane (somehow) wraps one — the agent refuses. These
// are NOT user-tunable: the regexes intentionally live in source so a
// misconfigured config.yaml can't accidentally re-enable them.
//
// Each pattern is matched case-insensitively against the full assembled
// command line. False positives are fine — the right answer to "what
// if I really need to run mkfs" is "use a dedicated tool with typed
// schemas," not "loosen the agent guard."
var hardBlockPatterns = []*regexp.Regexp{
	// Filesystem nukes — any rm of /, /etc, /usr, /bin, /sbin, /lib,
	// /var, /home (top-level). We deliberately accept rm of
	// subpaths under those, e.g. /var/log/old.log is fine. The lazy
	// fillers ([^|;&\n]*?) tolerate flag reordering and the
	// --no-preserve-root option appearing in any position.
	regexp.MustCompile(`(?i)\brm\b[^|;&\n]*?-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*[^|;&\n]*?\s/(?:\s|$|['"])`),
	regexp.MustCompile(`(?i)\brm\b[^|;&\n]*?-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*[^|;&\n]*?\s/(?:\s|$|['"])`),
	// Block rm of a top-level system dir itself (with or without
	// trailing slash). Subpath rm under those dirs is allowed —
	// `rm -f /usr/local/bin/node` (the App Installer's uninstall
	// command for mise-managed binaries) and `rm -rf /var/log/old.
	// log` are routine operations. The `/?(\s|$|['"])` clause
	// matches exactly "/usr" or "/usr/" followed by end-of-token,
	// not a deeper path like "/usr/local/...".
	regexp.MustCompile(`(?i)\brm\b[^|;&\n]*?-[rRfFa]+[^|;&\n]*?\s/(?:etc|usr|bin|sbin|lib|lib64|boot|var|home|root)/?(?:\s|$|['"])`),
	// dd to a raw block device (nukes the disk). We let dd to files
	// pass — that's a legitimate fallocate alternative.
	regexp.MustCompile(`(?i)\bdd\b[^|;&\n]*\bof=/dev/(?:sd[a-z]+|nvme|vd[a-z]+|xvd[a-z]+|disk\d+)`),
	// mkfs against a real disk (not a loop device or image file).
	regexp.MustCompile(`(?i)\bmkfs(?:\.[a-z0-9]+)?\s+/dev/(?:sd[a-z]+|nvme|vd[a-z]+|xvd[a-z]+|disk\d+)`),
	regexp.MustCompile(`(?i)\bwipefs\s+(-[a-z]*\s+)*/dev/`),
	regexp.MustCompile(`(?i)\bshred\s+(-[a-z]*\s+)*/dev/`),
	// Fork bomb (classic + variants).
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`),
	// > /dev/sda style direct writes to disk devices.
	regexp.MustCompile(`>\s*/dev/(?:sd[a-z]+|nvme|vd[a-z]+|xvd[a-z]+|disk\d+)`),
	// chmod 777 / 000 on the entire root or critical system dirs.
	regexp.MustCompile(`(?i)\bchmod\s+(-R\s+)?(?:000|777)\s+/(?:\s|$|etc|usr|bin|sbin|lib|lib64|boot|var)`),
	// chown changing ownership of /etc, /usr, /boot — almost always wrong.
	regexp.MustCompile(`(?i)\bchown\s+(-R\s+)?[a-zA-Z0-9._:-]+\s+/(?:etc|usr|boot|sbin|bin|lib|lib64)\s*(\s|$)`),
	// Disabling sudo, deleting /etc/sudoers, deleting authorized_keys.
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*/etc/sudoers(\.d)?\b`),
	regexp.MustCompile(`(?i)>\s*/etc/sudoers\b`),
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*[^\s]*/\.ssh/authorized_keys\b`),
	// Halting the agent's own host.
	regexp.MustCompile(`(?i)\b(?:shutdown|poweroff|halt|reboot|init\s+0|init\s+6|telinit\s+0|telinit\s+6)\b`),
	// Disabling/removing the cloudnan agent itself.
	regexp.MustCompile(`(?i)\bsystemctl\s+(?:disable|stop|mask)\s+cloudnan-agent\b`),
	regexp.MustCompile(`(?i)\b(?:apt(?:-get)?|yum|dnf|zypper)\s+(?:remove|purge|erase)\s+(?:[^&;|]*\s+)?cloudnan-agent\b`),
	// iptables -F flush (would drop all firewall rules) — UFW path is
	// preferred and our tools wrap it; raw iptables -F is almost
	// always a sign of a confused agent.
	regexp.MustCompile(`(?i)\biptables\s+(-[A-Za-z]*\s+)*-F\b`),
}

// curlPipeShellPattern blocks piping a downloaded script straight into a
// shell (`curl … | bash`). It is kept SEPARATE from hardBlockPatterns
// because it is the one rule the trusted App Installer path relaxes:
// catalog installers (docker, nodesource, redpanda, …) legitimately use
// this idiom, whereas the hardBlockPatterns above (filesystem nukes,
// mkfs/dd on disks, fork bombs, shutdown, disabling cloudnan-agent) must
// hold even for first-party installs. Enforced for every user/LLM command
// via isBlocked; skipped only by ExecuteInstall.
var curlPipeShellPattern = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^|;&\n]*\|\s*(?:sudo\s+)?(?:bash|sh|zsh|ksh)\b`)

// Executor handles command execution on the agent
type Executor struct {
	shell           string
	defaultTimeout  time.Duration
	allowedCommands []string
	blockedCommands []string
}

// Result represents the result of a command execution
type Result struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	StartedAt  time.Time
	FinishedAt time.Time
	Error      error
}

// StreamWriter is called with output chunks during streaming execution
type StreamWriter func(stdout, stderr string)

// New creates a new Executor
func New(shell string, defaultTimeout time.Duration, allowed, blocked []string) *Executor {
	if shell == "" {
		shell = "/bin/sh"
	}
	return &Executor{
		shell:           shell,
		defaultTimeout:  defaultTimeout,
		allowedCommands: allowed,
		blockedCommands: blocked,
	}
}

// Execute runs a command and returns the result
func (e *Executor) Execute(ctx context.Context, command string, args []string, env map[string]string, timeout time.Duration) (*Result, error) {
	// Build full command with quoted args
	fullCmd := command
	if len(args) > 0 {
		quotedArgs := make([]string, len(args))
		for i, arg := range args {
			quotedArgs[i] = e.quoteArg(arg)
		}
		fullCmd = command + " " + strings.Join(quotedArgs, " ")
	}

	// Check if command is blocked
	if e.isBlocked(fullCmd) {
		return nil, fmt.Errorf("command is blocked: %s", fullCmd)
	}

	// Check if command is allowed (if allowlist is set)
	if len(e.allowedCommands) > 0 && !e.isAllowed(fullCmd) {
		return nil, fmt.Errorf("command is not in allowlist: %s", fullCmd)
	}

	// Set timeout
	if timeout == 0 {
		timeout = e.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Create command
	cmd := exec.CommandContext(ctx, e.shell, "-c", fullCmd)

	// Set environment
	if len(env) > 0 {
		for k, v := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := &Result{
		StartedAt: time.Now(),
	}

	// Run command
	err := cmd.Run()
	result.FinishedAt = time.Now()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err
			result.ExitCode = -1
		}
	}

	return result, nil
}

// ExecuteInstall runs a first-party App Installer / clone shell command. It
// is the trusted-install path: the curl|bash hard-block is relaxed (catalog
// installers legitimately pipe vendor scripts into a shell), but isHardBlocked
// STILL applies — so a bypass for installs can never become a bypass for
// nuking the filesystem, reformatting a disk, fork-bombing, shutting the box
// down, or disabling cloudnan-agent. Only the mTLS-gated, BE-installer-only
// app-install sentinel reaches this; user/LLM commands cannot.
func (e *Executor) ExecuteInstall(ctx context.Context, command string, timeout time.Duration) (*Result, error) {
	if e.isHardBlocked(command) {
		return nil, fmt.Errorf("command is blocked: %s", command)
	}
	if timeout == 0 {
		timeout = e.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, e.shell, "-c", command)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := &Result{StartedAt: time.Now()}
	err := cmd.Run()
	result.FinishedAt = time.Now()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err
			result.ExitCode = -1
		}
	}
	return result, nil
}

// ExecuteStream runs a command and streams output in real-time
func (e *Executor) ExecuteStream(ctx context.Context, command string, args []string, env map[string]string, timeout time.Duration, writer StreamWriter) (*Result, error) {
	// Build full command with quoted args
	fullCmd := command
	if len(args) > 0 {
		quotedArgs := make([]string, len(args))
		for i, arg := range args {
			quotedArgs[i] = e.quoteArg(arg)
		}
		fullCmd = command + " " + strings.Join(quotedArgs, " ")
	}

	// Check if command is blocked
	if e.isBlocked(fullCmd) {
		return nil, fmt.Errorf("command is blocked: %s", fullCmd)
	}

	// Set timeout
	if timeout == 0 {
		timeout = e.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Create command
	cmd := exec.CommandContext(ctx, e.shell, "-c", fullCmd)

	// Set environment
	if len(env) > 0 {
		for k, v := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Create pipes for streaming
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	result := &Result{
		StartedAt: time.Now(),
	}

	// Start command
	if err := cmd.Start(); err != nil {
		result.Error = err
		return result, err
	}

	// Stream output
	var wg sync.WaitGroup
	var stdoutBuf, stderrBuf bytes.Buffer

	wg.Add(2)
	go e.streamPipe(&wg, stdoutPipe, &stdoutBuf, func(chunk string) {
		if writer != nil {
			writer(chunk, "")
		}
	})
	go e.streamPipe(&wg, stderrPipe, &stderrBuf, func(chunk string) {
		if writer != nil {
			writer("", chunk)
		}
	})

	// Wait for streams to finish
	wg.Wait()

	// Wait for command to complete
	err := cmd.Wait()
	result.FinishedAt = time.Now()
	result.Stdout = stdoutBuf.String()
	result.Stderr = stderrBuf.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err
			result.ExitCode = -1
		}
	}

	return result, nil
}

func (e *Executor) streamPipe(wg *sync.WaitGroup, pipe io.ReadCloser, buf *bytes.Buffer, callback func(string)) {
	defer wg.Done()

	chunk := make([]byte, 1024)
	for {
		n, err := pipe.Read(chunk)
		if n > 0 {
			data := string(chunk[:n])
			buf.WriteString(data)
			callback(data)
		}
		if err != nil {
			break
		}
	}
}

// isHardBlocked matches only the genuinely-catastrophic patterns
// (filesystem nukes, mkfs/dd on disks, fork bombs, shutdown, disabling
// cloudnan-agent). It deliberately EXCLUDES the curl|bash rule. This is the
// floor that every execution path — including the trusted App Installer
// path — must clear: relaxing curl|bash for first-party installs must never
// also relax "brick the host" or "kill the agent".
func (e *Executor) isHardBlocked(cmd string) bool {
	for _, re := range hardBlockPatterns {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func (e *Executor) isBlocked(cmd string) bool {
	// Anchored regex catastrophes only — these are baked-in patterns
	// that no configuration can re-enable. Even a malicious or
	// misconfigured control plane sending these gets rejected.
	if e.isHardBlocked(cmd) {
		return true
	}
	// curl|bash is blocked for every user/LLM-issued command; only the
	// trusted ExecuteInstall path relaxes it.
	if curlPipeShellPattern.MatchString(cmd) {
		return true
	}
	// The user-configured BlockedCommands substring matcher was
	// removed deliberately. It used strings.Contains, which means any
	// legitimate cleanup command containing a dangerous substring got
	// rejected — "rm -rf /tmp/wp-latest.zip" matched "rm -rf /",
	// "chown -R www-data /var/www" matched "chown -R", etc. This
	// false-positived every install_wordpress / deploy_from_github
	// run on droplets that still had the historical default blocklist
	// in agent.yaml. The hard-block regex above covers the genuinely
	// catastrophic shapes (rm of /etc, mkfs of disks, fork bombs,
	// curl|sh, shutdown, uninstall cloudnan-agent) with anchored
	// patterns that don't false-positive. Operators wanting stricter
	// custom rules can request a regex-based config field in a future
	// release — substring matching is dead.
	_ = e.blockedCommands
	// Note: We deliberately do NOT block "sh -c" / "bash -c" here. The control
	// plane wraps multi-token commands in `sh -c "..."` as a normal protocol
	// (e.g. `sudo wg show interfaces`, pipes, redirects). The control plane is
	// the only authenticated caller and is trusted; per-command authorization
	// belongs to the anchored hardBlockPatterns above, not to a blanket
	// interpreter ban that would break most agent operations.
	return false
}

// MatchedHardBlock returns the first hard-block pattern (as a string)
// that matched the command, or empty string. Exported for tests and
// for richer agent logs ("blocked: rm-of-root pattern" instead of
// just "blocked"). Used by callers that want to report *why* a command
// was rejected rather than just that it was.
func MatchedHardBlock(cmd string) string {
	for _, re := range hardBlockPatterns {
		if re.MatchString(cmd) {
			return re.String()
		}
	}
	// curl|bash lives in its own pattern (relaxed only on the trusted
	// install path) but is still a user-command rejection reason, so
	// report it here too.
	if curlPipeShellPattern.MatchString(cmd) {
		return curlPipeShellPattern.String()
	}
	return ""
}

func (e *Executor) isAllowed(cmd string) bool {
	cmdLower := strings.ToLower(cmd)
	for _, allowed := range e.allowedCommands {
		if strings.HasPrefix(cmdLower, strings.ToLower(allowed)) {
			return true
		}
	}
	return false
}

// quoteArg quotes an argument for shell execution
func (e *Executor) quoteArg(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}
