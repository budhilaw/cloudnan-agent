package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the agent configuration
type Config struct {
	Agent        AgentConfig        `yaml:"agent"`
	ControlPlane ControlPlaneConfig `yaml:"control_plane"`
	TLS          TLSConfig          `yaml:"tls"`
	Metrics      MetricsConfig      `yaml:"metrics"`
	Logging      LoggingConfig      `yaml:"logging"`
	Executor     ExecutorConfig     `yaml:"executor"`
}

type AgentConfig struct {
	ID     string            `yaml:"id"`
	Token  string            `yaml:"token"` // Auth token
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels"`
}

type ControlPlaneConfig struct {
	Address              string        `yaml:"address"`
	Timeout              time.Duration `yaml:"timeout"`
	ReconnectInterval    time.Duration `yaml:"reconnect_interval"`
	MaxReconnectAttempts int           `yaml:"max_reconnect_attempts"`
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled"`
	CACert             string `yaml:"ca_cert"`
	Cert               string `yaml:"cert"`
	Key                string `yaml:"key"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	UseSystemCerts     bool   `yaml:"use_system_certs"` // true when connecting via Cloudflare Tunnel (no mTLS)
}

type MetricsConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Interval time.Duration  `yaml:"interval"`
	Collect  MetricsCollect `yaml:"collect"`
}

type MetricsCollect struct {
	CPU     bool `yaml:"cpu"`
	Memory  bool `yaml:"memory"`
	Disk    bool `yaml:"disk"`
	Network bool `yaml:"network"`
	Load    bool `yaml:"load"`
}

type LoggingConfig struct {
	Level                string `yaml:"level"`
	Format               string `yaml:"format"`
	File                 string `yaml:"file"`
	StreamToControlPlane bool   `yaml:"stream_to_control_plane"`
}

type ExecutorConfig struct {
	Shell           string        `yaml:"shell"`
	DefaultTimeout  time.Duration `yaml:"default_timeout"`
	AllowedCommands []string      `yaml:"allowed_commands"`
	BlockedCommands []string      `yaml:"blocked_commands"`
}

// Load reads and parses the configuration file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	cfg.setDefaults()

	// One-time sanitizer for the historical default
	// BlockedCommands. Old agent.yaml files in the wild contain
	// entries like "rm -rf /", "rm -rf /*", "chown -R" — all
	// substring matchers that catch any legitimate cleanup
	// command containing the dangerous bytes as a substring.
	// The hardBlockPatterns in executor.go already cover the
	// catastrophic cases with anchored regex; the substring list
	// was redundant + harmful. Strip the known false-positives so
	// existing droplets stop blocking install_wordpress / deploy
	// flows on every restart without operator intervention.
	cfg.Executor.BlockedCommands = stripKnownBadBlocklist(cfg.Executor.BlockedCommands)

	return cfg, nil
}

// stripKnownBadBlocklist removes substring-match entries that
// historically false-positived legitimate install commands. The
// equivalent anchored regex check lives in executor.hardBlockPatterns
// — operators can re-add custom entries via agent.yaml if they
// want stricter restrictions; we just don't ship the bad defaults.
func stripKnownBadBlocklist(blocked []string) []string {
	badDefaults := map[string]bool{
		"rm -rf /":        true,
		"rm -rf /*":       true,
		"chown -R":        true,
		"chmod -R 777 /":  true,
		"mkfs":            true,
		"dd if=/dev/zero": true,
		"> /dev/sda":      true,
		":(){ :|:& };:":   true,
	}
	out := make([]string, 0, len(blocked))
	for _, b := range blocked {
		if badDefaults[b] {
			continue
		}
		out = append(out, b)
	}
	return out
}

func (c *Config) setDefaults() {
	if c.ControlPlane.Timeout == 0 {
		c.ControlPlane.Timeout = 30 * time.Second
	}
	if c.ControlPlane.ReconnectInterval == 0 {
		c.ControlPlane.ReconnectInterval = 5 * time.Second
	}
	if c.Metrics.Interval == 0 {
		c.Metrics.Interval = 10 * time.Second
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	if c.Executor.Shell == "" {
		c.Executor.Shell = "/bin/bash"
	}
	if c.Executor.DefaultTimeout == 0 {
		c.Executor.DefaultTimeout = 5 * time.Minute
	}
}

// LoadOrCreate tries to load config from file, or creates a default one
func LoadOrCreate(path string) (*Config, error) {
	// Try to load existing config
	if _, err := os.Stat(path); err == nil {
		return Load(path)
	}

	// File doesn't exist, return a default config
	cfg := CreateDefault()
	return cfg, nil
}

// CreateDefault creates a config with sensible defaults
func CreateDefault() *Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "agent"
	}

	cfg := &Config{
		Agent: AgentConfig{
			ID:     "",
			Token:  "",
			Name:   hostname,
			Labels: map[string]string{},
		},
		ControlPlane: ControlPlaneConfig{
			Address:              "localhost:9443",
			Timeout:              30 * time.Second,
			ReconnectInterval:    5 * time.Second,
			MaxReconnectAttempts: 0,
		},
		TLS: TLSConfig{
			Enabled:            false,
			InsecureSkipVerify: false,
		},
		Metrics: MetricsConfig{
			Enabled:  true,
			Interval: 10 * time.Second,
			Collect: MetricsCollect{
				CPU:     true,
				Memory:  true,
				Disk:    true,
				Network: true,
				Load:    true,
			},
		},
		Logging: LoggingConfig{
			Level:                "info",
			Format:               "text",
			File:                 "/var/log/cloudnan-agent.log",
			StreamToControlPlane: true,
		},
		Executor: ExecutorConfig{
			Shell:          "/bin/bash",
			DefaultTimeout: 5 * time.Minute,
			// AllowedCommands empty = allow-all; agent then relies
			// on the anchored regex hardBlockPatterns in executor.go
			// for actual safety.
			AllowedCommands: []string{},
			// BlockedCommands is INTENTIONALLY EMPTY by default.
			//
			// The agent's executor.isBlocked() does substring
			// matching on this list, which catches every false-
			// positive imaginable: "rm -rf /" matches "rm -rf
			// /tmp/foo", "chown -R" matches "chown -R user
			// /var/www/site", and so on. The control plane's
			// install_wordpress / install_app flows kept failing
			// because legitimate cleanup commands literally
			// contain the dangerous substrings.
			//
			// The CATASTROPHIC cases (rm -rf of root system dirs,
			// mkfs against real disks, fork bombs, > /dev/sd*,
			// chown of /etc /usr /boot, curl|sh, shutdown,
			// uninstalling cloudnan-agent itself) are all covered
			// by hardBlockPatterns in executor.go via PROPERLY
			// ANCHORED regex. Those are immutable + baked into
			// the binary; no operator can disable them. The
			// config-level list here is redundant.
			//
			// Operators with stricter needs can still add their
			// own entries via agent.yaml; the substring matcher
			// still works, just isn't pre-populated with traps.
			BlockedCommands: []string{},
		},
	}

	return cfg
}

// Save writes the config to a file
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Create directory if not exists
	dir := path[:len(path)-len("/agent.yaml")]
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create config directory: %w", err)
		}
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}
