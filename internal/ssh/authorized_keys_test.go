package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorizedKeysManager_SyncKeys(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ssh-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Mock file paths
	authKeysPath := filepath.Join(tmpDir, "authorized_keys")
	backupDir := filepath.Join(tmpDir, "backups")

	// Create a manager with custom path
	manager := NewAuthorizedKeysManager(backupDir)
	manager.CustomPath = authKeysPath

	// Test Case 1: Write new key
	keys := []KeyEntry{
		{ID: "key1", PublicKey: "ssh-ed25519 AAAA... test", Name: "Key1", IsActive: true},
	}

	count, _, err := manager.SyncKeys(keys, "root", true)
	if err != nil {
		t.Fatalf("SyncKeys failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 key written, got %d", count)
	}

	// Verify file content
	content, err := os.ReadFile(authKeysPath)
	if err != nil {
		t.Fatalf("failed to read authorized_keys: %v", err)
	}
	if !strings.Contains(string(content), "ssh-ed25519 AAAA... test # managed-by-malangpanel") {
		t.Errorf("File content missing key: %s", string(content))
	}

	// Test Case 2: Append Key (Merge)
	// First manually create a pre-existing key
	existingKey := "ssh-rsa BBBB... existing"
	if err := os.WriteFile(authKeysPath, []byte(existingKey+"\n"+string(content)), 0600); err != nil {
		t.Fatalf("failed to write existing key: %v", err)
	}

	newKeys := []KeyEntry{
		{ID: "key2", PublicKey: "ssh-ed25519 CCCC... test2", Name: "Key2", IsActive: true},
	}

	// Sync with replaceAll=false
	count, _, err = manager.SyncKeys(newKeys, "root", false)
	if err != nil {
		t.Fatalf("SyncKeys (append) failed: %v", err)
	}

	// Should write 1 NEW key, but file should contain 3 keys (existing + key1 + key2)?
	// Wait, we passed only `newKeys`. `SyncKeys` logic:
	// readExisting -> returns only non-managed keys.
	// So it preserves "existingKey".
	// It drops "key1" because it was managed-by-malangpanel but NOT in `newKeys`.
	// So result should be: existingKey + key2.

	content, err = os.ReadFile(authKeysPath)
	if err != nil {
		t.Fatalf("failed to read authorized_keys: %v", err)
	}
	strContent := string(content)

	if !strings.Contains(strContent, "ssh-rsa BBBB... existing") {
		t.Error("Existing unmanaged key was removed")
	}
	if !strings.Contains(strContent, "ssh-ed25519 CCCC... test2") {
		t.Error("New key was not added")
	}
	if strings.Contains(strContent, "ssh-ed25519 AAAA... test") {
		t.Error("Old managed key was not removed")
	}
}
