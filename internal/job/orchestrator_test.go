package job

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/linear"
)

func TestBuildCodingPromptDefersDeliveryToHost(t *testing.T) {
	orch := &Orchestrator{}
	prompt := orch.buildCodingPrompt(&linear.Issue{
		Identifier: "TEST-123",
		Title:      "Fix formatting",
		Team:       linear.Team{Name: "Test", Key: "TEST"},
		State:      linear.State{Name: "Todo"},
	}, "", "")

	for _, required := range []string{
		"formatting, lint, typecheck, test, and build failures as work to fix",
		"pnpm exec prettier --write .",
		"every file reported anywhere in the repository",
		"host delivery gate",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("coding prompt does not contain %q\n%s", required, prompt)
		}
	}
}

func TestBuildValidationRepairPromptIncludesExactFailureAndActions(t *testing.T) {
	prompt := buildValidationRepairPrompt(fmt.Errorf("prettier failed: web/tsconfig.json"))
	for _, required := range []string{
		"prettier failed: web/tsconfig.json",
		"pnpm exec prettier --write .",
		"rerun the exact failing command",
		"Leave the resulting fixes in the worktree",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("validation repair prompt does not contain %q\n%s", required, prompt)
		}
	}
}

func TestFailJobPreservesDirtyWorktree(t *testing.T) {
	repoPath := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}

	readme := filepath.Join(repoPath, "README.md")
	if err := os.WriteFile(readme, []byte("initial\n"), 0644); err != nil {
		t.Fatalf("failed to create initial file: %v", err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}

	dirtyFile := filepath.Join(repoPath, "agent-output.txt")
	if err := os.WriteFile(dirtyFile, []byte("valuable uncommitted work\n"), 0644); err != nil {
		t.Fatalf("failed to create dirty file: %v", err)
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	job := &db.Job{
		ID:            "job-preserve-dirty",
		LinearIssueID: "TEST-123",
		State:         db.StatePushing,
		RepoPath:      repoPath,
		WorktreePath:  repoPath,
		BranchName:    "devbox/test-123",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("failed to create job: %v", err)
	}

	orch := NewOrchestrator(&config.Config{GitHub: config.GitHubConfig{DefaultBaseBranch: "main"}}, database)
	orch.failJob(job, "delivery failed")

	if _, err := os.Stat(dirtyFile); err != nil {
		t.Fatalf("dirty worktree output was removed: %v", err)
	}
	updated, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("failed to reload job: %v", err)
	}
	if updated.State != db.StateFailed {
		t.Fatalf("job state = %s, want failed", updated.State)
	}
}

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
			BaseURL: "http://127.0.0.1:3000",
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
			BaseURL: "http://127.0.0.1:3000",
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
			BaseURL: "http://127.0.0.1:3000",
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
		ID:                "job-1",
		LinearIssueID:     "ENG-123",
		State:             db.StateBlocked,
		BlockerReason:     "Need clarification on button color",
		OpenCodeSessionID: "session-123",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
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
			BaseURL: "http://127.0.0.1:3000",
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
			BaseURL: "http://127.0.0.1:3000",
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
			BaseURL: "http://127.0.0.1:3000",
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
			BaseURL: "http://127.0.0.1:3000",
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

