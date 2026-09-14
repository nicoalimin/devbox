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
	_, err = orch.CreateJob("ENG-124", "")
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
	job2, err := orch.CreateJob("ENG-124", "")
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

func TestResumeInFlightJobs_SkipsTerminalStates(t *testing.T) {
	// Setup
	dbPath := "test_resume_terminal.db"
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
	}

	orch := NewOrchestrator(cfg, database)

	// Create jobs in terminal states
	now := time.Now()
	doneJob := &db.Job{
		ID:            "job-done",
		LinearIssueID: "ENG-100",
		State:         db.StateDone,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		CompletedAt:   &now,
	}
	failedJob := &db.Job{
		ID:            "job-failed",
		LinearIssueID: "ENG-101",
		State:         db.StateFailed,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		CompletedAt:   &now,
	}
	cancelledJob := &db.Job{
		ID:            "job-cancelled",
		LinearIssueID: "ENG-102",
		State:         db.StateCancelled,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		CompletedAt:   &now,
	}

	if err := database.CreateJob(doneJob); err != nil {
		t.Fatalf("Failed to create done job: %v", err)
	}
	if err := database.CreateJob(failedJob); err != nil {
		t.Fatalf("Failed to create failed job: %v", err)
	}
	if err := database.CreateJob(cancelledJob); err != nil {
		t.Fatalf("Failed to create cancelled job: %v", err)
	}

	// Resume in-flight jobs (should skip all terminal jobs)
	if err := orch.ResumeInFlightJobs(); err != nil {
		t.Fatalf("ResumeInFlightJobs failed: %v", err)
	}

	// Verify no jobs are marked as active
	if orch.isJobActive("job-done") {
		t.Error("Terminal job should not be marked active")
	}
	if orch.isJobActive("job-failed") {
		t.Error("Terminal job should not be marked active")
	}
	if orch.isJobActive("job-cancelled") {
		t.Error("Terminal job should not be marked active")
	}
}

func TestResumeInFlightJobs_SkipsBlockedState(t *testing.T) {
	// Setup
	dbPath := "test_resume_blocked.db"
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
	}

	orch := NewOrchestrator(cfg, database)

	// Create a blocked job (not IsBusy())
	blockedJob := &db.Job{
		ID:            "job-blocked",
		LinearIssueID: "ENG-200",
		State:         db.StateBlocked,
		BlockerReason: "Needs clarification",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	if err := database.CreateJob(blockedJob); err != nil {
		t.Fatalf("Failed to create blocked job: %v", err)
	}

	// Resume in-flight jobs (should skip blocked job)
	if err := orch.ResumeInFlightJobs(); err != nil {
		t.Fatalf("ResumeInFlightJobs failed: %v", err)
	}

	// Verify blocked job is not marked as active
	if orch.isJobActive("job-blocked") {
		t.Error("Blocked job should not be marked active")
	}
}

func TestResumeInFlightJobs_HandlesInFlightJobs(t *testing.T) {
	// Setup
	dbPath := "test_resume_inflight.db"
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
	}

	orch := NewOrchestrator(cfg, database)

	// Create in-flight jobs (without OpenCode session ID to force quick failure)
	codingJob := &db.Job{
		ID:            "job-coding",
		LinearIssueID: "ENG-300",
		State:         db.StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		// Missing OpenCodeSessionID - should fail gracefully
	}
	reviewingJob := &db.Job{
		ID:            "job-reviewing",
		LinearIssueID: "ENG-301",
		State:         db.StateReviewing,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
		// Missing OpenCodeSessionID - should fail gracefully
	}

	if err := database.CreateJob(codingJob); err != nil {
		t.Fatalf("Failed to create coding job: %v", err)
	}
	if err := database.CreateJob(reviewingJob); err != nil {
		t.Fatalf("Failed to create reviewing job: %v", err)
	}

	// Resume in-flight jobs
	if err := orch.ResumeInFlightJobs(); err != nil {
		t.Fatalf("ResumeInFlightJobs failed: %v", err)
	}

	// Give goroutines a moment to start
	time.Sleep(100 * time.Millisecond)

	// Verify jobs were marked as active (they should be processing)
	// Note: They will fail quickly due to missing session IDs, but should have been attempted
	
	// Check that jobs transitioned to failed state due to missing session ID
	codingJobAfter, err := database.GetJob("job-coding")
	if err != nil {
		t.Fatalf("Failed to get coding job after resume: %v", err)
	}
	if codingJobAfter.State != db.StateFailed {
		t.Errorf("Expected coding job to fail due to missing session ID, got state %s", codingJobAfter.State)
	}

	reviewingJobAfter, err := database.GetJob("job-reviewing")
	if err != nil {
		t.Fatalf("Failed to get reviewing job after resume: %v", err)
	}
	if reviewingJobAfter.State != db.StateFailed {
		t.Errorf("Expected reviewing job to fail due to missing session ID, got state %s", reviewingJobAfter.State)
	}
}

func TestJobActiveTracking(t *testing.T) {
	// Setup
	dbPath := "test_active_tracking.db"
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
	}

	orch := NewOrchestrator(cfg, database)

	// Test marking jobs as active/inactive
	jobID := "test-job-1"

	if orch.isJobActive(jobID) {
		t.Error("Job should not be active initially")
	}

	orch.markJobActive(jobID)
	if !orch.isJobActive(jobID) {
		t.Error("Job should be active after marking")
	}

	orch.markJobInactive(jobID)
	if orch.isJobActive(jobID) {
		t.Error("Job should not be active after unmarking")
	}
}

func TestResumeInFlightJobs_SkipsAlreadyActive(t *testing.T) {
	// Setup
	dbPath := "test_resume_already_active.db"
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
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job and mark it as active
	job := &db.Job{
		ID:            "job-active",
		LinearIssueID: "ENG-400",
		State:         db.StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Mark as active (simulating already running)
	orch.markJobActive("job-active")

	// Resume in-flight jobs (should skip already active job)
	if err := orch.ResumeInFlightJobs(); err != nil {
		t.Fatalf("ResumeInFlightJobs failed: %v", err)
	}

	// Give a moment for any goroutines
	time.Sleep(100 * time.Millisecond)

	// Verify job is still in coding state (not failed due to missing session ID)
	// because it was skipped
	jobAfter, err := database.GetJob("job-active")
	if err != nil {
		t.Fatalf("Failed to get job after resume: %v", err)
	}
	if jobAfter.State != db.StateCoding {
		t.Errorf("Expected job to remain in coding state (skipped), got state %s", jobAfter.State)
	}
}
