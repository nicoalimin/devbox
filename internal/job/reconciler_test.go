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

func newReconcilerTestOrchestrator(t *testing.T, cfg *config.Config) (*Orchestrator, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "reconciler-test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if cfg == nil {
		cfg = &config.Config{
			OpenCode: config.OpenCodeConfig{
				BaseURL: "http://127.0.0.1:3000",
				Version: "v2",
				Timeout: 30 * time.Minute,
			},
			Reconciler: config.ReconcilerConfig{
				Enabled:  true,
				Interval: 2 * time.Minute,
			},
		}
	}
	return NewOrchestrator(cfg, database), database
}

func TestReconcilerSkipsNonPROpenJobs(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	calls := 0
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		calls++
		return PRStatusMerged, 1, nil
	}

	states := []db.JobState{
		db.StateCoding, db.StateReviewing, db.StatePushing,
		db.StateDone, db.StateFailed, db.StateCancelled,
		db.StateBlocked, db.StateQueued, db.StateFetching, db.StatePreparing,
	}
	for i, st := range states {
		j := &db.Job{
			ID:            string(rune('a'+i)) + "-job",
			LinearIssueID: "ENG-1",
			State:         st,
			PRURL:         "https://github.com/o/r/pull/1",
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}
		if st == db.StateDone || st == db.StateFailed || st == db.StateCancelled {
			now := time.Now()
			j.CompletedAt = &now
		}
		if err := database.CreateJob(j); err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
	}

	orch.reconcileJobs()

	if calls != 0 {
		t.Errorf("Expected getPRStatus to never be called for non-pr_open jobs, got %d calls", calls)
	}
	for i, st := range states {
		id := string(rune('a'+i)) + "-job"
		got, err := database.GetJob(id)
		if err != nil {
			t.Fatalf("Failed to get job %s: %v", id, err)
		}
		if got.State != st {
			t.Errorf("Job %s: expected state %s unchanged, got %s", id, st, got.State)
		}
	}
}

func TestReconcilerSkipsMissingPRURL(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	called := false
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		called = true
		return PRStatusMerged, 1, nil
	}

	for _, prURL := range []string{"", "   "} {
		j := &db.Job{
			ID:            "job-missing-" + string(rune(len(prURL))),
			LinearIssueID: "ENG-1",
			State:         db.StatePROpen,
			PRURL:         prURL,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}
		if err := database.CreateJob(j); err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
	}

	orch.reconcileJobs()

	if called {
		t.Error("Expected getPRStatus not to be called for jobs without pr_url")
	}
	jobs, _ := database.ListJobs(0)
	for _, j := range jobs {
		if j.State != db.StatePROpen {
			t.Errorf("Job %s without pr_url was mutated to %s", j.ID, j.State)
		}
		if j.CompletedAt != nil {
			t.Errorf("Job %s without pr_url should not have CompletedAt set", j.ID)
		}
	}
}

func TestReconcilerDisabledDoesNotRunLoop(t *testing.T) {
	cfg := &config.Config{
		OpenCode: config.OpenCodeConfig{BaseURL: "http://127.0.0.1:3000", Version: "v2"},
		Reconciler: config.ReconcilerConfig{
			Enabled:  false,
			Interval: time.Millisecond,
		},
	}
	orch, database := newReconcilerTestOrchestrator(t, cfg)
	calls := 0
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		calls++
		return PRStatusMerged, 1, nil
	}
	j := &db.Job{
		ID: "job-disabled", LinearIssueID: "ENG-1",
		State: db.StatePROpen, PRURL: "https://github.com/o/r/pull/9",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.CreateJob(j); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	stop := make(chan struct{})
	close(stop)
	done := make(chan struct{})
	go func() { defer close(done); orch.StartReconcilerWithStop(stop) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartReconcilerWithStop with disabled config did not return promptly")
	}

	// Give any stray goroutine a moment, then verify no mutation.
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Errorf("Disabled reconciler should not call getPRStatus, got %d calls", calls)
	}
	got, _ := database.GetJob("job-disabled")
	if got.State != db.StatePROpen {
		t.Errorf("Disabled reconciler mutated job to %s", got.State)
	}
}

