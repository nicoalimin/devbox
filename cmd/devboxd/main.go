package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/nicoalimin/devbox/internal/api"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
)

func main() {
	configPath := flag.String("config", "devboxd.yaml", "path to configuration file")
	dbPath := flag.String("db", "devboxd.db", "path to database file")
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

	// Create and start server
	server := api.NewServer(cfg, database, orchestrator)
	
	fmt.Printf("devboxd version %s\n", api.Version)
	fmt.Printf("Configuration loaded from: %s\n", *configPath)
	fmt.Printf("Database: %s\n", *dbPath)
	
	if err := server.Start(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
