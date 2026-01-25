package ssh

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SSHDConfigManager handles /etc/ssh/sshd_config operations
type SSHDConfigManager struct {
	configPath string
	backupDir  string
}

// SSHDConfig represents SSH daemon configuration
type SSHDConfig struct {
	Port            int
	PermitRootLogin bool
	PasswordAuth    bool
	PubkeyAuth      bool
}

// NewSSHDConfigManager creates a new manager
func NewSSHDConfigManager(configPath, backupDir string) *SSHDConfigManager {
	if configPath == "" {
		configPath = "/etc/ssh/sshd_config"
	}
	if backupDir == "" {
		backupDir = "/var/backups/cloudnan/ssh"
	}
	return &SSHDConfigManager{
		configPath: configPath,
		backupDir:  backupDir,
	}
}

// GetCurrentConfig reads the current sshd_config settings
func (m *SSHDConfigManager) GetCurrentConfig() (*SSHDConfig, error) {
	file, err := os.Open(m.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sshd_config: %w", err)
	}
	defer func() { _ = file.Close() }()

	config := &SSHDConfig{
		Port:            22,   // Default
		PermitRootLogin: true, // Default varies by distro
		PasswordAuth:    true,
		PubkeyAuth:      true,
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		key := strings.ToLower(parts[0])
		value := parts[1]

		switch key {
		case "port":
			if p, err := strconv.Atoi(value); err == nil {
				config.Port = p
			}
		case "permitrootlogin":
			config.PermitRootLogin = parseBoolValue(value)
		case "passwordauthentication":
			config.PasswordAuth = parseBoolValue(value)
		case "pubkeyauthentication":
			config.PubkeyAuth = parseBoolValue(value)
		}
	}

	return config, scanner.Err()
}

// UpdateConfig updates sshd_config with new settings
func (m *SSHDConfigManager) UpdateConfig(port *int, permitRootLogin, passwordAuth, pubkeyAuth *bool) (string, error) {
	// Create backup first
	backupPath, err := m.createBackup()
	if err != nil {
		return "", fmt.Errorf("failed to create backup: %w", err)
	}

	// Read current config
	content, err := os.ReadFile(m.configPath)
	if err != nil {
		return backupPath, fmt.Errorf("failed to read sshd_config: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	updated := make(map[string]bool)

	// Update existing lines
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Handle commented lines that we might want to uncomment
		isCommented := strings.HasPrefix(trimmed, "#")
		if isCommented {
			trimmed = strings.TrimPrefix(trimmed, "#")
			trimmed = strings.TrimSpace(trimmed)
		}

		parts := strings.Fields(trimmed)
		if len(parts) < 1 {
			continue
		}

		key := strings.ToLower(parts[0])

		switch key {
		case "port":
			if port != nil {
				lines[i] = fmt.Sprintf("Port %d", *port)
				updated["port"] = true
			}
		case "permitrootlogin":
			if permitRootLogin != nil {
				lines[i] = fmt.Sprintf("PermitRootLogin %s", boolToSSHValue(*permitRootLogin))
				updated["permitrootlogin"] = true
			}
		case "passwordauthentication":
			if passwordAuth != nil {
				lines[i] = fmt.Sprintf("PasswordAuthentication %s", boolToSSHValue(*passwordAuth))
				updated["passwordauthentication"] = true
			}
		case "pubkeyauthentication":
			if pubkeyAuth != nil {
				lines[i] = fmt.Sprintf("PubkeyAuthentication %s", boolToSSHValue(*pubkeyAuth))
				updated["pubkeyauthentication"] = true
			}
		}
	}

	// Add settings that weren't found
	additions := []string{}
	if port != nil && !updated["port"] {
		additions = append(additions, fmt.Sprintf("Port %d", *port))
	}
	if permitRootLogin != nil && !updated["permitrootlogin"] {
		additions = append(additions, fmt.Sprintf("PermitRootLogin %s", boolToSSHValue(*permitRootLogin)))
	}
	if passwordAuth != nil && !updated["passwordauthentication"] {
		additions = append(additions, fmt.Sprintf("PasswordAuthentication %s", boolToSSHValue(*passwordAuth)))
	}
	if pubkeyAuth != nil && !updated["pubkeyauthentication"] {
		additions = append(additions, fmt.Sprintf("PubkeyAuthentication %s", boolToSSHValue(*pubkeyAuth)))
	}

	if len(additions) > 0 {
		lines = append(lines, "")
		lines = append(lines, "# Added by Cloudnan")
		lines = append(lines, additions...)
	}

	// Write updated config
	newContent := strings.Join(lines, "\n")
	if err := os.WriteFile(m.configPath, []byte(newContent), 0644); err != nil {
		return backupPath, fmt.Errorf("failed to write sshd_config: %w", err)
	}

	// Ensure Port is not duplicated in included config files
	if port != nil {
		if err := m.updatePortInIncludedFiles(*port); err != nil {
			return backupPath, fmt.Errorf("failed to update port in included sshd configs: %w", err)
		}
		if err := m.updatePortInDefaultIncludeDir(*port); err != nil {
			return backupPath, fmt.Errorf("failed to update port in default include dir: %w", err)
		}
	}

	return backupPath, nil
}

// updatePortInDefaultIncludeDir updates Port directives under /etc/ssh/sshd_config.d/*.conf
func (m *SSHDConfigManager) updatePortInDefaultIncludeDir(port int) error {
	pattern := "/etc/ssh/sshd_config.d/*.conf"
	files, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if err := m.updatePortInFile(path, port); err != nil {
			return err
		}
	}
	return nil
}

