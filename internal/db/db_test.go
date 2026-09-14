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
		{StatePROpen, false, false},
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

func TestRestartPersistence(t *testing.T) {
	dbPath := "test_restart.db"
	defer os.Remove(dbPath)

	// Phase 1: Create database, write data, close
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database (phase 1): %v", err)
	}

	// Create jobs with various states
	now := time.Now()
	completedTime := now.Add(1 * time.Hour)
	jobs := []*Job{
		{
			ID:              "job-restart-1",
			LinearIssueID:   "ENG-1001",
			LinearURL:       "https://linear.app/test/ENG-1001",
			State:           StateDone,
			RepoPath:        "/test/repo",
			BranchName:      "eng-1001-test",
			WorktreePath:    "/test/worktree/1001",
			PRURL:           "https://github.com/test/repo/pull/1",
			OperatorContext: "Test context 1",
			CreatedAt:       now,
			UpdatedAt:       now,
			CompletedAt:     &completedTime,
		},
		{
			ID:                "job-restart-2",
			LinearIssueID:     "ENG-1002",
			LinearURL:         "https://linear.app/test/ENG-1002",
			State:             StateBlocked,
			RepoPath:          "/test/repo",
			BranchName:        "eng-1002-test",
			WorktreePath:      "/test/worktree/1002",
			BlockerReason:     "Needs clarification",
			OpenCodeSessionID: "session-123",
			OperatorContext:   "Test context 2",
			ReviewFeedback:    "Please fix the formatting",
			CreatedAt:         now,
			UpdatedAt:         now,
		},
		{
			ID:            "job-restart-3",
			LinearIssueID: "ENG-1003",
			LinearURL:     "https://linear.app/test/ENG-1003",
			State:         StateCoding,
			RepoPath:      "/test/repo",
			BranchName:    "eng-1003-test",
			WorktreePath:  "/test/worktree/1003",
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	}

	for _, job := range jobs {
		if err := db1.CreateJob(job); err != nil {
			t.Fatalf("Failed to create job %s: %v", job.ID, err)
		}
	}

	// Add some logs for the first job
	if err := db1.AddLog("job-restart-1", "info", "Job created"); err != nil {
		t.Fatalf("Failed to add log: %v", err)
	}
	if err := db1.AddLog("job-restart-1", "info", "Job completed successfully"); err != nil {
		t.Fatalf("Failed to add log: %v", err)
	}

	// Close the database (simulates restart)
	if err := db1.Close(); err != nil {
		t.Fatalf("Failed to close database: %v", err)
	}

	// Phase 2: Reopen database and verify all data persisted
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database (phase 2): %v", err)
	}
	defer db2.Close()

	// Verify all jobs are still there
	allJobs, err := db2.ListJobs(0)
	if err != nil {
		t.Fatalf("Failed to list jobs after restart: %v", err)
	}

	if len(allJobs) != 3 {
		t.Errorf("Expected 3 jobs after restart, got %d", len(allJobs))
	}

	// Verify individual jobs with all fields
	for _, originalJob := range jobs {
		retrievedJob, err := db2.GetJob(originalJob.ID)
		if err != nil {
			t.Fatalf("Failed to get job %s after restart: %v", originalJob.ID, err)
		}
		if retrievedJob == nil {
			t.Errorf("Job %s not found after restart", originalJob.ID)
			continue
		}

		// Verify all critical fields
		if retrievedJob.ID != originalJob.ID {
			t.Errorf("Job ID mismatch: expected %s, got %s", originalJob.ID, retrievedJob.ID)
		}
		if retrievedJob.LinearIssueID != originalJob.LinearIssueID {
			t.Errorf("Linear issue ID mismatch: expected %s, got %s", originalJob.LinearIssueID, retrievedJob.LinearIssueID)
		}
		if retrievedJob.LinearURL != originalJob.LinearURL {
			t.Errorf("Linear URL mismatch: expected %s, got %s", originalJob.LinearURL, retrievedJob.LinearURL)
		}
		if retrievedJob.State != originalJob.State {
			t.Errorf("State mismatch for %s: expected %s, got %s", originalJob.ID, originalJob.State, retrievedJob.State)
		}
		if retrievedJob.RepoPath != originalJob.RepoPath {
			t.Errorf("Repo path mismatch: expected %s, got %s", originalJob.RepoPath, retrievedJob.RepoPath)
		}
		if retrievedJob.BranchName != originalJob.BranchName {
			t.Errorf("Branch name mismatch: expected %s, got %s", originalJob.BranchName, retrievedJob.BranchName)
		}
		if retrievedJob.WorktreePath != originalJob.WorktreePath {
			t.Errorf("Worktree path mismatch: expected %s, got %s", originalJob.WorktreePath, retrievedJob.WorktreePath)
		}
		if retrievedJob.PRURL != originalJob.PRURL {
			t.Errorf("PR URL mismatch: expected %s, got %s", originalJob.PRURL, retrievedJob.PRURL)
		}
		if retrievedJob.BlockerReason != originalJob.BlockerReason {
			t.Errorf("Blocker reason mismatch: expected %s, got %s", originalJob.BlockerReason, retrievedJob.BlockerReason)
		}
		if retrievedJob.OpenCodeSessionID != originalJob.OpenCodeSessionID {
			t.Errorf("OpenCode session ID mismatch: expected %s, got %s", originalJob.OpenCodeSessionID, retrievedJob.OpenCodeSessionID)
		}
		if retrievedJob.OperatorContext != originalJob.OperatorContext {
			t.Errorf("Operator context mismatch: expected %s, got %s", originalJob.OperatorContext, retrievedJob.OperatorContext)
		}
		if retrievedJob.ReviewFeedback != originalJob.ReviewFeedback {
			t.Errorf("Review feedback mismatch: expected %s, got %s", originalJob.ReviewFeedback, retrievedJob.ReviewFeedback)
		}

		// Verify completed timestamp if present
		if originalJob.CompletedAt != nil {
			if retrievedJob.CompletedAt == nil {
				t.Errorf("Completed timestamp missing for job %s", originalJob.ID)
			} else if !retrievedJob.CompletedAt.Equal(*originalJob.CompletedAt) {
				t.Errorf("Completed timestamp mismatch for job %s", originalJob.ID)
			}
		}
	}

	// Verify GetCurrentJob still works (should return the coding job)
	currentJob, err := db2.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job after restart: %v", err)
	}
	if currentJob == nil || currentJob.ID != "job-restart-3" {
		t.Errorf("Current job should be job-restart-3 after restart, got %v", currentJob)
	}

	// Verify GetBlockedJobs still works
	blockedJobs, err := db2.GetBlockedJobs()
	if err != nil {
		t.Fatalf("Failed to get blocked jobs after restart: %v", err)
	}
	if len(blockedJobs) != 1 || blockedJobs[0].ID != "job-restart-2" {
		t.Errorf("Expected 1 blocked job (job-restart-2) after restart, got %d", len(blockedJobs))
	}

	// Verify GetJobByLinearIssueID still works
	jobByLinear, err := db2.GetJobByLinearIssueID("ENG-1001")
	if err != nil {
		t.Fatalf("Failed to get job by Linear ID after restart: %v", err)
	}
	if jobByLinear == nil || jobByLinear.ID != "job-restart-1" {
		t.Errorf("Expected job-restart-1 when looking up by Linear ID, got %v", jobByLinear)
	}

	// Verify logs persisted
	logs, err := db2.GetLogs("job-restart-1", 0)
	if err != nil {
		t.Fatalf("Failed to get logs after restart: %v", err)
	}
	if len(logs) != 2 {
		t.Errorf("Expected 2 log entries after restart, got %d", len(logs))
	}

	// Verify we can still update jobs after restart
	jobToUpdate, _ := db2.GetJob("job-restart-2")
	jobToUpdate.State = StateCoding
	jobToUpdate.BlockerReason = ""
	if err := db2.UpdateJob(jobToUpdate); err != nil {
		t.Fatalf("Failed to update job after restart: %v", err)
	}

	// Verify update persisted
	updatedJob, err := db2.GetJob("job-restart-2")
	if err != nil {
		t.Fatalf("Failed to get updated job: %v", err)
	}
	if updatedJob.State != StateCoding {
		t.Errorf("Job state update did not persist: expected %s, got %s", StateCoding, updatedJob.State)
	}
	if updatedJob.BlockerReason != "" {
		t.Errorf("BlockerReason should be empty after update, got: %s", updatedJob.BlockerReason)
	}
}

