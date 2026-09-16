package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/nicoalimin/devbox/internal/api"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
	"github.com/nicoalimin/devbox/internal/tui"
)

func main() {
	configPath := flag.String("config", "devboxd.yaml", "path to configuration file")
	dbPath := flag.String("db", "", "path to database file (overrides config file)")
	noTUI := flag.Bool("no-tui", false, "disable terminal UI (log output only)")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Determine database path: CLI flag > config file > default
	finalDBPath := *dbPath
	if finalDBPath == "" {
		finalDBPath = cfg.GetDBPath()
	}

	// Ensure database directory exists
	dbDir := filepath.Dir(finalDBPath)
	if dbDir != "." && dbDir != "" {
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			log.Fatalf("Failed to create database directory: %v", err)
		}
	}

	// Open database
	database, err := db.Open(finalDBPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create orchestrator
	orchestrator := job.NewOrchestrator(cfg, database)

	// The TUI is the default. Headless mode must be explicitly requested.
	useTUI := shouldUseTUI(*noTUI)

	// Bind before redirecting logs or entering the TUI so startup failures are
	// always reported to the invoking terminal.
	var listener net.Listener
	if useTUI {
		listener, err = net.Listen("tcp", cfg.Server.Listen)
		if err != nil {
			log.Fatalf("Server failed: %v", err)
		}
		defer listener.Close()
	}

	if useTUI {
		// Capture startup logs too, including messages emitted while resuming jobs.
		tui.InitGlobalLogBuffer(1000)
		tui.RedirectStdLog()
	}

	// Resume any in-flight jobs from previous run
	if err := orchestrator.ResumeInFlightJobs(); err != nil {
		log.Printf("Warning: Failed to resume in-flight jobs: %v", err)
		// Don't fail startup, just log the warning
	}

	// Start the periodic GitHub reconciler for stuck pr_open jobs (UTA-68).
	// It no-ops immediately when disabled in config.
	go orchestrator.StartReconciler()

	// Create server
	server := api.NewServer(cfg, database, orchestrator)

	if useTUI {
		// Log startup message to buffer
		tui.LogInfo("devboxd version %s", api.Version)
		tui.LogInfo("Configuration loaded from: %s", *configPath)
		tui.LogInfo("Database: %s", finalDBPath)

		// Start server in background with custom logger
		go func() {
			if err := server.ServeWithLogger(listener, tui.LogInfo); err != nil {
				tui.LogError("Server failed: %v", err)
			}
		}()

		// Run TUI in foreground
		if err := tui.Run(cfg, database); err != nil {
			// Restore standard logging before exiting
			tui.RestoreStdLog(os.Stdout)
			log.Fatalf("TUI failed: %v", err)
		}

		// Restore standard logging after TUI exits
		tui.RestoreStdLog(os.Stdout)
	} else {
		// Traditional headless mode - log to stdout
		fmt.Printf("devboxd version %s\n", api.Version)
		fmt.Printf("Configuration loaded from: %s\n", *configPath)
		fmt.Printf("Database: %s\n", finalDBPath)
		fmt.Printf("Starting devboxd server on %s (headless mode)\n", cfg.Server.Listen)

		if err := server.Start(); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}
}

// shouldUseTUI determines if the TUI should be enabled. The TUI is the default;
// callers must explicitly opt out when running as a headless service.
func shouldUseTUI(noTUIFlag bool) bool {
	// Check if --no-tui flag is set
	if noTUIFlag {
		return false
	}

	// Check DEVBOX_NO_TUI environment variable
	if os.Getenv("DEVBOX_NO_TUI") != "" {
		return false
	}

	return true
}