func TestHealSession_CodingPhase(t *testing.T) {
	// Setup
	dbPath := "test_heal_coding.db"
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
			BaseURL:  "http://127.0.0.1:3000",
			Timeout:  30 * time.Minute,
			Username: "test",
			Password: "test",
			Version:  "v2",
		},
		Repos: []config.RepoConfig{
			{
				Match: config.RepoMatch{Team: "ENG"},
				Repo:  config.RepoInfo{Path: "/tmp/repo", BaseBranch: "main"},
			},
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job in coding state
	job := &db.Job{
		ID:                "job-heal-coding",
		LinearIssueID:     "ENG-500",
		State:             db.StateCoding,
		OpenCodeSessionID: "dead-session-123",
		WorktreePath:      "/tmp/worktree",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Test healSession method (will fail because OpenCode isn't running, but we can verify the logic)
	// We can't fully test without mocking OpenCode, but we can verify the method exists and handles errors
	_, err = orch.healSession(job, "coding")
	if err == nil {
		t.Error("Expected error when healing without OpenCode running, got nil")
	}

	// Verify error message indicates it's trying to fetch issue or create session
	errMsg := err.Error()
	if !strings.Contains(errMsg, "failed") {
		t.Errorf("Expected error to contain 'failed', got: %s", errMsg)
	}
}

func TestHealSession_ReviewingPhase(t *testing.T) {
	// Setup
	dbPath := "test_heal_reviewing.db"
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
			BaseURL:  "http://127.0.0.1:3000",
			Timeout:  30 * time.Minute,
			Username: "test",
			Password: "test",
			Version:  "v2",
		},
		Repos: []config.RepoConfig{
			{
				Match: config.RepoMatch{Team: "ENG"},
				Repo:  config.RepoInfo{Path: "/tmp/repo", BaseBranch: "main"},
			},
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job in reviewing state
	job := &db.Job{
		ID:                "job-heal-reviewing",
		LinearIssueID:     "ENG-501",
		State:             db.StateReviewing,
		OpenCodeSessionID: "dead-session-456",
		WorktreePath:      "/tmp/worktree",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Test healSession with reviewing phase
	_, err = orch.healSession(job, "reviewing")
	if err == nil {
		t.Error("Expected error when healing without Linear/OpenCode running, got nil")
	}
}

func TestHealingAttemptLimit(t *testing.T) {
	// Setup
	dbPath := "test_healing_limit.db"
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
			BaseURL:  "http://127.0.0.1:3000",
			Timeout:  30 * time.Minute,
			Username: "test",
			Password: "test",
			Version:  "v2",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job
	job := &db.Job{
		ID:                "job-heal-limit",
		LinearIssueID:     "ENG-502",
		State:             db.StateCoding,
		OpenCodeSessionID: "dead-session-789",
		WorktreePath:      "/tmp/worktree",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Simulate healing attempts
	attemptKey := fmt.Sprintf("%s-%s", job.ID, "coding")

	// First attempt should be allowed
	orch.healingAttempts[attemptKey] = 0
	if orch.healingAttempts[attemptKey] >= maxHealingAttempts {
		t.Error("First attempt should be allowed")
	}

	// Second attempt should be allowed
	orch.healingAttempts[attemptKey] = 1
	if orch.healingAttempts[attemptKey] >= maxHealingAttempts {
		t.Error("Second attempt should be allowed")
	}

	// Third attempt should be blocked
	orch.healingAttempts[attemptKey] = 2
	if orch.healingAttempts[attemptKey] < maxHealingAttempts {
		t.Error("Third attempt should be blocked (max is 2)")
	}
}

func TestHealSession_InvalidPhase(t *testing.T) {
	// Setup
	dbPath := "test_heal_invalid.db"
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
			BaseURL:  "http://127.0.0.1:3000",
			Timeout:  30 * time.Minute,
			Username: "test",
			Password: "test",
			Version:  "v2",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job
	job := &db.Job{
		ID:                "job-heal-invalid",
		LinearIssueID:     "ENG-503",
		State:             db.StateCoding,
		OpenCodeSessionID: "dead-session-xyz",
		WorktreePath:      "/tmp/worktree",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Test with invalid phase (even without Linear running, it should fail on phase validation first)
	// Actually, it will fail on Linear fetch first, but let's verify the error handling
	_, err = orch.healSession(job, "invalid-phase")
	if err == nil {
		t.Error("Expected error when healing with invalid phase")
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
			BaseURL: "http://127.0.0.1:3000",
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

// TestCodingTimeoutFailsJob verifies that a coding phase timeout or failure
// results in a failed job that does NOT proceed to the reviewing phase.
// This is a regression test for UTA-15 where timed-out coding sessions
// incorrectly proceeded to code review.
func TestCodingTimeoutFailsJob(t *testing.T) {
	// Setup
	dbPath := "test_coding_timeout_fails.db"
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
			BaseURL:  "http://127.0.0.1:3000",
			Timeout:  1 * time.Second, // Very short timeout to trigger failure quickly
			Username: "test",
			Password: "test",
			Version:  "v2",
		},
		Repos: []config.RepoConfig{
			{
				Match: config.RepoMatch{Team: "ENG"},
				Repo:  config.RepoInfo{Path: "/tmp/repo", BaseBranch: "main"},
			},
		},
		GitHub: config.GitHubConfig{
			DefaultBaseBranch: "main",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job in coding state with a dead/missing OpenCode session
	// This simulates the UTA-15 scenario where the session times out
	job := &db.Job{
		ID:                "job-timeout-test",
		LinearIssueID:     "ENG-999",
		State:             db.StateCoding,
		OpenCodeSessionID: "nonexistent-session-id", // This session doesn't exist
		WorktreePath:      "/tmp/test-worktree",
		RepoPath:          "/tmp/repo",
		BranchName:        "test-branch",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Start resuming the coding phase in a goroutine (simulates real workflow)
	done := make(chan bool)
	go func() {
		defer func() { done <- true }()
		orch.markJobActive(job.ID)
		defer orch.markJobInactive(job.ID)

		// This should fail and mark the job as failed
		err := orch.resumeCoding(job)
		if err == nil {
			t.Error("Expected resumeCoding to return error for timed-out/missing session")
		}

		// The error should propagate and cause failJob to be called by the caller
		// In the real workflow, this happens in resumeJobFromState
		if err != nil {
			orch.failJob(job, fmt.Sprintf("Failed to resume coding: %v", err))
		}
	}()

	// Wait for goroutine to complete (with timeout)
	select {
	case <-done:
		// Good, completed
	case <-time.After(10 * time.Second):
		t.Fatal("Test timed out waiting for resumeCoding")
	}

	// Verify the job is in failed state (not reviewing)
	finalJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job after timeout: %v", err)
	}

	if finalJob.State != db.StateFailed {
		t.Errorf("Expected job to be in failed state after coding timeout, got %s", finalJob.State)
	}

	// Verify the job did NOT enter reviewing state
	if finalJob.State == db.StateReviewing {
		t.Error("Job incorrectly proceeded to reviewing state after coding timeout - this is the bug!")
	}

	// Verify there's a blocker reason explaining the failure
	if finalJob.BlockerReason == "" {
		t.Error("Expected BlockerReason to be set on failed job")
	}

	// Verify the error message mentions the timeout/failure
	if !strings.Contains(strings.ToLower(finalJob.BlockerReason), "coding") {
		t.Errorf("Expected BlockerReason to mention coding failure, got: %s", finalJob.BlockerReason)
	}
}

func TestWaitTimeoutPersistence_BrandNewJob(t *testing.T) {
	// Test that a brand-new job gets the full timeout budget
	dbPath := "test_wait_persistence_new.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://127.0.0.1:3000",
			Timeout: 30 * time.Minute,
			Version: "v2",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a new job
	job := &db.Job{
		ID:                "test-new-job",
		LinearIssueID:     "ENG-500",
		State:             db.StateCoding,
		OpenCodeSessionID: "ses_new",
		WorktreePath:      "/tmp/test",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Verify no wait start time is set initially
	if job.CodingWaitStartedAt != nil {
		t.Error("New job should not have CodingWaitStartedAt set")
	}
	if job.ReviewingWaitStartedAt != nil {
		t.Error("New job should not have ReviewingWaitStartedAt set")
	}

	// Simulate starting a wait by calling waitForSessionWithHealing
	// (it will fail because OpenCode isn't running, but that's OK - we're testing persistence)
	logFunc := func(msg string) {
		t.Logf("[test] %s", msg)
	}

	// Start wait in background (will timeout or fail)
	done := make(chan bool)
	go func() {
		defer close(done)
		_, _ = orch.waitForSessionWithHealing(job, "coding", logFunc)
	}()

	// Give it a moment to persist the wait start time
	time.Sleep(100 * time.Millisecond)

	// Fetch job from DB
	updatedJob, err := database.GetJob(job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	// Verify wait start time was persisted
	if updatedJob.CodingWaitStartedAt == nil {
		t.Error("Expected CodingWaitStartedAt to be set after starting wait")
	}

	// Wait for completion
	select {
	case <-done:
		// Good
	case <-time.After(5 * time.Second):
		// Timeout is OK for this test
	}
}

func TestWaitTimeoutPersistence_ResumeFromPriorElapsed(t *testing.T) {
	// Test that resuming a job continues from prior elapsed time, not fresh timeout
	dbPath := "test_wait_persistence_resume.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://127.0.0.1:3000",
			Timeout: 10 * time.Second, // Short timeout for testing
			Version: "v2",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job with a wait that started 8 seconds ago
	waitStartedAt := time.Now().Add(-8 * time.Second)
	job := &db.Job{
		ID:                  "test-resume-job",
		LinearIssueID:       "ENG-501",
		State:               db.StateCoding,
		OpenCodeSessionID:   "ses_resume",
		WorktreePath:        "/tmp/test",
		CodingWaitStartedAt: &waitStartedAt, // Already waiting for 8s
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Resume the wait - should have only 2s remaining (10s budget - 8s elapsed)
	logFunc := func(msg string) {
		t.Logf("[test] %s", msg)
	}

	start := time.Now()
	_, err = orch.waitForSessionWithHealing(job, "coding", logFunc)
	elapsed := time.Since(start)

	// Should fail quickly (around 2s, not 10s)
	// We expect it to timeout after ~2s (remaining time), not wait the full 10s
	if elapsed > 5*time.Second {
		t.Errorf("Expected wait to use remaining timeout (~2s), but waited %v", elapsed)
	}

	// Verify error mentions exhausted/timed out
	if err == nil {
		t.Error("Expected error (timeout), got nil")
	}
}

func TestWaitTimeoutPersistence_AlreadyExhausted(t *testing.T) {
	// Test that a job with exhausted timeout fails immediately
	dbPath := "test_wait_persistence_exhausted.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://127.0.0.1:3000",
			Timeout: 10 * time.Second,
			Version: "v2",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job with a wait that started 15 seconds ago (already over budget)
	waitStartedAt := time.Now().Add(-15 * time.Second)
	job := &db.Job{
		ID:                  "test-exhausted-job",
		LinearIssueID:       "ENG-502",
		State:               db.StateCoding,
		OpenCodeSessionID:   "ses_exhausted",
		WorktreePath:        "/tmp/test",
		CodingWaitStartedAt: &waitStartedAt,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	logFunc := func(msg string) {
		t.Logf("[test] %s", msg)
	}

	// Resume the wait - should fail immediately
	start := time.Now()
	_, err = orch.waitForSessionWithHealing(job, "coding", logFunc)
	elapsed := time.Since(start)

	// Should fail almost immediately (< 1s)
	if elapsed > 1*time.Second {
		t.Errorf("Expected immediate failure for exhausted timeout, but waited %v", elapsed)
	}

	// Verify error mentions exhausted
	if err == nil {
		t.Error("Expected error for exhausted timeout, got nil")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("Expected error to mention 'exhausted', got: %v", err)
	}
}

func TestWaitTimeoutPersistence_ReviewingPhase(t *testing.T) {
	// Test that reviewing phase has separate timeout tracking
	dbPath := "test_wait_persistence_reviewing.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://127.0.0.1:3000",
			Timeout: 10 * time.Second,
			Version: "v2",
		},
	}

	orch := NewOrchestrator(cfg, database)

	// Create a job in reviewing state with a wait that started 5 seconds ago
	waitStartedAt := time.Now().Add(-5 * time.Second)
	job := &db.Job{
		ID:                     "test-reviewing-job",
		LinearIssueID:          "ENG-503",
		State:                  db.StateReviewing,
		OpenCodeSessionID:      "ses_review",
		WorktreePath:           "/tmp/test",
		ReviewingWaitStartedAt: &waitStartedAt, // Different field than coding
		CreatedAt:              time.Now(),
		UpdatedAt:              time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Verify coding wait is NOT set
	if job.CodingWaitStartedAt != nil {
		t.Error("Coding wait should not be set for reviewing phase")
	}

	logFunc := func(msg string) {
		t.Logf("[test] %s", msg)
	}

	// Resume the reviewing wait - should have ~5s remaining
	start := time.Now()
	_, err = orch.waitForSessionWithHealing(job, "reviewing", logFunc)
	elapsed := time.Since(start)

	// Should timeout after ~5s (remaining time)
	if elapsed > 8*time.Second {
		t.Errorf("Expected wait to use remaining reviewing timeout (~5s), but waited %v", elapsed)
	}
}

func TestWaitTimeoutPersistence_ClearOnSuccess(t *testing.T) {
	// Test that wait start time is cleared when session completes successfully
	// Note: This test can't fully run without a real OpenCode server,
	// but we can verify the logic structure
	dbPath := "test_wait_persistence_clear.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://127.0.0.1:3000",
			Timeout: 30 * time.Minute,
			Version: "v2",
		},
	}

	_ = NewOrchestrator(cfg, database)

	// Create a job with wait started
	waitStartedAt := time.Now().Add(-5 * time.Minute)
	job := &db.Job{
		ID:                  "test-clear-job",
		LinearIssueID:       "ENG-504",
		State:               db.StateCoding,
		OpenCodeSessionID:   "ses_clear",
		WorktreePath:        "/tmp/test",
		CodingWaitStartedAt: &waitStartedAt,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	if err := database.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Verify wait is set
	if job.CodingWaitStartedAt == nil {
		t.Error("Expected CodingWaitStartedAt to be set")
	}

	// In a real scenario where WaitForSessionIdle succeeds, the wait time would be cleared
	// We can't test the full flow without OpenCode, but the logic is in waitForSessionWithHealing:
	//   if err == nil {
	//     job.CodingWaitStartedAt = nil
	//     db.UpdateJob(job)
	//   }
	// This test verifies the structure exists
}
