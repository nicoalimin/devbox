package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadConfigValidationCommands(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configContent := `
server:
  auth_token: test-token
linear:
  api_key: test-api-key
opencode:
  review_timeout: 7m
repos:
  - match:
      team: TEST
    repo:
      path: /tmp/test
      base_branch: main
      validation_commands:
        - go test ./...
        - go vet ./...
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	want := []string{"go test ./...", "go vet ./..."}
	if !reflect.DeepEqual(cfg.Repos[0].Repo.ValidationCommands, want) {
		t.Fatalf("validation commands = %#v, want %#v", cfg.Repos[0].Repo.ValidationCommands, want)
	}
	if cfg.OpenCode.ReviewTimeout != 7*time.Minute {
		t.Fatalf("review timeout = %v, want 7m", cfg.OpenCode.ReviewTimeout)
	}
}

func TestOpenCodeAuthEnvVars(t *testing.T) {
	// Create a minimal valid config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test-config.yaml")

	configContent := `
server:
  listen: "0.0.0.0:8080"
  auth_token: "test-token"

linear:
  api_key: "test-api-key"

repos:
  - match:
      team: "TEST"
    repo:
      path: "/tmp/test"
      base_branch: "main"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	tests := []struct {
		name         string
		envVars      map[string]string
		wantUsername string
		wantPassword string
	}{
		{
			name:         "no env vars - empty credentials",
			envVars:      map[string]string{},
			wantUsername: "",
			wantPassword: "",
		},
		{
			name: "DEVBOXD_OPENCODE_USERNAME and PASSWORD",
			envVars: map[string]string{
				"DEVBOXD_OPENCODE_USERNAME": "devbox-user",
				"DEVBOXD_OPENCODE_PASSWORD": "devbox-pass",
			},
			wantUsername: "devbox-user",
			wantPassword: "devbox-pass",
		},
		{
			name: "OPENCODE_SERVER_USERNAME and PASSWORD",
			envVars: map[string]string{
				"OPENCODE_SERVER_USERNAME": "opencode",
				"OPENCODE_SERVER_PASSWORD": "server-pass",
			},
			wantUsername: "opencode",
			wantPassword: "server-pass",
		},
		{
			name: "DEVBOXD prefix takes priority over OPENCODE_SERVER",
			envVars: map[string]string{
				"DEVBOXD_OPENCODE_USERNAME": "devbox-user",
				"DEVBOXD_OPENCODE_PASSWORD": "devbox-pass",
				"OPENCODE_SERVER_USERNAME":  "opencode",
				"OPENCODE_SERVER_PASSWORD":  "server-pass",
			},
			wantUsername: "devbox-user",
			wantPassword: "devbox-pass",
		},
		{
			name: "mixed: DEVBOXD username, OPENCODE_SERVER password",
			envVars: map[string]string{
				"DEVBOXD_OPENCODE_USERNAME": "devbox-user",
				"OPENCODE_SERVER_PASSWORD":  "server-pass",
			},
			wantUsername: "devbox-user",
			wantPassword: "server-pass",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear existing env vars
			os.Unsetenv("DEVBOXD_OPENCODE_USERNAME")
			os.Unsetenv("DEVBOXD_OPENCODE_PASSWORD")
			os.Unsetenv("OPENCODE_SERVER_USERNAME")
			os.Unsetenv("OPENCODE_SERVER_PASSWORD")

			// Set test env vars
			for key, value := range tt.envVars {
				os.Setenv(key, value)
			}

			// Load config
			cfg, err := LoadConfig(configPath)
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}

			// Verify username
			if cfg.OpenCode.Username != tt.wantUsername {
				t.Errorf("Expected username %q, got %q", tt.wantUsername, cfg.OpenCode.Username)
			}

			// Verify password
			if cfg.OpenCode.Password != tt.wantPassword {
				t.Errorf("Expected password %q, got %q", tt.wantPassword, cfg.OpenCode.Password)
			}

			// Clean up
			for key := range tt.envVars {
				os.Unsetenv(key)
			}
		})
	}
}

func TestOpenCodeAuthFromConfigFile(t *testing.T) {
	// Test that credentials in config file are loaded correctly
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test-config.yaml")

	configContent := `
server:
  listen: "0.0.0.0:8080"
  auth_token: "test-token"

linear:
  api_key: "test-api-key"

opencode:
  username: "config-user"
  password: "config-pass"

repos:
  - match:
      team: "TEST"
    repo:
      path: "/tmp/test"
      base_branch: "main"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	// Clear env vars to ensure we're testing config file only
	os.Unsetenv("DEVBOXD_OPENCODE_USERNAME")
	os.Unsetenv("DEVBOXD_OPENCODE_PASSWORD")
	os.Unsetenv("OPENCODE_SERVER_USERNAME")
	os.Unsetenv("OPENCODE_SERVER_PASSWORD")

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.OpenCode.Username != "config-user" {
		t.Errorf("Expected username 'config-user', got %q", cfg.OpenCode.Username)
	}

	if cfg.OpenCode.Password != "config-pass" {
		t.Errorf("Expected password 'config-pass', got %q", cfg.OpenCode.Password)
	}
}

func TestOpenCodeAuthEnvVarOverridesConfigFile(t *testing.T) {
	// Test that env vars override config file values
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test-config.yaml")

	configContent := `
server:
  listen: "0.0.0.0:8080"
  auth_token: "test-token"

linear:
  api_key: "test-api-key"

opencode:
  username: "config-user"
  password: "config-pass"

repos:
  - match:
      team: "TEST"
    repo:
      path: "/tmp/test"
      base_branch: "main"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	// Set env vars that should override config file
	os.Setenv("DEVBOXD_OPENCODE_USERNAME", "env-user")
	os.Setenv("DEVBOXD_OPENCODE_PASSWORD", "env-pass")
	defer func() {
		os.Unsetenv("DEVBOXD_OPENCODE_USERNAME")
		os.Unsetenv("DEVBOXD_OPENCODE_PASSWORD")
	}()

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Env vars should take precedence
	if cfg.OpenCode.Username != "env-user" {
		t.Errorf("Expected username 'env-user' (from env), got %q", cfg.OpenCode.Username)
	}

	if cfg.OpenCode.Password != "env-pass" {
		t.Errorf("Expected password 'env-pass' (from env), got %q", cfg.OpenCode.Password)
	}
}
