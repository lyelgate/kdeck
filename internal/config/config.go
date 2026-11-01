// Package config loads kp's user configuration and small pieces of state.
//
// Everything lives under one home directory, $KP_HOME or ~/.kp:
//
//	config.yaml   user settings: aliases, silent thresholds and suppressions
//	state.json    previous context and namespace, for "kp ctx -" and "kp ns -"
//	bin/          plugin executables (kubectl-<name>), put on PATH at start-up
//	store/        unpacked plugin versions
//	receipts/     one file per installed plugin
//	index/        local copy of the krew plugin index
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Config is the content of config.yaml.
type Config struct {
	// Aliases maps a word to the arguments it expands to, for example
	// "kgpr": "get pods --field-selector=status.phase=Running".
	Aliases  map[string]string `json:"aliases,omitempty"`
	Silent   SilentConfig      `json:"silent,omitempty"`
	Linode   LinodeConfig      `json:"linode,omitempty"`
	Teleport TeleportConfig    `json:"teleport,omitempty"`
}

// TeleportConfig is set once by "kp setup" or "kp tsh setup"; afterwards the
// short commands (kp tl, kp tls, kp tu) need nothing else.
type TeleportConfig struct {
	Proxy string `json:"proxy,omitempty"` // e.g. teleport.example.com:443
	User  string `json:"user,omitempty"`  // optional; tsh defaults to the local user
	Auth  string `json:"auth,omitempty"`  // optional auth connector (github, okta, ...)
}

// SilentConfig tunes "kp silent".
type SilentConfig struct {
	// Checks holds per-check overrides keyed by check ID.
	Checks map[string]CheckConfig `json:"checks,omitempty"`
	// ExcludeNamespaces skips findings in these namespaces (glob patterns).
	ExcludeNamespaces []string `json:"excludeNamespaces,omitempty"`
	// BackupLabels marks CronJobs as backups ("key=value"), which raises
	// the severity of SF-CRON-001 to critical.
	BackupLabels []string      `json:"backupLabels,omitempty"`
	Suppressions []Suppression `json:"suppressions,omitempty"`
}

// CheckConfig overrides one check.
type CheckConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
	// Thresholds are check-specific numbers, for example
	// {"missedRuns": 5} for SF-CRON-001 or {"criticalDays": 14} for certificates.
	Thresholds map[string]float64 `json:"thresholds,omitempty"`
}

// Suppression hides a known, accepted finding until a date.
type Suppression struct {
	Check  string `json:"check"`
	Object string `json:"object"` // "namespace/Kind/name", "*" wildcards allowed
	Reason string `json:"reason"`
	Until  string `json:"until,omitempty"` // YYYY-MM-DD
}

// LinodeConfig holds Linode defaults. The API token is never stored here;
// it comes from --token, LINODE_TOKEN or the linode-cli config file.
type LinodeConfig struct {
	DefaultRegion string `json:"defaultRegion,omitempty"`
}

// State is small, machine-written data.
type State struct {
	PreviousContext   string            `json:"previousContext,omitempty"`
	PreviousNamespace map[string]string `json:"previousNamespace,omitempty"` // per context
}

// Home returns kp's home directory, creating it if needed.
func Home() string {
	dir := os.Getenv("KP_HOME")
	if dir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			h = "."
		}
		dir = filepath.Join(h, ".kp")
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// Path joins elements onto kp's home directory.
func Path(elem ...string) string {
	return filepath.Join(append([]string{Home()}, elem...)...)
}

// Load reads config.yaml. A missing file is an empty config, not an error.
func Load() (*Config, error) {
	c := &Config{}
	data, err := os.ReadFile(Path("config.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Save writes config.yaml.
func (c *Config) Save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(Path("config.yaml"), data, 0o600)
}

// LoadState reads state.json; a missing or corrupt file yields empty state.
func LoadState() *State {
	s := &State{}
	if data, err := os.ReadFile(Path("state.json")); err == nil {
		_ = json.Unmarshal(data, s)
	}
	if s.PreviousNamespace == nil {
		s.PreviousNamespace = map[string]string{}
	}
	return s
}

// Save writes state.json.
func (s *State) Save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path("state.json"), data, 0o600)
}
