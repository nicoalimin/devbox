package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the server configuration
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Database   DatabaseConfig   `yaml:"database"`
	Linear     LinearConfig     `yaml:"linear"`
	OpenCode   OpenCodeConfig   `yaml:"opencode"`
	GitHub     GitHubConfig     `yaml:"github"`
	Repos      []RepoConfig     `yaml:"repos"`
	Queue      QueueConfig      `yaml:"queue"`
	Reconciler ReconcilerConfig `yaml:"reconciler"`
}

// ServerConfig defines HTTP server settings
type ServerConfig struct {
	Listen    string `yaml:"listen"`
	AuthToken string `yaml:"auth_token"`
}

// DatabaseConfig defines database settings
type DatabaseConfig struct {
	Path string `yaml:"path"` // Path to SQLite database file
}

// LinearConfig defines Linear API settings
type LinearConfig struct {
	APIKey      string `yaml:"api_key"`
	AssigneeID  string `yaml:"assignee_id"`  // Optional filter
	WorkspaceID string `yaml:"workspace_id"` // Optional
}

// OpenCodeConfig defines OpenCode integration settings
type OpenCodeConfig struct {
	BaseURL  string        `yaml:"base_url"`
	Version  string        `yaml:"version"`  // "v2" or "classic" (default: v2)
	Username string        `yaml:"username"` // Optional
	Password string        `yaml:"password"` // Optional
	Timeout  time.Duration `yaml:"timeout"`  // Session timeout before marking blocked
}

// GitHubConfig defines GitHub integration settings (uses gh CLI)
type GitHubConfig struct {
	DefaultBaseBranch string `yaml:"default_base_branch"`
}

// RepoConfig maps Linear issue attributes to repository paths
type RepoConfig struct {
	Match RepoMatch `yaml:"match"`
	Repo  RepoInfo  `yaml:"repo"`
}

// RepoMatch defines criteria for matching Linear issues to repos
type RepoMatch struct {
	Team    string `yaml:"team"`    // Match by team key
	Project string `yaml:"project"` // Match by project name
	Label   string `yaml:"label"`   // Match by label
}

// RepoInfo defines repository details
type RepoInfo struct {
	Path       string `yaml:"path"`
	BaseBranch string `yaml:"base_branch"`
}

// QueueConfig defines job queue settings
type QueueConfig struct {
	Enabled  bool `yaml:"enabled"`   // Default: false (reject when busy)
	MaxDepth int  `yaml:"max_depth"` // Max queued jobs
}

// ReconcilerConfig defines the periodic GitHub PR reconciler settings.
//
// The reconciler polls GitHub for jobs stuck in pr_open and advances them
// to a terminal state when the PR is merged or closed.
// PR-not-found is treated as CLOSED/cancelled (see orchestrator.reconcileJobs).
type ReconcilerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

// UnmarshalYAML applies defaults (enabled=true, interval=2m) while still
// allowing explicit `enabled: false` or a custom interval.
func (r *ReconcilerConfig) UnmarshalYAML(value *yaml.Node) error {
	// Defaults.
	r.Enabled = true
	r.Interval = 2 * time.Minute

	// Alias to avoid recursion.
	type plain ReconcilerConfig
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	// value.Decode into plain loses the "was field present?" signal for
	// Enabled (false could mean unset or explicit false). Decode into a map
	// first to detect explicit presence.
	var raw map[string]any
	if err := value.Decode(&raw); err == nil && raw != nil {
		if _, ok := raw["enabled"]; ok {
			r.Enabled = p.Enabled
		} // else keep default true
	} else {
		r.Enabled = p.Enabled
	}
	if p.Interval != 0 {
		r.Interval = p.Interval
	}
	return nil
}

