package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeReconcilerTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}
	return path
}

const reconcilerMinimalConfig = `
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

func TestReconcilerDefaults(t *testing.T) {
	path := writeReconcilerTestConfig(t, reconcilerMinimalConfig)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if !cfg.Reconciler.Enabled {
		t.Error("Expected reconciler to be enabled by default")
	}
	if cfg.Reconciler.Interval != 2*time.Minute {
		t.Errorf("Expected default interval 2m, got %s", cfg.Reconciler.Interval)
	}
	if !cfg.ReconcilerEnabled() {
		t.Error("Expected ReconcilerEnabled() to be true by default")
	}
	if cfg.ReconcilerInterval() != 2*time.Minute {
		t.Errorf("Expected ReconcilerInterval() 2m, got %s", cfg.ReconcilerInterval())
	}
}

func TestReconcilerDisabled(t *testing.T) {
	path := writeReconcilerTestConfig(t, reconcilerMinimalConfig+`
reconciler:
  enabled: false
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Reconciler.Enabled {
		t.Error("Expected reconciler to be disabled")
	}
	if cfg.ReconcilerEnabled() {
		t.Error("Expected ReconcilerEnabled() to be false")
	}
	// Interval still falls back to the default when unset.
	if cfg.ReconcilerInterval() != DefaultReconcilerInterval {
		t.Errorf("Expected default interval, got %s", cfg.ReconcilerInterval())
	}
}

func TestReconcilerCustomInterval(t *testing.T) {
	path := writeReconcilerTestConfig(t, reconcilerMinimalConfig+`
reconciler:
  enabled: true
  interval: "5m"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if !cfg.Reconciler.Enabled {
		t.Error("Expected reconciler to be enabled")
	}
	if cfg.Reconciler.Interval != 5*time.Minute {
		t.Errorf("Expected interval 5m, got %s", cfg.Reconciler.Interval)
	}
	if cfg.ReconcilerInterval() != 5*time.Minute {
		t.Errorf("Expected ReconcilerInterval() 5m, got %s", cfg.ReconcilerInterval())
	}
}

func TestReconcilerIntervalFallback(t *testing.T) {
	// Programmatically built configs (zero value) fall back to the default interval.
	cfg := &Config{}
	if cfg.ReconcilerInterval() != DefaultReconcilerInterval {
		t.Errorf("Expected default interval, got %s", cfg.ReconcilerInterval())
	}
	if DefaultReconcilerInterval != 2*time.Minute {
		t.Errorf("Expected DefaultReconcilerInterval 2m, got %s", DefaultReconcilerInterval)
	}
}
