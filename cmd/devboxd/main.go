package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nicoalimin/devbox/internal/api"
	"github.com/nicoalimin/devbox/internal/buildinfo"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
	"github.com/nicoalimin/devbox/internal/tui"
	"github.com/nicoalimin/devbox/internal/upgrade"
)

func main() {
	configPath := flag.String("config", "devboxd.yaml", "path to configuration file")
	dbPath := flag.String("db", "", "path to database file (overrides config file)")
	noTUI := flag.Bool("no-tui", false, "disable terminal UI (log output only)")
	version := flag.Bool("version", false, "print version and build revision")
	checkConfig := flag.Bool("check-config", false, "validate configuration without starting the server")
	flag.Parse()
	if *version {
		fmt.Printf("devboxd %s (%s)\n", buildinfo.Version, buildinfo.Revision)
		return
	}
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	if *checkConfig {
		return
	}
	finalDBPath := *dbPath
	if finalDBPath == "" {
		finalDBPath = cfg.GetDBPath()
	}
	if err := serve(cfg, *configPath, finalDBPath, shouldUseTUI(*noTUI)); err != nil {
		if recoveryErr := recoverFailedRestart(finalDBPath, err); recoveryErr != nil {
			log.Printf("Upgrade startup recovery: %v", recoveryErr)
		}
		log.Fatal(err)
	}
}

func serve(cfg *config.Config, configPath, dbPath string, useTUI bool) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	dbPath, err = filepath.Abs(dbPath)
	if err != nil {
		return err
	}
	if cfg.Upgrade.SourcePath != "" {
		cfg.Upgrade.SourcePath, err = filepath.Abs(cfg.Upgrade.SourcePath)
		if err != nil {
			return err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	// Bind before migrations/UI so a second daemon cannot start background work.
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("server bind failed: %w", err)
	}
	defer listener.Close()
	restart := make(chan struct{}, 1)
	upgrader := upgrade.New(upgrade.Options{
		Config: cfg.Upgrade, StateDir: filepath.Join(filepath.Dir(dbPath), "upgrades"),
		BinaryPath: executable, ConfigPath: configPath, Revision: buildinfo.Revision,
		DatabasePath: dbPath,
		Restart:      func() error { restart <- struct{}{}; return nil },
	})
	defer upgrader.Close()
	if err := upgrader.RecoverStartup(); err != nil {
		if errors.Is(err, upgrade.ErrRollbackRestart) {
			listener.Close()
			return syscall.Exec(executable, os.Args, os.Environ())
		}
		return fmt.Errorf("recover upgrade state: %w", err)
	}
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database/migrate: %w", err)
	}
	defer database.Close()
	orchestrator := job.NewOrchestrator(cfg, database)
	server := api.NewServer(cfg, database, orchestrator)
	upgrader.SetRuntime(orchestrator, database.Backup, server.InstanceID())
	if state, err := upgrader.Status(); err != nil {
		return err
	} else if state.Phase == "restarting" {
		orchestrator.BeginDrain()
	}
	server.SetUpgrader(upgrader)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	uiCtx, stopUI := context.WithCancel(context.Background())
	defer stopUI()
	var logger func(string, ...interface{})
	if useTUI {
		tui.InitGlobalLogBuffer(1000)
		tui.RedirectStdLog()
		defer tui.RestoreStdLog(os.Stdout)
		logger = tui.LogInfo
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeWithLogger(listener, logger) }()
	if err := probeHealth(listener.Addr().String(), server.InstanceID()); err != nil {
		return err
	}
	if err := upgrader.ConfirmHealthy(); err != nil {
		return fmt.Errorf("confirm upgrade health: %w", err)
	}
	if err := orchestrator.ResumeInFlightJobs(); err != nil {
		log.Printf("Failed to resume jobs: %v", err)
	}
	go orchestrator.StartReconciler()
	log.Printf("devboxd %s (%s), instance %s, config %s, database %s", buildinfo.Version, buildinfo.Revision, server.InstanceID(), configPath, dbPath)
	var uiDone chan error
	if useTUI {
		uiDone = make(chan error, 1)
		go func() { uiDone <- tui.RunContextWithStartedAt(uiCtx, cfg, database, server.StartedAt()) }()
	}
	restarting := false
	var serveErr error
	select {
	case <-restart:
		restarting = true
	case <-ctx.Done():
	case serveErr = <-serverDone:
	case serveErr = <-uiDone:
		uiDone = nil
	}
	stopUI()
	if uiDone != nil {
		<-uiDone
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("HTTP shutdown: %w", err)
	}
	upgrader.Close()
	if err := database.Close(); err != nil {
		return fmt.Errorf("database close: %w", err)
	}
	if restarting {
		// exec preserves PID, environment, arguments, cwd, and terminal. The
		// TUI is stopped and SQLite is closed before replacing the process.
		if useTUI {
			tui.RestoreStdLog(os.Stdout)
		}
		return replaceProcess(upgrader, executable, os.Args, os.Environ(), syscall.Exec)
	}
	return serveErr
}

// A candidate that cannot bind, migrate, or serve healthy HTTP rolls back the
// executables and the pre-migration database snapshot before restarting again.
func recoverFailedRestart(dbPath string, cause error) error {
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	manager := upgrade.New(upgrade.Options{StateDir: filepath.Join(filepath.Dir(absolute), "upgrades")})
	defer manager.Close()
	state, err := manager.Status()
	if err != nil {
		return err
	}
	if state.Phase != "restarting" || state.ProcessID != os.Getpid() {
		return nil
	}
	recoveryErr := manager.RestartFailed(cause)
	if errors.Is(recoveryErr, upgrade.ErrRollbackFailed) {
		return recoveryErr
	}
	if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("%v; restart restored daemon: %w", recoveryErr, err)
	}
	return nil
}

func replaceProcess(manager *upgrade.Manager, executable string, args, env []string, execFn func(string, []string, []string) error) error {
	if err := execFn(executable, args, env); err != nil {
		recoveryErr := manager.RestartFailed(fmt.Errorf("exec upgraded server: %w", err))
		if errors.Is(recoveryErr, upgrade.ErrRollbackFailed) {
			return recoveryErr
		}
		if fallbackErr := execFn(executable, args, env); fallbackErr != nil {
			return fmt.Errorf("%v; restart previous binary: %w", recoveryErr, fallbackErr)
		}
	}
	return nil
}

func probeHealth(address, instanceID string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health")
		if err == nil {
			var health struct {
				Healthy    bool
				InstanceID string `json:"instanceId"`
				Revision   string
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&health)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && health.Healthy && health.InstanceID == instanceID && health.Revision == buildinfo.Revision {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("new server failed its local HTTP health check")
}

func shouldUseTUI(noTUIFlag bool) bool {
	return !noTUIFlag && os.Getenv("DEVBOX_NO_TUI") == ""
}
