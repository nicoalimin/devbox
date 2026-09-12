package job

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
)

func TestSingleFlightEnforcement(t *testing.T) {
	// Setup
	dbPath := "test_single_flight.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		Linear: config.LinearConfig{
			APIKey: "test-key",
		},
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://localhost:3000",
			Timeout: 30 * time.Minute,
		},
		Repos: []config.RepoConfig{
			{
				Match: config.RepoMatch{Team: "ENG"},
				Repo:  config.RepoInfo{Path: "/tmp/repo", BaseBranch: "main"},
			},
		},
		Queue: config.QueueConfig{
			Enabled:  false,
			MaxDepth: 0,
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create first job manually
	job1 := &db.Job{
		ID:            "job-1",
		LinearIssueID: "ENG-123",
		State:         db.StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(job1); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Try to create second job (should fail because job1 is busy)
	_, err = orch.CreateJob("ENG-124")
	if err == nil {
		t.Error("Expected error when creating job while busy, got nil")
	}

	expectedError := fmt.Sprintf("server busy with job %s (state: %s)", job1.ID, job1.State)
	if err.Error() != expectedError {
		t.Errorf("Expected error '%s', got '%s'", expectedError, err.Error())
	}

	// Mark job1 as done
	now := time.Now()
	job1.State = db.StateDone
	job1.CompletedAt = &now
	if err := database.UpdateJob(job1); err != nil {
		t.Fatalf("Failed to update job: %v", err)
	}

	// Now should be able to create job2
	job2, err := orch.CreateJob("ENG-124")
	if err != nil {
		t.Errorf("Expected success when creating job after previous done, got error: %v", err)
	}

	if job2 == nil {
		t.Error("Expected job to be created, got nil")
	}

	if job2 != nil && job2.State != db.StateFetching {
		t.Errorf("Expected new job to be in fetching state, got %s", job2.State)
	}
}

func TestCancelJob(t *testing.T) {
	// Setup
	dbPath := "test_cancel_job.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		Linear: config.LinearConfig{
			APIKey: "test-key",
		},
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://localhost:3000",
		},
		Repos: []config.RepoConfig{
			{
				Match: config.RepoMatch{Team: "ENG"},
				Repo:  config.RepoInfo{Path: "/tmp/repo", BaseBranch: "main"},
			},
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job
	job := &db.Job{
		ID:            "job-1",
		LinearIssueID: "ENG-123",
		State:         db.StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Cancel the job
	if err := orch.CancelJob(job.ID); err != nil {
		t.Fatalf("Failed to cancel job: %v", err)
	}

	// Verify job is cancelled
	cancelled, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if cancelled.State != db.StateCancelled {
		t.Errorf("Expected state cancelled, got %s", cancelled.State)
	}

	if cancelled.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set")
	}

	// Try to cancel already cancelled job (should fail)
	err = orch.CancelJob(job.ID)
	if err == nil {
		t.Error("Expected error when cancelling already terminal job, got nil")
	}
}

func TestReplyToJob(t *testing.T) {
	// Setup
	dbPath := "test_reply_job.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		Linear: config.LinearConfig{
			APIKey: "test-key",
		},
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://localhost:3000",
		},
		Repos: []config.RepoConfig{
			{
				Match: config.RepoMatch{Team: "ENG"},
				Repo:  config.RepoInfo{Path: "/tmp/repo", BaseBranch: "main"},
			},
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a blocked job
	job := &db.Job{
		ID:               "job-1",
		LinearIssueID:    "ENG-123",
		State:            db.StateBlocked,
		BlockerReason:    "Need clarification on button color",
		OpenCodeSessionID: "session-123",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Note: We can't fully test reply without a real OpenCode server
	// But we can test the state transition

	// Try to reply to non-blocked job (should fail)
	job.State = db.StateCoding
	if err := database.UpdateJob(job); err != nil {
		t.Fatalf("Failed to update job: %v", err)
	}

	err = orch.ReplyToJob(job.ID, "Use blue")
	if err == nil {
		t.Error("Expected error when replying to non-blocked job, got nil")
	}

	// Put it back in blocked state
	job.State = db.StateBlocked
	if err := database.UpdateJob(job); err != nil {
		t.Fatalf("Failed to update job: %v", err)
	}

	// This will fail because OpenCode isn't running, but we can check
	// that it attempts to send the message
	_ = orch.ReplyToJob(job.ID, "Use blue")
	// We'd need to mock OpenCode to test success case
}
