package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/config"
)

type testGate struct {
	draining atomic.Bool
	busy     atomic.Bool
}

func (g *testGate) BeginDrain()              { g.draining.Store(true) }
func (g *testGate) EndDrain()                { g.draining.Store(false) }
func (g *testGate) IsDrained() (bool, error) { return !g.busy.Load(), nil }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (*Manager, *testGate, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	gate, restarts := &testGate{}, &atomic.Int32{}
	m := New(Options{Config: config.UpgradeConfig{Enabled: true, BuildTimeout: time.Second, DrainTimeout: time.Second}, StateDir: filepath.Join(dir, "state"), BinaryPath: filepath.Join(dir, "bin", "devboxd"), Revision: "old", InstanceID: "old-instance", Gate: gate, Restart: func() error { restarts.Add(1); return nil }})
	t.Cleanup(m.Close)
	m.prepare = func(ctx context.Context, s *State) error {
		s.TargetRevision = "new"
		for _, name := range []string{"devboxd", "devbox"} {
			target, built := filepath.Join(dir, "bin", name), filepath.Join(dir, "state", s.ID, name)
			writeFile(t, target, "old-"+name)
			writeFile(t, built, "new-"+name)
			s.Files = append(s.Files, File{Target: target, Built: built})
		}
		return nil
	}
	return m, gate, restarts
}

func awaitPhase(t *testing.T, m *Manager, phase string) State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := m.Status()
		if err != nil {
			t.Fatal(err)
		}
		if s.Phase == phase {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	s, _ := m.Status()
	t.Fatalf("wanted %s, got %+v", phase, s)
	return s
}

func TestUpgradeDrainsInstallsAndRequiresNewHealthyInstance(t *testing.T) {
	m, gate, restarts := fixture(t)
	gate.busy.Store(true)
	accepted, err := m.Start()
	if err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, m, "draining")
	if !gate.draining.Load() || restarts.Load() != 0 {
		t.Fatal("restart before drain")
	}
	if _, err := m.Start(); err == nil {
		t.Fatal("duplicate upgrade accepted")
	}
	gate.busy.Store(false)
	s := awaitPhase(t, m, "restarting")
	m.Close()
	if restarts.Load() != 1 {
		t.Fatal("restart not requested")
	}
	if err := m.ConfirmHealthy(); err == nil {
		t.Fatal("old process confirmed restart")
	}
	for _, f := range s.Files {
		data, _ := os.ReadFile(f.Target)
		if string(data) != "new-"+filepath.Base(f.Target) {
			t.Fatal("binary not replaced")
		}
		data, _ = os.ReadFile(f.Backup)
		if string(data) != "old-"+filepath.Base(f.Target) {
			t.Fatal("old binary not backed up")
		}
	}
	options := m.options
	options.Revision, options.InstanceID = "new", "new-instance"
	replacement := New(options)
	defer replacement.Close()
	if err := replacement.RecoverStartup(); err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Start(); err == nil {
		t.Fatal("new upgrade replaced unconfirmed state")
	}
	if err := replacement.ConfirmHealthy(); err != nil {
		t.Fatal(err)
	}
	s, _ = replacement.Status()
	if s.ID != accepted.ID || s.Phase != "complete" || s.InstanceID != "new-instance" || gate.draining.Load() || s.ClientAction == "" {
		t.Fatalf("bad completion: %+v", s)
	}
}

func TestUpgradeFailuresKeepCurrentService(t *testing.T) {
	for _, scenario := range []string{"build", "drain", "install", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			m, gate, restarts := fixture(t)
			prepare := m.prepare
			m.prepare = func(ctx context.Context, s *State) error {
				if err := prepare(ctx, s); err != nil {
					return err
				}
				if scenario == "build" {
					return errors.New("build failed")
				}
				if scenario == "install" {
					return os.Remove(s.Files[1].Built)
				}
				return nil
			}
			if scenario == "drain" {
				gate.busy.Store(true)
				m.options.Config.DrainTimeout = 10 * time.Millisecond
			}
			if scenario == "restart" {
				m.options.Restart = func() error { return errors.New("restart unavailable") }
			}
			if _, err := m.Start(); err != nil {
				t.Fatal(err)
			}
			s := awaitPhase(t, m, "failed")
			m.Close()
			if gate.draining.Load() || restarts.Load() != 0 || s.Error == "" {
				t.Fatalf("bad failure: %+v", s)
			}
			for _, f := range s.Files {
				data, _ := os.ReadFile(f.Target)
				if string(data) != "old-"+filepath.Base(f.Target) {
					t.Fatalf("old binary lost: %s", data)
				}
			}
		})
	}
}

func TestInterruptedInstallRestoresBothBinaries(t *testing.T) {
	m, _, _ := fixture(t)
	s := State{ID: "interrupted", Phase: "installing", TargetRevision: "new"}
	if err := m.prepare(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	if err := m.backup(&s); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&s); err != nil {
		t.Fatal(err)
	}
	if err := copyAtomic(s.Files[0].Built, s.Files[0].Target); err != nil {
		t.Fatal(err)
	}
	if err := m.RecoverStartup(); !errors.Is(err, ErrRollbackRestart) {
		t.Fatalf("unexpected recovery: %v", err)
	}
	for _, f := range s.Files {
		data, _ := os.ReadFile(f.Target)
		if string(data) != "old-"+filepath.Base(f.Target) {
			t.Fatal("rollback lost original")
		}
	}
}

func TestShutdownCancelsBuild(t *testing.T) {
	m, _, restarts := fixture(t)
	m.prepare = func(ctx context.Context, s *State) error { <-ctx.Done(); return ctx.Err() }
	if _, err := m.Start(); err != nil {
		t.Fatal(err)
	}
	m.Close()
	s, _ := m.Status()
	if s.Phase != "failed" || restarts.Load() != 0 {
		t.Fatalf("upgrade survived shutdown: %+v", s)
	}
}

func TestAlreadyCurrentDoesNotRestart(t *testing.T) {
	m, _, restarts := fixture(t)
	m.prepare = func(ctx context.Context, s *State) error { s.TargetRevision = "old"; return nil }
	if _, err := m.Start(); err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, m, "up_to_date")
	if restarts.Load() != 0 {
		t.Fatal("unnecessary restart")
	}
}

func TestFailedStartupRestoresDatabaseSnapshot(t *testing.T) {
	m, _, _ := fixture(t)
	databasePath := filepath.Join(t.TempDir(), "jobs.db")
	writeFile(t, databasePath, "original database")
	m.options.DatabasePath = databasePath
	m.options.Snapshot = func(path string) error { return copyAtomic(databasePath, path) }
	if _, err := m.Start(); err != nil {
		t.Fatal(err)
	}
	s := awaitPhase(t, m, "restarting")
	m.Close()
	writeFile(t, databasePath, "failed migration")
	writeFile(t, databasePath+"-wal", "failed WAL")
	if err := m.RestartFailed(errors.New("migration failed")); err == nil {
		t.Fatal("lost original error")
	}
	data, _ := os.ReadFile(databasePath)
	if string(data) != "original database" {
		t.Fatal("database was not restored")
	}
	data, _ = os.ReadFile(s.DatabaseBackup + ".failed")
	if string(data) != "failed migration" {
		t.Fatal("failed migration output was lost")
	}
	if _, err := os.Stat(databasePath + "-wal"); !os.IsNotExist(err) {
		t.Fatal("failed WAL left beside restored database")
	}
}