func TestReconcilerDefaultIntervalAndEnabled(t *testing.T) {
	// Zero-value reconciler on a manually-built config falls back to 2m.
	orch, _ := newReconcilerTestOrchestrator(t, &config.Config{
		OpenCode: config.OpenCodeConfig{BaseURL: "http://127.0.0.1:3000"},
	})
	if got := orch.getReconcilerInterval(); got != 2*time.Minute {
		t.Errorf("Expected default interval 2m, got %s", got)
	}

	// Custom interval is respected.
	orch2, _ := newReconcilerTestOrchestrator(t, &config.Config{
		OpenCode:   config.OpenCodeConfig{BaseURL: "http://127.0.0.1:3000"},
		Reconciler: config.ReconcilerConfig{Enabled: true, Interval: 90 * time.Second},
	})
	if got := orch2.getReconcilerInterval(); got != 90*time.Second {
		t.Errorf("Expected custom interval 90s, got %s", got)
	}
}

func TestReconcilerMergedToDone(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		return PRStatusMerged, 42, nil
	}
	j := &db.Job{
		ID: "job-merged", LinearIssueID: "ENG-1",
		State: db.StatePROpen, PRURL: "https://github.com/o/r/pull/42",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.CreateJob(j); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	orch.reconcileJobs()

	got, _ := database.GetJob("job-merged")
	if got.State != db.StateDone {
		t.Errorf("Expected state done, got %s", got.State)
	}
	if got.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set on merge")
	}
	logs, err := database.GetLogs("job-merged", 10)
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
		t.Errorf("Expected job log mentioning merge, got %+v", logs)
	}
}

func TestReconcilerClosedToCancelled(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		return PRStatusClosed, 43, nil
	}
	j := &db.Job{
		ID: "job-closed", LinearIssueID: "ENG-1",
		State: db.StatePROpen, PRURL: "https://github.com/o/r/pull/43",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.CreateJob(j); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	orch.reconcileJobs()

	got, _ := database.GetJob("job-closed")
	if got.State != db.StateCancelled {
		t.Errorf("Expected state cancelled, got %s", got.State)
	}
	if got.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set on close")
	}
	if !strings.Contains(got.BlockerReason, "closed without merge") {
		t.Errorf("Expected blocker reason 'PR closed without merge', got %q", got.BlockerReason)
	}
}

func TestReconcilerOpenUnchanged(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		return PRStatusOpen, 44, nil
	}
	j := &db.Job{
		ID: "job-open", LinearIssueID: "ENG-1",
		State: db.StatePROpen, PRURL: "https://github.com/o/r/pull/44",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.CreateJob(j); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	orch.reconcileJobs()

	got, _ := database.GetJob("job-open")
	if got.State != db.StatePROpen {
		t.Errorf("Expected open PR job to stay pr_open, got %s", got.State)
	}
	if got.CompletedAt != nil {
		t.Error("Open PR job should not have CompletedAt set")
	}
}

func TestReconcilerNotFoundToCancelled(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		return "", 0, ErrPRNotFound
	}
	j := &db.Job{
		ID: "job-notfound", LinearIssueID: "ENG-1",
		State: db.StatePROpen, PRURL: "https://github.com/o/r/pull/999999",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.CreateJob(j); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	orch.reconcileJobs()

	got, _ := database.GetJob("job-notfound")
	if got.State != db.StateCancelled {
		t.Errorf("Expected not-found PR job to be cancelled, got %s", got.State)
	}
	if got.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set on not-found")
	}
}

func TestReconcilerTransientErrorSkips(t *testing.T) {
	orch, database := newReconcilerTestOrchestrator(t, nil)
	boom := errors.New("gh rate limited")
	orch.prStatusFunc = func(prURL string) (PRStatus, int, error) {
		return "", 0, boom
	}
	j := &db.Job{
		ID: "job-err", LinearIssueID: "ENG-1",
		State: db.StatePROpen, PRURL: "https://github.com/o/r/pull/45",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.CreateJob(j); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	orch.reconcileJobs()

	got, _ := database.GetJob("job-err")
	if got.State != db.StatePROpen {
		t.Errorf("Transient error should not mutate job, got %s", got.State)
	}
	if got.CompletedAt != nil {
		t.Error("Transient error should not set CompletedAt")
	}
}
