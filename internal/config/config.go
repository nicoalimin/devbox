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
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Linear   LinearConfig   `yaml:"linear"`
	OpenCode OpenCodeConfig `yaml:"opencode"`
	GitHub   GitHubConfig   `yaml:"github"`
	Repos    []RepoConfig   `yaml:"repos"`
	Queue    QueueConfig    `yaml:"queue"`
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