// LoadConfig loads configuration from a YAML file, with env var overrides
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Environment variable overrides
	if token := os.Getenv("DEVBOXD_AUTH_TOKEN"); token != "" {
		cfg.Server.AuthToken = token
	}
	if dbPath := os.Getenv("DEVBOXD_DB_PATH"); dbPath != "" {
		cfg.Database.Path = dbPath
	}
	if apiKey := os.Getenv("LINEAR_API_KEY"); apiKey != "" {
		cfg.Linear.APIKey = apiKey
	}
	if assigneeID := os.Getenv("LINEAR_ASSIGNEE_ID"); assigneeID != "" {
		cfg.Linear.AssigneeID = assigneeID
	}
	if baseURL := os.Getenv("OPENCODE_BASE_URL"); baseURL != "" {
		cfg.OpenCode.BaseURL = baseURL
	}
	// OpenCode authentication - support both DEVBOXD_ prefix and OPENCODE_SERVER_ (matching OpenCode server vars)
	if username := os.Getenv("DEVBOXD_OPENCODE_USERNAME"); username != "" {
		cfg.OpenCode.Username = username
	} else if username := os.Getenv("OPENCODE_SERVER_USERNAME"); username != "" {
		cfg.OpenCode.Username = username
	}
	if password := os.Getenv("DEVBOXD_OPENCODE_PASSWORD"); password != "" {
		cfg.OpenCode.Password = password
	} else if password := os.Getenv("OPENCODE_SERVER_PASSWORD"); password != "" {
		cfg.OpenCode.Password = password
	}

	// Defaults
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "0.0.0.0:8080"
	}
	if cfg.OpenCode.BaseURL == "" {
		cfg.OpenCode.BaseURL = "http://127.0.0.1:3000"
	}
	if cfg.OpenCode.Version == "" {
		cfg.OpenCode.Version = "v2"
	}
	if cfg.OpenCode.Timeout == 0 {
		cfg.OpenCode.Timeout = 30 * time.Minute
	}
	if cfg.GitHub.DefaultBaseBranch == "" {
		cfg.GitHub.DefaultBaseBranch = "main"
	}
	// Reconciler defaults: enabled=true, interval=2m.
	// UnmarshalYAML already applies these when the `reconciler:` section is
	// present. When the section is absent entirely, the zero value remains
	// (Enabled=false, Interval=0) — detect via Interval==0 and apply defaults.
	if cfg.Reconciler.Interval == 0 {
		// Distinguish "section missing" (Interval==0) from explicit config.
		// Explicit `enabled: false` without interval still gets Interval=2m
		// from UnmarshalYAML, so Interval==0 reliably means missing section.
		cfg.Reconciler.Interval = 2 * time.Minute
		// Only default Enabled=true when the section was missing. If the user
		// explicitly set enabled:false with interval:0s (nonsensical), we still
		// enable with 2m — interval 0 would spin the loop.
		// To preserve an explicit enabled:false with no interval, users should
		// set an interval; but missing section is the common case.
		// Check raw file for a reconciler key to be precise.
		hasReconcilerKey := false
		// Best-effort: re-parse as generic map (ignore errors, defaults already set).
		var generic map[string]any
		if err := yaml.Unmarshal(data, &generic); err == nil {
			_, hasReconcilerKey = generic["reconciler"]
		}
		if !hasReconcilerKey {
			cfg.Reconciler.Enabled = true
		}
	}

	// Environment variable overrides for reconciler
	if v := os.Getenv("DEVBOXD_RECONCILER_ENABLED"); v != "" {
		switch v {
		case "1", "true", "TRUE", "True", "yes", "YES":
			cfg.Reconciler.Enabled = true
		case "0", "false", "FALSE", "False", "no", "NO":
			cfg.Reconciler.Enabled = false
		}
	}
	if v := os.Getenv("DEVBOXD_RECONCILER_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Reconciler.Interval = d
		}
	}
	if cfg.Reconciler.Interval <= 0 {
		cfg.Reconciler.Interval = 2 * time.Minute
	}

	// Validation
	if cfg.Server.AuthToken == "" {
		return nil, fmt.Errorf("server.auth_token is required (or set DEVBOXD_AUTH_TOKEN)")
	}
	if cfg.Linear.APIKey == "" {
		return nil, fmt.Errorf("linear.api_key is required (or set LINEAR_API_KEY)")
	}
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("at least one repo must be configured")
	}

	return &cfg, nil
}

// GetDefaultDBPath returns the default database path (in-repo .devbox/jobs.db)
func GetDefaultDBPath() string {
	return filepath.Join(".devbox", "jobs.db")
}

// GetDBPath returns the configured database path or the default
func (c *Config) GetDBPath() string {
	if c.Database.Path != "" {
		return c.Database.Path
	}
	return GetDefaultDBPath()
}

// FindRepo finds the matching repository for a Linear issue
func (c *Config) FindRepo(team, project string, labels []string) *RepoInfo {
	for _, repo := range c.Repos {
		if repo.Match.Team != "" && repo.Match.Team == team {
			return &repo.Repo
		}
		if repo.Match.Project != "" && repo.Match.Project == project {
			return &repo.Repo
		}
		if repo.Match.Label != "" {
			for _, label := range labels {
				if repo.Match.Label == label {
					return &repo.Repo
				}
			}
		}
	}
	return nil
}
