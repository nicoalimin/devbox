package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/nicoalimin/devbox/internal/api"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
	"github.com/nicoalimin/devbox/internal/tui"
)

func main() {
	configPath := flag.String("config", "devboxd.yaml", "path to configuration file")
	dbPath := flag.String("db", "devboxd.db", "path to database file")
	noTUI := flag.Bool("no-tui", false, "disable terminal UI (log output only)")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Open database
	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create orchestrator
	orchestrator := job.NewOrchestrator(cfg, database)

	// Create server
	server := api.NewServer(cfg, database, orchestrator)
	
	// Decide whether to show TUI
	useTUI := shouldUseTUI(*noTUI)
	
	if useTUI {
		// Initialize log buffer for capturing logs in TUI
		tui.InitGlobalLogBuffer(1000) // 1000 log entries
		
		// Redirect standard logging to buffer
		tui.RedirectStdLog()
		
		// Log startup message to buffer
		tui.LogInfo("devboxd version %s", api.Version)
		tui.LogInfo("Configuration loaded from: %s", *configPath)
		tui.LogInfo("Database: %s", *dbPath)
		
		// Start server in background with custom logger
		go func() {
			if err := server.StartWithLogger(tui.LogInfo); err != nil {
				tui.LogError("Server failed: %v", err)
				os.Exit(1)
			}
		}()
		
		// Give server a moment to start
		time.Sleep(100 * time.Millisecond)
		
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
		fmt.Printf("Database: %s\n", *dbPath)
		fmt.Printf("Starting devboxd server on %s (headless mode)\n", cfg.Server.Listen)
		
		if err := server.Start(); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}
}

// shouldUseTUI determines if the TUI should be enabled
func shouldUseTUI(noTUIFlag bool) bool {
	// Check if --no-tui flag is set
	if noTUIFlag {
		return false
	}
	
	// Check DEVBOX_NO_TUI environment variable
	if os.Getenv("DEVBOX_NO_TUI") != "" {
		return false
	}
	
	// Check if stdout is a TTY
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	
	return true
}
