package job

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
)

var errTestReconciler = errors.New("simulated gh failure")

func newReconcilerTestOrchestrator(t *testing.T, cfg *config.Config) *Orchestrator {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "reconciler-test.db"))
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return NewOrchestrator(cfg, database)
}

func reconcilerTestConfig() *config.Config {
	return &config.Config{
		Linear: config.LinearConfig{APIKey: "test-key"},
		OpenCode: config.OpenCodeConfig{
			BaseURL: "http://127.0.0.1:3000",
			Timeout: 30 * time.Minute,
		},
		Reconciler: config.ReconcilerConfig{
			Enabled:  true,
			Interval: 2 * time.Minute,
		},
	}
}

func createReconcilerTestJob(t *testing.T, orch *Orchestrator, id string, state db.JobState, prURL string) {
	t.Helper()
	job := &db.Job{
		ID:            id,
		LinearIssueID: "ENG-123",
		State:         state,
		PRURL:         prURL,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := orch.db.CreateJob(job); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
}

func TestReconcileJobs_SkipsNonPROpenJobs(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())

	// Stub: every PR lookup reports merged — non-pr_open jobs must still be untouched.
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return prStatusMerged, 1, nil
	}

	for _, state := range []db.JobState{
		db.StateCoding, db.StateReviewing, db.StatePushing,
		db.StateDone, db.StateFailed, db.StateCancelled, db.StateBlocked,
	} {
		createReconcilerTestJob(t, orch, "job-"+string(state), state, "https://github.com/o/r/pull/1")
	}

	orch.reconcileJobs()

	for _, state := range []db.JobState{
		db.StateCoding, db.StateReviewing, db.StatePushing,
		db.StateDone, db.StateFailed, db.StateCancelled, db.StateBlocked,
	} {
		job, err := orch.db.GetJob("job-" + string(state))
		if err != nil {
			t.Fatalf("Failed to get job: %v", err)
		}
		if job.State != state {
			t.Errorf("Expected job in state %s to stay %s, got %s", state, state, job.State)
		}
		if job.CompletedAt != nil {
			t.Errorf("Expected job in state %s to have no CompletedAt", state)
		}
	}
}

func TestReconcileJobs_SkipsMissingPRURL(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())

	called := false
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		called = true
		return prStatusMerged, 1, nil
	}

	createReconcilerTestJob(t, orch, "job-no-url", db.StatePROpen, "")

	orch.reconcileJobs()

	if called {
		t.Error("Expected getPRStatus not to be called for job without pr_url")
	}
	job, err := orch.db.GetJob("job-no-url")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StatePROpen {
		t.Errorf("Expected job without pr_url to stay pr_open, got %s", job.State)
	}
}

func TestReconcileJobs_MergedToDone(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return prStatusMerged, 42, nil
	}

	createReconcilerTestJob(t, orch, "job-merged", db.StatePROpen, "https://github.com/o/r/pull/42")

	orch.reconcileJobs()

	job, err := orch.db.GetJob("job-merged")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StateDone {
		t.Errorf("Expected merged PR job to be done, got %s", job.State)
	}
	if job.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set on done job")
	}

	logs, err := orch.db.GetLogs("job-merged", 0)
	if err != nil {
		t.Fatalf("Failed to get logs: %v", err)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "merged") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected job log mentioning merged PR")
	}
}

func TestReconcileJobs_ClosedToCancelled(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return prStatusClosed, 7, nil
	}

	createReconcilerTestJob(t, orch, "job-closed", db.StatePROpen, "https://github.com/o/r/pull/7")

	orch.reconcileJobs()

	job, err := orch.db.GetJob("job-closed")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StateCancelled {
		t.Errorf("Expected closed PR job to be cancelled, got %s", job.State)
	}
	if job.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set on cancelled job")
	}
	if job.BlockerReason == "" {
		t.Error("Expected BlockerReason to be set on cancelled job")
	}
	if !strings.Contains(job.BlockerReason, "closed without merge") {
		t.Errorf("Expected BlockerReason to mention closed without merge, got %q", job.BlockerReason)
	}
}

func TestReconcileJobs_NotFoundToCancelled(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return prStatusNotFound, 0, nil
	}

	createReconcilerTestJob(t, orch, "job-gone", db.StatePROpen, "https://github.com/o/r/pull/999")

	orch.reconcileJobs()

	job, err := orch.db.GetJob("job-gone")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StateCancelled {
		t.Errorf("Expected not-found PR job to be cancelled, got %s", job.State)
	}
	if job.BlockerReason == "" {
		t.Error("Expected BlockerReason to be set for not-found PR")
	}

	logs, err := orch.db.GetLogs("job-gone", 0)
	if err != nil {
		t.Fatalf("Failed to get logs: %v", err)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "not found") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected job log mentioning PR not found")
	}
}

func TestReconcileJobs_OpenUnchanged(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return prStatusOpen, 3, nil
	}

	createReconcilerTestJob(t, orch, "job-open", db.StatePROpen, "https://github.com/o/r/pull/3")

	orch.reconcileJobs()

	job, err := orch.db.GetJob("job-open")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StatePROpen {
		t.Errorf("Expected open PR job to stay pr_open, got %s", job.State)
	}
	if job.CompletedAt != nil {
		t.Error("Expected open PR job to have no CompletedAt")
	}
}

func TestReconcileJobs_LookupErrorLeavesJobUnchanged(t *testing.T) {
	orch := newReconcilerTestOrchestrator(t, reconcilerTestConfig())
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return "", 0, errTestReconciler
	}

	createReconcilerTestJob(t, orch, "job-err", db.StatePROpen, "https://github.com/o/r/pull/5")

	orch.reconcileJobs()

	job, err := orch.db.GetJob("job-err")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StatePROpen {
		t.Errorf("Expected job with lookup error to stay pr_open, got %s", job.State)
	}
}

func TestStartReconciler_DisabledReturnsImmediately(t *testing.T) {
	cfg := reconcilerTestConfig()
	cfg.Reconciler.Enabled = false
	orch := newReconcilerTestOrchestrator(t, cfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		orch.StartReconciler()
	}()

	select {
	case <-done:
		// Disabled reconciler must not run the loop.
	case <-time.After(2 * time.Second):
		t.Fatal("StartReconciler with disabled config did not return")
	}
}

func TestReconcileJobs_DisabledDoesNothing(t *testing.T) {
	cfg := reconcilerTestConfig()
	cfg.Reconciler.Enabled = false
	orch := newReconcilerTestOrchestrator(t, cfg)
	orch.prStatusFn = func(prURL string) (prStatus, int, error) {
		return prStatusMerged, 1, nil
	}

	createReconcilerTestJob(t, orch, "job-disabled", db.StatePROpen, "https://github.com/o/r/pull/1")

	orch.reconcileJobs()

	job, err := orch.db.GetJob("job-disabled")
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if job.State != db.StatePROpen {
		t.Errorf("Expected disabled reconciler to leave job in pr_open, got %s", job.State)
	}
}

func TestPRNumberFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"https://github.com/owner/repo/pull/123", 123},
		{"https://github.com/owner/repo/pull/1", 1},
		{"https://example.com/not-a-pr", 0},
		{"", 0},
		{"https://github.com/owner/repo/pull/abc", 0},
	}
	for _, c := range cases {
		if got := prNumberFromURL(c.url); got != c.want {
			t.Errorf("prNumberFromURL(%q) = %d, want %d", c.url, got, c.want)
		}
	}
}
