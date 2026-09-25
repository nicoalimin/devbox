package db

import (
	"os"
	"path/filepath"
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
		{StateStuck, true, false},
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

// TestMigrationEmptyDatabase tests that migrations work on a fresh database
func TestMigrationEmptyDatabase(t *testing.T) {
	dbPath := "test_migration_empty.db"
	defer os.Remove(dbPath)
	defer os.Remove(dbPath + "-shm")
	defer os.Remove(dbPath + "-wal")

	// Open database - should run migrations
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open empty database: %v", err)
	}
	defer db.Close()

	// Verify schema exists by creating a job with all columns
	now := time.Now()
	codingWaitStarted := now.Add(-5 * time.Minute)
	reviewingWaitStarted := now.Add(-2 * time.Minute)

	job := &Job{
		ID:                     "migration-test-1",
		LinearIssueID:          "ENG-MIGRATE-1",
		LinearURL:              "https://linear.app/test/ENG-MIGRATE-1",
		State:                  StateCoding,
		RepoPath:               "/test/repo",
		BranchName:             "eng-migrate-1",
		WorktreePath:           "/test/worktree",
		PRURL:                  "",
		BlockerReason:          "",
		OpenCodeSessionID:      "session-123",
		OperatorContext:        "test context",
		ReviewFeedback:         "",
		CodingWaitStartedAt:    &codingWaitStarted,
		ReviewingWaitStartedAt: &reviewingWaitStarted,
		CreatedAt:              now,
		UpdatedAt:              now,
		CompletedAt:            nil,
	}

	if err := db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job after migration: %v", err)
	}

	// Retrieve and verify all fields including wait timestamps
	retrieved, err := db.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to retrieve job: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Retrieved job is nil")
	}

	if retrieved.CodingWaitStartedAt == nil {
		t.Error("CodingWaitStartedAt should not be nil")
	} else if !retrieved.CodingWaitStartedAt.Equal(codingWaitStarted) {
		t.Errorf("CodingWaitStartedAt mismatch: expected %v, got %v", codingWaitStarted, *retrieved.CodingWaitStartedAt)
	}

	if retrieved.ReviewingWaitStartedAt == nil {
		t.Error("ReviewingWaitStartedAt should not be nil")
	} else if !retrieved.ReviewingWaitStartedAt.Equal(reviewingWaitStarted) {
		t.Errorf("ReviewingWaitStartedAt mismatch: expected %v, got %v", reviewingWaitStarted, *retrieved.ReviewingWaitStartedAt)
	}
}

// TestMigrationIdempotent tests that reopening a database is idempotent
func TestMigrationIdempotent(t *testing.T) {
	dbPath := "test_migration_idempotent.db"
	defer os.Remove(dbPath)
	defer os.Remove(dbPath + "-shm")
	defer os.Remove(dbPath + "-wal")

	// Open database first time
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database (first time): %v", err)
	}

	// Create a job
	job := &Job{
		ID:            "idempotent-test",
		LinearIssueID: "ENG-IDEM-1",
		State:         StateCoding,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db1.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	db1.Close()

	// Reopen database - migrations should be idempotent (no error)
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer db2.Close()

	// Verify job still exists
	retrieved, err := db2.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to retrieve job after reopen: %v", err)
	}
	if retrieved == nil {
		t.Fatal("Job was lost after reopen")
	}
	if retrieved.ID != job.ID {
		t.Errorf("Job ID mismatch: expected %s, got %s", job.ID, retrieved.ID)
	}

	// Close and reopen a third time to ensure idempotency
	db2.Close()
	db3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to reopen database (third time): %v", err)
	}
	defer db3.Close()

	// Verify job still exists
	retrieved3, err := db3.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to retrieve job after third reopen: %v", err)
	}
	if retrieved3 == nil {
		t.Fatal("Job was lost after third reopen")
	}
}