// updatePortInIncludedFiles replaces any Port directives in included sshd config files
func (m *SSHDConfigManager) updatePortInIncludedFiles(port int) error {
	patterns, err := m.getIncludePatterns()
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		return nil
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			if err := m.updatePortInFile(path, port); err != nil {
				return err
			}
		}
	}

	return nil
}

// getIncludePatterns returns Include patterns from the main sshd_config
func (m *SSHDConfigManager) getIncludePatterns() ([]string, error) {
	content, err := os.ReadFile(m.configPath)
	if err != nil {
		return nil, err
	}

	var patterns []string
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			continue
		}

		if strings.EqualFold(parts[0], "Include") {
			for _, p := range parts[1:] {
				pattern := p
				if !filepath.IsAbs(pattern) {
					pattern = filepath.Join(filepath.Dir(m.configPath), pattern)
				}
				patterns = append(patterns, pattern)
			}
		}
	}

	return patterns, nil
}

// updatePortInFile updates all Port directives in a single sshd config file
func (m *SSHDConfigManager) updatePortInFile(path string, port int) error {
	_, err := m.createBackupForPath(path)
	if err != nil {
		return err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")
	updated := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		isCommented := strings.HasPrefix(trimmed, "#")
		if isCommented {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		}
		parts := strings.Fields(trimmed)
		if len(parts) < 1 {
			continue
		}
		if strings.EqualFold(parts[0], "Port") {
			lines[i] = fmt.Sprintf("Port %d", port)
			updated = true
		}
	}

	if !updated {
		return nil
	}

	newContent := strings.Join(lines, "\n")
	return os.WriteFile(path, []byte(newContent), 0644)
}

// ValidateConfig runs sshd -t to validate the configuration
func (m *SSHDConfigManager) ValidateConfig() error {
	cmd := exec.Command("sshd", "-t")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sshd config validation failed: %s", string(output))
	}
	return nil
}

// RestartSSHD restarts the SSH daemon
func (m *SSHDConfigManager) RestartSSHD() error {
	// Try systemctl first (most modern systems)
	cmd := exec.Command("systemctl", "restart", "sshd")
	if err := cmd.Run(); err == nil {
		return nil
	}

	// Try systemctl with ssh (some systems use 'ssh' instead of 'sshd')
	cmd = exec.Command("systemctl", "restart", "ssh")
	if err := cmd.Run(); err == nil {
		return nil
	}

	// Fallback to service command
	cmd = exec.Command("service", "sshd", "restart")
	if err := cmd.Run(); err == nil {
		return nil
	}

	cmd = exec.Command("service", "ssh", "restart")
	return cmd.Run()
}

// GetEffectivePorts returns the effective ports from sshd -T output
func (m *SSHDConfigManager) GetEffectivePorts() ([]int, error) {
	cmd := exec.Command("sshd", "-T")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sshd -T failed: %s", string(output))
	}

	ports := []int{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "port ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if p, err := strconv.Atoi(parts[1]); err == nil {
			ports = append(ports, p)
		}
	}

	return ports, nil
}

// GetSSHDStatus returns the status of the SSH daemon
func (m *SSHDConfigManager) GetSSHDStatus() string {
	// Try systemctl
	cmd := exec.Command("systemctl", "is-active", "sshd")
	output, err := cmd.Output()
	if err == nil {
		status := strings.TrimSpace(string(output))
		if status == "active" {
			return "running"
		}
		return status
	}

	// Try with 'ssh' service name
	cmd = exec.Command("systemctl", "is-active", "ssh")
	output, err = cmd.Output()
	if err == nil {
		status := strings.TrimSpace(string(output))
		if status == "active" {
			return "running"
		}
		return status
	}

	// Check if process is running
	cmd = exec.Command("pgrep", "-x", "sshd")
	if err := cmd.Run(); err == nil {
		return "running"
	}

	return "unknown"
}

// RestoreBackup restores the config from a backup file
func (m *SSHDConfigManager) RestoreBackup(backupPath string) error {
	content, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("failed to read backup: %w", err)
	}

	if err := os.WriteFile(m.configPath, content, 0644); err != nil {
		return fmt.Errorf("failed to restore config: %w", err)
	}

	return nil
}

// createBackup creates a timestamped backup of sshd_config
func (m *SSHDConfigManager) createBackup() (string, error) {
	if err := os.MkdirAll(m.backupDir, 0700); err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405")
	backupPath := filepath.Join(m.backupDir, fmt.Sprintf("sshd_config.%s.bak", timestamp))

	content, err := os.ReadFile(m.configPath)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(backupPath, content, 0600); err != nil {
		return "", err
	}

	return backupPath, nil
}

// createBackupForPath creates a timestamped backup for a specific config file
func (m *SSHDConfigManager) createBackupForPath(path string) (string, error) {
	if err := os.MkdirAll(m.backupDir, 0700); err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405")
	base := filepath.Base(path)
	backupPath := filepath.Join(m.backupDir, fmt.Sprintf("%s.%s.bak", base, timestamp))

	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(backupPath, content, 0600); err != nil {
		return "", err
	}

	return backupPath, nil
}

// parseBoolValue parses SSH config boolean values
func parseBoolValue(value string) bool {
	value = strings.ToLower(value)
	return value == "yes" || value == "true" || value == "1"
}

// boolToSSHValue converts bool to SSH config format
func boolToSSHValue(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// IsPortInUse checks if a port is already in use (other than by sshd)
func IsPortInUse(port int) bool {
	// Use netstat or ss to check
	cmd := exec.Command("ss", "-tlnp")
	output, err := cmd.Output()
	if err != nil {
		return false // Assume not in use if we can't check
	}

	pattern := regexp.MustCompile(fmt.Sprintf(`:%d\s`, port))
	return pattern.Match(output)
}
