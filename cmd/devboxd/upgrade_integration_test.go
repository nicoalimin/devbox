package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/pkg/client"
)

// Opt-in because this builds two real daemon revisions. The remote is a local
// bare repo; no production server, credentials, worktree, or remote is touched.
func TestSelfUpgradeProcess(t *testing.T) {
	if os.Getenv("DEVBOX_UPGRADE_INTEGRATION") != "1" {
		t.Skip("set DEVBOX_UPGRADE_INTEGRATION=1 for real build/restart verification")
	}
	t.Run("healthy_restart", func(t *testing.T) { testSelfUpgradeProcess(t, false) })
	t.Run("migration_rollback", func(t *testing.T) { testSelfUpgradeProcess(t, true) })
}

func testSelfUpgradeProcess(t *testing.T, failMigration bool) {
	dir := t.TempDir()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	command := func(cwd, name string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = cwd
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	expected := command(root, "git", "rev-parse", "HEAD")
	remote, source := filepath.Join(dir, "remote.git"), filepath.Join(dir, "source")
	command(dir, "git", "clone", "--bare", root, remote)
	command(remote, "git", "update-ref", "refs/heads/main", expected)
	command(dir, "git", "clone", "--branch", "main", remote, source)
	serverBinary := filepath.Join(dir, "bin", "devboxd")
	command(source, "go", "build", "-buildvcs=false", "-ldflags", "-X github.com/nicoalimin/devbox/internal/buildinfo.Revision=bootstrap", "-o", serverBinary, "./cmd/devboxd")
	command(source, "go", "build", "-buildvcs=false", "-ldflags", "-X github.com/nicoalimin/devbox/internal/buildinfo.Revision=bootstrap", "-o", filepath.Join(dir, "bin", "devbox"), "./cmd/devbox")
	if failMigration {
		migration := filepath.Join(source, "internal", "db", "migrations", "000002_upgrade_failure.up.sql")
		if err := os.WriteFile(migration, []byte("CREATE TABLE failed_upgrade (id INTEGER); INVALID SQL;"), 0644); err != nil {
			t.Fatal(err)
		}
		command(source, "git", "config", "user.name", "Upgrade Test")
		command(source, "git", "config", "user.email", "upgrade-test@example.com")
		command(source, "git", "add", migration)
		command(source, "git", "-c", "commit.gpgsign=false", "commit", "-m", "Intentionally fail candidate migration")
		command(source, "git", "push", "origin", "main")
		expected = command(source, "git", "rev-parse", "HEAD")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	configPath := filepath.Join(dir, "devboxd.yaml")
	body := fmt.Sprintf("server:\n  listen: %q\n  auth_token: test-token\nlinear:\n  api_key: test-key\nrepos:\n  - match:\n      team: TEST\n    repo:\n      path: %q\nreconciler:\n  enabled: false\nupgrade:\n  enabled: true\n  source_path: %q\n  build_timeout: 2m\n  drain_timeout: 10s\n", address, source, source)
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "state", "jobs.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateJob(&db.Job{ID: "preserved-job", LinearIssueID: "TEST-1", State: db.StateDone, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	database.Close()
	logs, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	process := exec.Command(serverBinary, "--config", configPath, "--db", dbPath, "--no-tui")
	process.Stdout, process.Stderr = logs, logs
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		process.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			process.Process.Kill()
			<-done
		}
		if t.Failed() {
			data, _ := os.ReadFile(logs.Name())
			t.Log(string(data))
		}
	}()
	c := client.NewClient("http://"+address, "test-token")
	deadline := time.Now().Add(10 * time.Second)
	var initial *client.HealthResponse
	for time.Now().Before(deadline) {
		initial, err = c.Health()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if initial.Revision != "bootstrap" {
		t.Fatal(initial.Revision)
	}
	accepted, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	complete, err := c.WaitForUpgrade(ctx, accepted.ID, 50*time.Millisecond, nil)
	if failMigration {
		if err == nil || complete == nil || complete.Phase != "failed" {
			t.Fatalf("bad migration did not fail and recover: %+v %v", complete, err)
		}
		health, healthErr := c.Health()
		if healthErr != nil || health.Revision != "bootstrap" {
			t.Fatalf("old server not restored: %+v %v", health, healthErr)
		}
		if _, err := os.Stat(filepath.Join(dir, "state", "upgrades", accepted.ID, "jobs.previous.db.failed")); err != nil {
			t.Fatalf("failed migration not preserved: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if !failMigration && (complete.TargetRevision != expected || complete.InstanceID == initial.InstanceID) {
		t.Fatalf("restart mismatch: %+v", complete)
	}
	if err := process.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("exec did not preserve daemon PID: %v", err)
	}
	version := command(dir, filepath.Join(dir, "bin", "devbox"), "version")
	expectedClient := expected
	if failMigration {
		expectedClient = "bootstrap"
	}
	if !strings.Contains(version, expectedClient) {
		t.Fatalf("client not updated: %s", version)
	}
	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	job, err := database.GetJob("preserved-job")
	if err != nil || job == nil || job.State != db.StateDone {
		t.Fatalf("job corrupted across restart: %+v %v", job, err)
	}
	t.Logf("Verified PID %d upgrade phase %s targeting %s; client at %s and persisted job preserved", process.Process.Pid, complete.Phase, expected, expectedClient)
}