func TestGetCurrentJobExcludesPROpen(t *testing.T) {
	dbPath := "test_pr_open_not_busy.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create a job in pr_open state
	job := &Job{
		ID:            "job-pr-open",
		LinearIssueID: "ENG-456",
		State:         StatePROpen,
		PRURL:         "https://github.com/test/repo/pull/1",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// GetCurrentJob should return nil (pr_open is not busy)
	current, err := db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current != nil {
		t.Errorf("Expected nil (pr_open should not be busy), got job %s", current.ID)
	}
}

func TestGetCurrentJobExcludesBlocked(t *testing.T) {
	dbPath := "test_blocked_not_busy.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create a job in blocked state
	job := &Job{
		ID:            "job-blocked",
		LinearIssueID: "ENG-789",
		State:         StateBlocked,
		BlockerReason: "Waiting for clarification",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// GetCurrentJob should return nil (blocked is not busy)
	current, err := db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current != nil {
		t.Errorf("Expected nil (blocked should not be busy), got job %s", current.ID)
	}
}

func TestGetCurrentJobWithMultipleStates(t *testing.T) {
	dbPath := "test_multiple_states.db"
	defer os.Remove(dbPath)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create jobs in various states
	jobs := []*Job{
		{
			ID:            "job-done",
			LinearIssueID: "ENG-100",
			State:         StateDone,
			CreatedAt:     time.Now().Add(-5 * time.Minute),
			UpdatedAt:     time.Now(),
		},
		{
			ID:            "job-pr-open",
			LinearIssueID: "ENG-200",
			State:         StatePROpen,
			PRURL:         "https://github.com/test/repo/pull/2",
			CreatedAt:     time.Now().Add(-4 * time.Minute),
			UpdatedAt:     time.Now(),
		},
		{
			ID:            "job-blocked",
			LinearIssueID: "ENG-300",
			State:         StateBlocked,
			BlockerReason: "Need info",
			CreatedAt:     time.Now().Add(-3 * time.Minute),
			UpdatedAt:     time.Now(),
		},
		{
			ID:            "job-coding",
			LinearIssueID: "ENG-400",
			State:         StateCoding,
			CreatedAt:     time.Now().Add(-2 * time.Minute),
			UpdatedAt:     time.Now(),
		},
		{
			ID:            "job-failed",
			LinearIssueID: "ENG-500",
			State:         StateFailed,
			CreatedAt:     time.Now().Add(-1 * time.Minute),
			UpdatedAt:     time.Now(),
		},
	}

	for _, job := range jobs {
		if err := db.CreateJob(job); err != nil {
			t.Fatalf("Failed to create job %s: %v", job.ID, err)
		}
	}

	// GetCurrentJob should return only the coding job
	current, err := db.GetCurrentJob()
	if err != nil {
		t.Fatalf("Failed to get current job: %v", err)
	}
	if current == nil {
		t.Errorf("Expected job-coding, got nil")
	} else if current.ID != "job-coding" {
		t.Errorf("Expected job-coding, got %s", current.ID)
	}
}
