package ssh

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// AuthorizedKeysManager handles ~/.ssh/authorized_keys file operations
type AuthorizedKeysManager struct {
	backupDir string
	// CustomPath can be set for testing to override default path resolution
	CustomPath string
}

// KeyEntry represents an SSH key entry
type KeyEntry struct {
	ID        string
	PublicKey string
	Name      string
	IsActive  bool
}

// NewAuthorizedKeysManager creates a new manager
func NewAuthorizedKeysManager(backupDir string) *AuthorizedKeysManager {
	if backupDir == "" {
		backupDir = "/var/backups/malangpanel/ssh"
	}
	return &AuthorizedKeysManager{backupDir: backupDir}
}

// GetAuthorizedKeysPath returns the path to authorized_keys for a user
func (m *AuthorizedKeysManager) GetAuthorizedKeysPath(targetUser string) (string, error) {
	if m.CustomPath != "" {
		return m.CustomPath, nil
	}

	var homeDir string

	if targetUser == "" || targetUser == "root" {
		homeDir = "/root"
	} else {
		u, err := user.Lookup(targetUser)
		if err != nil {
			return "", fmt.Errorf("user not found: %s", targetUser)
		}
		homeDir = u.HomeDir
	}

	return filepath.Join(homeDir, ".ssh", "authorized_keys"), nil
}

// SyncKeys writes the provided keys to authorized_keys
func (m *AuthorizedKeysManager) SyncKeys(keys []KeyEntry, targetUser string, replaceAll bool) (int, string, error) {
	path, err := m.GetAuthorizedKeysPath(targetUser)
	if err != nil {
		return 0, "", err
	}

	// Ensure .ssh directory exists with proper permissions
	sshDir := filepath.Dir(path)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return 0, "", fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	// Create backup if file exists
	backupPath := ""
	if _, err := os.Stat(path); err == nil {
		backupPath, err = m.createBackup(path)
		if err != nil {
			return 0, "", fmt.Errorf("failed to create backup: %w", err)
		}
	}

	// Collect keys to write
	var linesToWrite []string

	// If not replacing all, read existing keys first
	if !replaceAll {
		existing, err := m.readExistingKeys(path)
		if err == nil {
			linesToWrite = append(linesToWrite, existing...)
		}
	}

	// Add new keys (only active ones)
	keyCount := 0
	for _, key := range keys {
		if !key.IsActive {
			continue
		}

		// Format key line with comment
		keyLine := strings.TrimSpace(key.PublicKey)
		if key.Name != "" && !strings.Contains(keyLine, key.Name) {
			// Check if key already has a comment
			parts := strings.Fields(keyLine)
			if len(parts) >= 2 {
				// Append name as comment if not present
				keyLine = fmt.Sprintf("%s # managed-by-malangpanel id=%s name=%s", keyLine, key.ID, key.Name)
			}
		}

		linesToWrite = append(linesToWrite, keyLine)
		keyCount++
	}

	// Write keys to file
	if err := m.writeKeysFile(path, linesToWrite, targetUser); err != nil {
		return 0, backupPath, fmt.Errorf("failed to write authorized_keys: %w", err)
	}

	return keyCount, backupPath, nil
}

// readExistingKeys reads existing keys that are NOT managed by malangpanel
func (m *AuthorizedKeysManager) readExistingKeys(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and malangpanel-managed keys
		if line == "" || strings.Contains(line, "managed-by-malangpanel") {
			continue
		}
		lines = append(lines, line)
	}

	return lines, scanner.Err()
}

// writeKeysFile writes keys to the authorized_keys file with proper permissions
func (m *AuthorizedKeysManager) writeKeysFile(path string, lines []string, targetUser string) error {
	// Remove duplicates
	seen := make(map[string]bool)
	var uniqueLines []string
	for _, line := range lines {
		// Use just the key data (first two parts) for deduplication
		parts := strings.Fields(line)
		var keyID string
		if len(parts) >= 2 {
			keyID = parts[0] + " " + parts[1]
		} else {
			keyID = line
		}

		if !seen[keyID] {
			seen[keyID] = true
			uniqueLines = append(uniqueLines, line)
		}
	}

	// Write to file
	content := strings.Join(uniqueLines, "\n")
	if len(uniqueLines) > 0 {
		content += "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return err
	}

	// Set ownership if not root
	if targetUser != "" && targetUser != "root" {
		u, err := user.Lookup(targetUser)
		if err == nil {
			// Note: This requires running as root
			// In production, use syscall.Chown with uid/gid
			_ = u // ownership change would go here
		}
	}

	return nil
}

// createBackup creates a timestamped backup of the file
func (m *AuthorizedKeysManager) createBackup(path string) (string, error) {
	if err := os.MkdirAll(m.backupDir, 0700); err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405")
	backupPath := filepath.Join(m.backupDir, fmt.Sprintf("authorized_keys.%s.bak", timestamp))

	input, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(backupPath, input, 0600); err != nil {
		return "", err
	}

	return backupPath, nil
}

// CountKeys counts the number of keys in authorized_keys
func (m *AuthorizedKeysManager) CountKeys(targetUser string) (int, error) {
	path, err := m.GetAuthorizedKeysPath(targetUser)
	if err != nil {
		return 0, err
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = file.Close() }()

	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			count++
		}
	}

	return count, scanner.Err()
}

// RemoveKey removes a specific key by ID from authorized_keys
func (m *AuthorizedKeysManager) RemoveKey(keyID string, targetUser string) error {
	path, err := m.GetAuthorizedKeysPath(targetUser)
	if err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Keep lines that don't contain this key ID
		if !strings.Contains(line, fmt.Sprintf("id=%s", keyID)) {
			lines = append(lines, line)
		}
	}
	_ = file.Close()

	if err := scanner.Err(); err != nil {
		return err
	}

	return m.writeKeysFile(path, lines, targetUser)
}
