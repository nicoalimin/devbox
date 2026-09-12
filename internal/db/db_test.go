package db

import (
	"os"
	"testing"
	"time"
)

func TestJobStateTransitions(t *testing.T) {
	// Create temporary database
	dbPath := "test_jobs.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create a job
	job := &Job{
		ID:            "test-job-1",
		LinearIssueID: "ENG-123",
		State:         StateQueued,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	if err := db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Test state transitions
	states := []JobState{
		StateFetching,
		StatePreparing,
		StateCoding,
		StateReviewing,
		StatePushing,
		StatePROpen,
		StateDone,
	}

	for _, state := range states {
		job.State = state
		if err := db.UpdateJob(job); err != nil {
			t.Fatalf("Failed to update job to state %s: %v", state, err)
		}

		retrieved, err := db.GetJob(job.ID)
		if err != nil {
			t.Fatalf("Failed to get job: %v", err)
		}

		if retrieved.State != state {
			t.Errorf("Expected state %s, got %s", state, retrieved.State)
		}
	}
}

func TestJobStateMethods(t *testing.T) {
	tests := []struct {
		state      JobState
		isTerminal bool
		isBusy     bool
	}{
		{StateQueued, false, false},
		{StateFetching, false, true},
		{StatePreparing, false, true},
		{StateCoding, false, true},
		{StateReviewing, false, true},
		{StatePushing, false, true},
		{StatePROpen, false, true},
		{StateDone, true, false},
		{StateBlocked, false, false},
		{StateFailed, true, false},
		{StateCancelled, true, false},
	}

	for _, tt := range tests {
		if tt.state.IsTerminal() != tt.isTerminal {
			t.Errorf("State %s: IsTerminal() = %v, want %v",
				tt.state, tt.state.IsTerminal(), tt.isTerminal)
		}
		if tt.state.IsBusy() != tt.isBusy {
			t.Errorf("State %s: IsBusy() = %v, want %v",
				tt.state, tt.state.IsBusy(), tt.isBusy)
		}
	}
}

func TestGetCurrentJob(t *testing.T) {
	dbPath := "test_current_job.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// No jobs initially
	current, err := db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current != nil {
		t.Errorf("Expected nil, got job %s", current.ID)
	}

	// Create a busy job
	job1 := &Job{
		ID:            "job-1",
		LinearIssueID: "ENG-123",
		State:         StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db.CreateJob(job1); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Should return the busy job
	current, err = db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current == nil || current.ID != "job-1" {
		t.Errorf("Expected job-1, got %v", current)
	}

	// Create a completed job (should not be current)
	job2 := &Job{
		ID:            "job-2",
		LinearIssueID: "ENG-124",
		State:         StateDone,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db.CreateJob(job2); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Should still return job-1
	current, err = db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current == nil || current.ID != "job-1" {
		t.Errorf("Expected job-1, got %v", current)
	}

	// Mark job1 as done
	now := time.Now()
	job1.State = StateDone
	job1.CompletedAt = &now
	if err := db.UpdateJob(job1); err != nil {
		t.Fatalf("Failed to update job: %v", err)
	}

	// Should return nil (no busy jobs)
	current, err = db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current != nil {
		t.Errorf("Expected nil, got job %s", current.ID)
	}
}

func TestGetBlockedJobs(t *testing.T) {
	dbPath := "test_blocked_jobs.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create jobs in various states
	jobs := []*Job{
		{
			ID:            "job-1",
			LinearIssueID: "ENG-123",
			State:         StateBlocked,
			BlockerReason: "Need clarification",
			CreatedAt:     time.Now().Add(-2 * time.Hour),
			UpdatedAt:     time.Now(),
		},
		{
			ID:            "job-2",
			LinearIssueID: "ENG-124",
			State:         StateCoding,
			CreatedAt:     time.Now().Add(-1 * time.Hour),
			UpdatedAt:     time.Now(),
		},
		{
			ID:            "job-3",
			LinearIssueID: "ENG-125",
			State:         StateBlocked,
			BlockerReason: "Missing specs",
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		},
	}

	for _, job := range jobs {
		if err := db.CreateJob(job); err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
	}

	// Get blocked jobs
	blocked, err := db.GetBlockedJobs()
	if err != nil {
		t.Fatalf("Failed to get blocked jobs: %v", err)
	}

	if len(blocked) != 2 {
		t.Errorf("Expected 2 blocked jobs, got %d", len(blocked))
	}

	// Verify they are in reverse chronological order (newest first)
	if len(blocked) == 2 {
		if blocked[0].ID != "job-3" || blocked[1].ID != "job-1" {
			t.Errorf("Blocked jobs not in correct order: got %s, %s", blocked[0].ID, blocked[1].ID)
		}
	}
}

func TestJobLogs(t *testing.T) {
	dbPath := "test_logs.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create a job
	job := &Job{
		ID:            "job-1",
		LinearIssueID: "ENG-123",
		State:         StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Add logs
	logs := []struct {
		level   string
		message string
	}{
		{"info", "Job created"},
		{"info", "Fetching Linear issue"},
		{"info", "Creating worktree"},
		{"info", "Starting OpenCode"},
		{"warn", "OpenCode is taking longer than expected"},
	}

	for _, log := range logs {
		if err := db.AddLog(job.ID, log.level, log.message); err != nil {
			t.Fatalf("Failed to add log: %v", err)
		}
		time.Sleep(10 * time.Millisecond) // Ensure distinct timestamps
	}

	// Get all logs
	allLogs, err := db.GetLogs(job.ID, 0)
	if err != nil {
		t.Fatalf("Failed to get logs: %v", err)
	}

	if len(allLogs) != 5 {
		t.Errorf("Expected 5 logs, got %d", len(allLogs))
	}

	// Verify chronological order
	if len(allLogs) >= 2 {
		if allLogs[0].Timestamp.After(allLogs[1].Timestamp) {
			t.Errorf("Logs not in chronological order")
		}
	}

	// Get tail
	tailLogs, err := db.GetLogs(job.ID, 2)
	if err != nil {
		t.Fatalf("Failed to get tail logs: %v", err)
	}

	if len(tailLogs) != 2 {
		t.Errorf("Expected 2 tail logs, got %d", len(tailLogs))
	}

	if len(tailLogs) == 2 {
		if tailLogs[1].Message != "OpenCode is taking longer than expected" {
			t.Errorf("Expected last log to be about OpenCode, got: %s", tailLogs[1].Message)
		}
	}
}