// TestDirectoryCreation tests that parent directories are created automatically
func TestDirectoryCreation(t *testing.T) {
	testDir := "test_subdir/nested/path"
	dbPath := filepath.Join(testDir, "jobs.db")

	// Clean up before and after
	defer os.RemoveAll("test_subdir")
	os.RemoveAll("test_subdir")

	// Open database in nested directory
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database in nested directory: %v", err)
	}
	defer db.Close()

	// Verify directory was created
	if _, err := os.Stat(testDir); os.IsNotExist(err) {
		t.Errorf("Parent directory was not created: %s", testDir)
	}

	// Verify database file exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("Database file was not created: %s", dbPath)
	}

	// Verify we can use the database
	job := &Job{
		ID:            "dir-test",
		LinearIssueID: "ENG-DIR-1",
		State:         StateQueued,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job in nested database: %v", err)
	}
}

func TestGetPriorReusableJob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prior.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	older := &Job{
		ID: "old", LinearIssueID: "UTA-96", State: StateDone,
		BranchName: "devbox/uta-96", WorktreePath: "/tmp/old",
		PRURL:     "https://example.com/pull/1",
		CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now(),
	}
	newer := &Job{
		ID: "new", LinearIssueID: "UTA-96", State: StateDone,
		BranchName: "devbox/uta-96-2", WorktreePath: "/tmp/new",
		PRURL:     "https://example.com/pull/2",
		CreatedAt: time.Now().Add(-1 * time.Hour), UpdatedAt: time.Now(),
	}
	current := &Job{
		ID: "current", LinearIssueID: "UTA-96", State: StateFetching,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	for _, j := range []*Job{older, newer, current} {
		if err := database.CreateJob(j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := database.GetPriorReusableJob("UTA-96", "current")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "new" {
		t.Fatalf("want newest prior with PR, got %+v", got)
	}
	got, err = database.GetPriorReusableJob("UTA-96", "new")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "old" {
		t.Fatalf("excluding new should return old, got %+v", got)
	}
	got, err = database.GetPriorReusableJob("OTHER", "current")
	if err != nil || got != nil {
		t.Fatalf("expected nil for other issue, got %+v err=%v", got, err)
	}
}

func TestFailureSignaturePersistsAndPreviousJobLookup(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now()
	older := &Job{ID: "older", LinearIssueID: "UTA-94", State: StateStuck, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
		FailureSignature: "abc123", FailureSummary: "TypeScript errors: 45 total, 2 distinct"}
	newer := &Job{ID: "newer", LinearIssueID: "UTA-94", State: StateCoding, CreatedAt: now, UpdatedAt: now}
	other := &Job{ID: "other", LinearIssueID: "UTA-1", State: StateFailed, CreatedAt: now.Add(time.Hour), UpdatedAt: now, FailureSignature: "zzz"}
	for _, j := range []*Job{older, newer, other} {
		if err := database.CreateJob(j); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := database.GetPreviousJob("UTA-94", "newer")
	if err != nil || prev == nil || prev.ID != "older" || prev.FailureSignature != "abc123" || prev.FailureSummary == "" || !prev.State.IsFailure() {
		t.Fatalf("prev=%+v err=%v", prev, err)
	}
	newer.FailureSignature, newer.FailureSummary = "def456", "summary"
	if err := database.UpdateJob(newer); err != nil {
		t.Fatal(err)
	}
	got, _ := database.GetJob("newer")
	if got.FailureSignature != "def456" || got.FailureSummary != "summary" {
		t.Fatalf("update not persisted: %+v", got)
	}
	if none, err := database.GetPreviousJob("UTA-1", "other"); err != nil || none != nil {
		t.Fatalf("expected no previous job, got %+v %v", none, err)
	}
}

func TestGetPreviousJobSkipsCancelled(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "prev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	base := time.Now().Add(-time.Hour)
	for i, j := range []*Job{
		{ID: "failed-1", State: StateFailed},
		{ID: "cancelled", State: StateCancelled},
		{ID: "current", State: StateCoding},
	} {
		j.LinearIssueID = "UTA-1"
		j.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		j.UpdatedAt = j.CreatedAt
		if err := database.CreateJob(j); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := database.GetPreviousJob("UTA-1", "current")
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.ID != "failed-1" {
		t.Fatalf("previous=%v want failed-1 (cancelled job must be skipped)", prev)
	}
}
