// Package upgrade builds trusted remote revisions and drains the daemon before
// atomically installing both binaries. State survives the process replacement.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nicoalimin/devbox/internal/config"
)

type Gate interface {
	BeginDrain()
	EndDrain()
	IsDrained() (bool, error)
}

type State struct {
	ID                 string    `json:"id"`
	Phase              string    `json:"phase"`
	FromRevision       string    `json:"fromRevision"`
	TargetRevision     string    `json:"targetRevision,omitempty"`
	PreviousInstanceID string    `json:"previousInstanceId"`
	ProcessID          int       `json:"processId"`
	InstanceID         string    `json:"instanceId,omitempty"`
	Error              string    `json:"error,omitempty"`
	ClientAction       string    `json:"clientAction,omitempty"`
	DatabasePath       string    `json:"databasePath,omitempty"`
	DatabaseBackup     string    `json:"databaseBackup,omitempty"`
	RollbackFailed     bool      `json:"rollbackFailed,omitempty"`
	UpdatedAt          time.Time `json:"updatedAt"`
	Files              []File    `json:"-"`
}

type File struct {
	Target string `json:"target"`
	Backup string `json:"backup,omitempty"`
	Built  string `json:"built"`
}

type record struct {
	State
	Files []File `json:"files,omitempty"`
}

type Options struct {
	Config                                                 config.UpgradeConfig
	StateDir, BinaryPath, ConfigPath, Revision, InstanceID string
	Gate                                                   Gate
	// Restart requests a graceful process replacement by the main goroutine.
	Restart      func() error
	DatabasePath string
	Snapshot     func(string) error
}

type Manager struct {
	options Options
	mu      sync.Mutex
	running bool
	prepare func(context.Context, *State) error
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

var ErrRollbackRestart = errors.New("upgrade rolled back; restart the restored executable")
var ErrRollbackFailed = errors.New("upgrade rollback failed; retained artifacts require recovery")

func New(options Options) *Manager {
	m := &Manager{options: options}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.prepare = m.prepareBuild
	return m
}

func (m *Manager) Enabled() bool { return m.options.Config.Enabled }

// SetRuntime is called during startup, before serving the upgrade endpoint.
func (m *Manager) SetRuntime(gate Gate, snapshot func(string) error, instanceID string) {
	m.options.Gate, m.options.Snapshot = gate, snapshot
	m.options.InstanceID = instanceID
}

func (m *Manager) Status() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

func (m *Manager) load() (State, error) {
	data, err := os.ReadFile(filepath.Join(m.options.StateDir, "upgrade.json"))
	if os.IsNotExist(err) {
		return State{Phase: "idle"}, nil
	}
	if err != nil {
		return State{}, err
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return State{}, err
	}
	r.State.Files = r.Files
	return r.State, nil
}

func (m *Manager) save(s *State) error {
	s.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(record{State: *s, Files: s.Files}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.options.StateDir, 0700); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(m.options.StateDir, "upgrade.json"), data, 0600)
}

func (m *Manager) setPhase(s *State, phase string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Phase = phase
	return m.save(s)
}

