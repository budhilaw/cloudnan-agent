package monitor

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type FirewallRule struct {
	Index    string
	To       string
	Action   string
	From     string
	Comment  string
	Protocol string
}

type FirewallInfo struct {
	Status string
	Rules  []FirewallRule
}

// GetFirewall returns status of ufw. Cached for shellCacheTTL and hard-bounded
// by shellCmdTimeout. Running `ufw status` (via sudo) on the 5s metrics tick
// flooded the auth log and risked stalling collection; this serves a cached
// snapshot and only re-shells at most every shellCacheTTL.
func (m *Monitor) GetFirewall() *FirewallInfo {
	m.shellMu.Lock()
	if m.firewallCache != nil && time.Since(m.firewallAt) < shellCacheTTL {
		cached := m.firewallCache
		m.shellMu.Unlock()
		return cached
	}
	m.shellMu.Unlock()

	fresh, ok := m.collectFirewall()

	m.shellMu.Lock()
	defer m.shellMu.Unlock()
	if ok {
		m.firewallCache = fresh
		m.firewallAt = time.Now()
		return fresh
	}
	if m.firewallCache != nil {
		return m.firewallCache
	}
	return fresh
}

// collectFirewall does the actual ufw shell-out. The bool is false only when
// the command times out (so the caller keeps the previous cache); installed/
// not-installed/permission-denied are all legitimate (true) outcomes.
func (m *Monitor) collectFirewall() (*FirewallInfo, bool) {
	info := &FirewallInfo{
		Status: "inactive",
		Rules:  []FirewallRule{},
	}

	// Check if ufw is installed
	_, err := exec.LookPath("ufw")
	if err != nil {
		// Try common paths
		commonPaths := []string{"/usr/sbin/ufw", "/sbin/ufw"}
		found := false
		for _, p := range commonPaths {
			vctx, vcancel := context.WithTimeout(context.Background(), shellCmdTimeout)
			runErr := exec.CommandContext(vctx, p, "--version").Run()
			vcancel()
			if runErr == nil {
				found = true
				break
			}
		}
		if !found {
			info.Status = "not_installed"
			return info, true
		}
	}

	// Run ufw status numbered
	// We need sudo for ufw usually, but agent runs as root hopefully?
	// If not root, this might fail with "ERROR: You need to be root..."
	ctx, cancel := context.WithTimeout(context.Background(), shellCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ufw", "status", "numbered")
	output, err := cmd.CombinedOutput()
	outStr := string(output)

	if err != nil {
		// A timeout means we couldn't determine state — keep the last cache.
		if ctx.Err() == context.DeadlineExceeded {
			return info, false
		}
		// If permission denied or other error
		if strings.Contains(outStr, "You need to be root") {
			info.Status = "permission_denied"
		} else {
			info.Status = "error"
		}
		return info, true
	}

	if strings.Contains(outStr, "Status: active") {
		info.Status = "active"
	}

	if info.Status == "active" {
		lines := strings.Split(outStr, "\n")
		// Regex to parse: [ 1] 22/tcp ALLOW IN Anywhere
		re := regexp.MustCompile(`\[\s*(\d+)]\s+([\w/:.-]+(?:\s+\(v6\))?)\s+((?:ALLOW|DENY|REJECT|LIMIT)\s+(?:IN|OUT))\s+(.*)`)

		for _, line := range lines {
			line = strings.TrimSpace(line)
			matches := re.FindStringSubmatch(line)
			if len(matches) == 5 {
				toParts := strings.Split(matches[2], "/")
				protocol := ""
				if len(toParts) > 1 {
					protocol = toParts[1]
				}

				info.Rules = append(info.Rules, FirewallRule{
					Index:    matches[1],
					To:       matches[2],
					Action:   matches[3],
					From:     matches[4],
					Protocol: protocol,
				})
			}
		}
	}

	return info, true
}
