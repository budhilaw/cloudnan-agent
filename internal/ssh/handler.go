package ssh

import (
	"fmt"
)

// Handler orchestrates SSH key and config management
type Handler struct {
	keysManager   *AuthorizedKeysManager
	configManager *SSHDConfigManager
}

// NewHandler creates a new SSH handler
func NewHandler(backupDir string) *Handler {
	return &Handler{
		keysManager:   NewAuthorizedKeysManager(backupDir),
		configManager: NewSSHDConfigManager("", backupDir),
	}
}

// SyncKeysRequest represents a request to sync SSH keys
type SyncKeysRequest struct {
	Keys       []KeyEntry
	TargetUser string
	ReplaceAll bool
}

// SyncKeysResponse represents the result of syncing keys
type SyncKeysResponse struct {
	Success    bool
	Message    string
	KeysSynced int
	BackupPath string
}

// SyncKeys syncs SSH keys to authorized_keys file
func (h *Handler) SyncKeys(req *SyncKeysRequest) *SyncKeysResponse {
	targetUser := req.TargetUser
	if targetUser == "" {
		targetUser = "root"
	}

	keysSynced, backupPath, err := h.keysManager.SyncKeys(req.Keys, targetUser, req.ReplaceAll)
	if err != nil {
		return &SyncKeysResponse{
			Success:    false,
			Message:    fmt.Sprintf("Failed to sync keys: %v", err),
			BackupPath: backupPath,
		}
	}

	return &SyncKeysResponse{
		Success:    true,
		Message:    fmt.Sprintf("Successfully synced %d keys", keysSynced),
		KeysSynced: keysSynced,
		BackupPath: backupPath,
	}
}

// ConfigUpdateRequest represents a request to update SSH config
type ConfigUpdateRequest struct {
	Port            *int
	PermitRootLogin *bool
	PasswordAuth    *bool
	PubkeyAuth      *bool
	RestartSSHD     bool
	ValidateConfig  bool
}

// ConfigUpdateResponse represents the result of updating config
type ConfigUpdateResponse struct {
	Success       bool
	Message       string
	BackupPath    string
	SSHDRestarted bool
}

// UpdateConfig updates sshd_config with new settings
func (h *Handler) UpdateConfig(req *ConfigUpdateRequest) *ConfigUpdateResponse {
	// Validate port if provided
	if req.Port != nil {
		if *req.Port < 1 || *req.Port > 65535 {
			return &ConfigUpdateResponse{
				Success: false,
				Message: "Invalid port number (must be 1-65535)",
			}
		}
	}

	// Update config
	backupPath, err := h.configManager.UpdateConfig(
		req.Port,
		req.PermitRootLogin,
		req.PasswordAuth,
		req.PubkeyAuth,
	)
	if err != nil {
		return &ConfigUpdateResponse{
			Success:    false,
			Message:    fmt.Sprintf("Failed to update config: %v", err),
			BackupPath: backupPath,
		}
	}

	// Validate if requested
	if req.ValidateConfig {
		if err := h.configManager.ValidateConfig(); err != nil {
			// Restore backup on validation failure
			if backupPath != "" {
				_ = h.configManager.RestoreBackup(backupPath)
			}
			return &ConfigUpdateResponse{
				Success:    false,
				Message:    fmt.Sprintf("Config validation failed, restored backup: %v", err),
				BackupPath: backupPath,
			}
		}
	}

	// Restart if requested
	sshdRestarted := false
	if req.RestartSSHD {
		if err := h.configManager.RestartSSHD(); err != nil {
			return &ConfigUpdateResponse{
				Success:    false,
				Message:    fmt.Sprintf("Config updated but failed to restart sshd: %v", err),
				BackupPath: backupPath,
			}
		}
		sshdRestarted = true

		// Verify effective port if provided
		if req.Port != nil {
			ports, err := h.configManager.GetEffectivePorts()
			if err != nil {
				return &ConfigUpdateResponse{
					Success:    false,
					Message:    fmt.Sprintf("Config updated but failed to verify sshd port: %v", err),
					BackupPath: backupPath,
				}
			}
			if len(ports) == 0 {
				return &ConfigUpdateResponse{
					Success:    false,
					Message:    "Config updated but sshd reported no active ports",
					BackupPath: backupPath,
				}
			}
			for _, p := range ports {
				if p != *req.Port {
					return &ConfigUpdateResponse{
						Success:    false,
						Message:    fmt.Sprintf("Config updated but sshd still listening on port %d", p),
						BackupPath: backupPath,
					}
				}
			}
		}
	}

	return &ConfigUpdateResponse{
		Success:       true,
		Message:       "SSH configuration updated successfully",
		BackupPath:    backupPath,
		SSHDRestarted: sshdRestarted,
	}
}

// StatusResponse represents the current SSH status
type StatusResponse struct {
	CurrentPort         int
	PermitRootLogin     bool
	PasswordAuth        bool
	PubkeyAuth          bool
	SSHDStatus          string
	AuthorizedKeysCount int
}

// GetStatus returns the current SSH configuration and status
func (h *Handler) GetStatus(targetUser string) (*StatusResponse, error) {
	if targetUser == "" {
		targetUser = "root"
	}

	config, err := h.configManager.GetCurrentConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	keyCount, err := h.keysManager.CountKeys(targetUser)
	if err != nil {
		keyCount = 0 // Don't fail if we can't count keys
	}

	return &StatusResponse{
		CurrentPort:         config.Port,
		PermitRootLogin:     config.PermitRootLogin,
		PasswordAuth:        config.PasswordAuth,
		PubkeyAuth:          config.PubkeyAuth,
		SSHDStatus:          h.configManager.GetSSHDStatus(),
		AuthorizedKeysCount: keyCount,
	}, nil
}

// RemoveKey removes a specific key from authorized_keys
func (h *Handler) RemoveKey(keyID string, targetUser string) error {
	if targetUser == "" {
		targetUser = "root"
	}
	return h.keysManager.RemoveKey(keyID, targetUser)
}

// RestartSSHD restarts the SSH daemon
func (h *Handler) RestartSSHD() error {
	return h.configManager.RestartSSHD()
}