func (m *Manager) Start() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.Enabled() {
		return State{}, fmt.Errorf("self-upgrade is disabled; configure upgrade.enabled and upgrade.source_path")
	}
	if m.running {
		return State{}, fmt.Errorf("an upgrade is already in progress")
	}
	if err := m.ctx.Err(); err != nil {
		return State{}, err
	}
	previous, err := m.load()
	if err != nil {
		return State{}, err
	}
	if previous.Phase == "restarting" || previous.Phase == "installing" {
		return State{}, fmt.Errorf("previous upgrade is awaiting restart verification")
	}
	if m.options.Gate == nil || m.options.Restart == nil {
		return State{}, fmt.Errorf("upgrade restart support is unavailable")
	}
	s := State{ID: uuid.NewString(), Phase: "building", FromRevision: m.options.Revision, PreviousInstanceID: m.options.InstanceID, ProcessID: os.Getpid()}
	if err := m.save(&s); err != nil {
		return State{}, err
	}
	m.running = true
	m.done = make(chan struct{})
	accepted := s
	go m.run(s)
	return accepted, nil
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (m *Manager) run(s State) {
	defer close(m.done)
	restarting := false
	defer func() {
		if !restarting {
			m.options.Gate.EndDrain()
		}
		m.mu.Lock()
		m.running = restarting
		m.mu.Unlock()
	}()
	fail := func(err error) {
		s.Error = err.Error()
		if saveErr := m.setPhase(&s, "failed"); saveErr != nil {
			// No successful acknowledgement is written if persistence fails.
			fmt.Fprintf(os.Stderr, "upgrade failure: %v; cannot persist status: %v\n", err, saveErr)
		}
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.options.Config.BuildTimeout)
	err := m.prepare(ctx, &s)
	cancel()
	if err != nil {
		fail(err)
		return
	}
	if s.TargetRevision == m.options.Revision {
		s.InstanceID = m.options.InstanceID
		if err := m.setPhase(&s, "up_to_date"); err != nil {
			fail(err)
		}
		return
	}
	m.options.Gate.BeginDrain()
	if err := m.setPhase(&s, "draining"); err != nil {
		fail(err)
		return
	}
	ctx, cancel = context.WithTimeout(m.ctx, m.options.Config.DrainTimeout)
	err = waitForDrain(ctx, m.options.Gate)
	cancel()
	if err != nil {
		fail(err)
		return
	}
	// Save rollback copies before installing either executable. The plan is
	// durable before the first replacement, covering an interrupted install.
	if err := m.backup(&s); err != nil {
		fail(err)
		return
	}
	if m.options.Snapshot != nil {
		s.DatabasePath = m.options.DatabasePath
		s.DatabaseBackup = filepath.Join(m.options.StateDir, s.ID, "jobs.previous.db")
		if err := m.options.Snapshot(s.DatabaseBackup); err != nil {
			fail(err)
			return
		}
	}
	if err := m.setPhase(&s, "installing"); err != nil {
		fail(err)
		return
	}
	if err := m.ctx.Err(); err != nil {
		fail(err)
		return
	}
	for _, file := range s.Files {
		if err := copyAtomic(file.Built, file.Target); err != nil {
			fail(m.rollback(&s, err))
			return
		}
	}
	if err := m.setPhase(&s, "restarting"); err != nil {
		fail(m.rollback(&s, err))
		return
	}
	if err := m.ctx.Err(); err != nil {
		fail(m.rollback(&s, err))
		return
	}
	if err := m.options.Restart(); err != nil {
		fail(m.rollback(&s, err))
		return
	}
	restarting = true
}

func waitForDrain(ctx context.Context, gate Gate) error {
	for {
		drained, err := gate.IsDrained()
		if err != nil {
			return fmt.Errorf("cannot verify job drain: %w", err)
		}
		if drained {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("upgrade drain timed out; existing jobs continue: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// RecoverStartup runs before accepting work. Interrupted installs roll back;
// a replacement process is only confirmed after its HTTP health check passes.
func (m *Manager) RecoverStartup() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return err
	}
	if s.ProcessID > 0 && s.ProcessID != os.Getpid() && (s.Phase == "building" || s.Phase == "draining" || s.Phase == "installing" || s.Phase == "restarting") {
		if err := syscall.Kill(s.ProcessID, 0); err == nil || err == syscall.EPERM {
			return fmt.Errorf("upgrade is owned by running process %d", s.ProcessID)
		}
	}
	switch s.Phase {
	case "installing":
		s.Error = m.restoreDatabase(&s, m.rollback(&s, fmt.Errorf("upgrade interrupted during installation"))).Error()
		s.Phase = "failed"
		if err := m.save(&s); err != nil {
			return err
		}
		if s.RollbackFailed {
			return fmt.Errorf("%w: %s", ErrRollbackFailed, s.Error)
		}
		return ErrRollbackRestart
	case "restarting":
		if s.TargetRevision == m.options.Revision && s.PreviousInstanceID != m.options.InstanceID {
			if m.options.Gate != nil {
				m.options.Gate.BeginDrain()
			}
			return nil
		}
		s.Error = m.restoreDatabase(&s, m.rollback(&s, fmt.Errorf("replacement started with unexpected revision %s", m.options.Revision))).Error()
		s.Phase = "failed"
		if err := m.save(&s); err != nil {
			return err
		}
		if s.RollbackFailed {
			return fmt.Errorf("%w: %s", ErrRollbackFailed, s.Error)
		}
		return ErrRollbackRestart
	case "building", "draining":
		s.Phase, s.Error = "failed", "upgrade interrupted by server shutdown; retry the upgrade"
		return m.save(&s)
	}
	return nil
}

func (m *Manager) ConfirmHealthy() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return err
	}
	if s.Phase != "restarting" {
		return nil
	}
	if s.TargetRevision != m.options.Revision || s.PreviousInstanceID == m.options.InstanceID {
		return fmt.Errorf("cannot confirm upgrade from the old server")
	}
	s.Phase, s.InstanceID = "complete", m.options.InstanceID
	s.ClientAction = "Server upgraded. Restart long-running clients; on this host use " + filepath.Join(filepath.Dir(m.options.BinaryPath), "devbox") + ". Remote clients should install the matching revision."
	if err := m.save(&s); err != nil {
		return err
	}
	m.options.Gate.EndDrain()
	return nil
}

// RestartFailed restores both executables when exec itself fails.
func (m *Manager) RestartFailed(cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return err
	}
	err = m.restoreDatabase(&s, m.rollback(&s, cause))
	s.Phase, s.Error = "failed", err.Error()
	if saveErr := m.save(&s); saveErr != nil {
		return fmt.Errorf("%v; saving failure: %w", err, saveErr)
	}
	if s.RollbackFailed {
		return fmt.Errorf("%w: %v", ErrRollbackFailed, err)
	}
	return err
}

func (m *Manager) prepareBuild(ctx context.Context, s *State) error {
	cfg := m.options.Config
	remote, err := run(ctx, cfg.SourcePath, "git", "remote", "get-url", cfg.Remote)
	if err != nil {
		return fmt.Errorf("read upgrade remote: %w", err)
	}
	remoteURL := strings.TrimSpace(string(remote))
	// Resolve local remotes relative to the source checkout, not the build dir.
	if !strings.Contains(remoteURL, ":") && !filepath.IsAbs(remoteURL) {
		remoteURL = filepath.Join(cfg.SourcePath, remoteURL)
	}
	releaseDir := filepath.Join(m.options.StateDir, s.ID)
	checkout := filepath.Join(releaseDir, "source")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(checkout) // Owned temporary checkout; binaries/backups remain.
	for _, args := range [][]string{{"init"}, {"remote", "add", "origin", remoteURL}, {"fetch", "--depth=1", "origin", "refs/heads/" + cfg.Branch}, {"checkout", "--detach", "FETCH_HEAD"}} {
		if _, err := run(ctx, checkout, "git", args...); err != nil {
			return fmt.Errorf("fetch trusted upgrade revision: %w", err)
		}
	}
	revision, err := run(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	s.TargetRevision = strings.TrimSpace(string(revision))
	if err := m.setPhase(s, "building"); err != nil {
		return err
	}
	if s.TargetRevision == m.options.Revision {
		return nil
	}
	for _, name := range []string{"devboxd", "devbox"} {
		built := filepath.Join(releaseDir, name)
		flags := "-X github.com/nicoalimin/devbox/internal/buildinfo.Revision=" + s.TargetRevision
		if _, err := run(ctx, checkout, "go", "build", "-buildvcs=false", "-ldflags", flags, "-o", built, "./cmd/"+name); err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
		s.Files = append(s.Files, File{Target: filepath.Join(filepath.Dir(m.options.BinaryPath), name), Built: built})
	}
	// The server might have a custom executable name.
	s.Files[0].Target = m.options.BinaryPath
	if _, err := run(ctx, releaseDir, s.Files[0].Built, "--check-config", "--config", m.options.ConfigPath); err != nil {
		return fmt.Errorf("candidate config preflight: %w", err)
	}
	return nil
}
