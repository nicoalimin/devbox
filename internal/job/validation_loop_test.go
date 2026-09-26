package job

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/linear"
	"github.com/nicoalimin/devbox/internal/validation"
)

const tsMissing = "Type 'InMemoryCatalogStore' is missing the following properties from type 'CatalogStore': createTransferDraft, addTransferItems, findTransferById, findTransferWithItems, and 5 more."

func tsOutput(root string) string {
	var b strings.Builder
	b.WriteString(root + "/src/stores/in-memory-catalog-store.ts(41,11): error TS6133: 'nextTransferId' is declared but its value is never read.\n")
	for i := 0; i < 5; i++ {
		b.WriteString(root + "/test/transfers.test.ts(1" + string(rune('0'+i)) + ",3): error TS2740: " + tsMissing + "\n")
	}
	return b.String()
}

// recordingIssues is a mocked Linear client that records comments.
type recordingIssues struct {
	mu       sync.Mutex
	comments []string
}

func (r *recordingIssues) GetIssue(id string) (*linear.Issue, error) {
	return &linear.Issue{ID: "issue-uuid", Identifier: "UTA-94", Title: "Warehouse transfer API"}, nil
}

func (r *recordingIssues) AddComment(issueID, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.comments = append(r.comments, issueID+"|"+body)
	return nil
}

func (r *recordingIssues) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.comments...)
}

func loopFixture(t *testing.T) (*Orchestrator, *recordingIssues) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	orch := NewOrchestrator(&config.Config{}, database)
	issues := &recordingIssues{}
	orch.linear = issues
	return orch, issues
}

func createLoopJob(t *testing.T, orch *Orchestrator, id string, state db.JobState, created time.Time, signature, summary string) *db.Job {
	t.Helper()
	job := &db.Job{ID: id, LinearIssueID: "UTA-94", State: state, CreatedAt: created, UpdatedAt: created,
		FailureSignature: signature, FailureSummary: summary,
		OperatorContext: "continue_pr: true\npush_ref: devbox/uta-94"}
	if err := orch.db.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	return job
}

func tsFailure(root string) *validation.Failure {
	return validation.NewFailure("pnpm run typecheck", errors.New("exit status 2"), []byte(tsOutput(root)), root)
}

func TestRepeatValidationFailureMarksStuckAndCommentsLinear(t *testing.T) {
	orch, issues := loopFixture(t)
	prevFailure := tsFailure("/repo/.devbox-worktrees/UTA-94")
	createLoopJob(t, orch, "prev", db.StateFailed, time.Now().Add(-time.Hour), prevFailure.Signature, "prior summary")
	job := createLoopJob(t, orch, "next", db.StateCoding, time.Now(), "", "")

	// Same errors from a different worktree path and order still match.
	orch.recordValidationFailure(job.ID, errors.Join(errors.New("wrapped"), tsFailure("/repo/.devbox-worktrees/UTA-94-2")))
	orch.failJob(job, "Failed to push branch: local validation failed after 3 repair attempts")

	stored, err := orch.db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != db.StateStuck || !stored.State.IsTerminal() {
		t.Fatalf("expected stuck, got %s (%s)", stored.State, stored.BlockerReason)
	}
	if stored.FailureSignature != prevFailure.Signature || !strings.HasPrefix(stored.BlockerReason, "Stuck:") {
		t.Fatalf("signature/reason not recorded: %+v", stored)
	}
	for _, want := range []string{"TS6133", "TS2740", "2 distinct"} {
		if !strings.Contains(stored.FailureSummary, want) {
			t.Fatalf("summary missing %q: %s", want, stored.FailureSummary)
		}
	}
	comments := issues.all()
	if len(comments) != 1 || !strings.HasPrefix(comments[0], "issue-uuid|") || !strings.Contains(comments[0], "stuck") ||
		!strings.Contains(comments[0], prevFailure.Signature) || !strings.Contains(comments[0], "TS2740") {
		t.Fatalf("expected one Linear stuck comment with deduped errors, got %q", comments)
	}
}

func TestDifferentValidationFailureIsNormalFailed(t *testing.T) {
	orch, issues := loopFixture(t)
	createLoopJob(t, orch, "prev", db.StateFailed, time.Now().Add(-time.Hour), "0000000000000000", "prior summary")
	job := createLoopJob(t, orch, "next", db.StateCoding, time.Now(), "", "")
	orch.recordValidationFailure(job.ID, tsFailure("/wt"))
	orch.failJob(job, "Failed to push branch")

	stored, _ := orch.db.GetJob(job.ID)
	if stored.State != db.StateFailed || stored.FailureSignature == "" {
		t.Fatalf("expected failed with signature, got %s %q", stored.State, stored.FailureSignature)
	}
	if got := issues.all(); len(got) != 0 {
		t.Fatalf("unexpected Linear comment: %q", got)
	}

	// A non-validation failure never counts as stuck and clears the signature.
	third := createLoopJob(t, orch, "third", db.StateCoding, time.Now().Add(time.Minute), "", "")
	orch.failJob(third, "OpenCode coding session timed out")
	stored, _ = orch.db.GetJob(third.ID)
	if stored.State != db.StateFailed || stored.FailureSignature != "" {
		t.Fatalf("non-validation failure: %s %q", stored.State, stored.FailureSignature)
	}
}

func TestContinuePromptInjectsPriorErrorsAsMandatoryStep1(t *testing.T) {
	orch, _ := loopFixture(t)
	prior := tsFailure("/wt")
	createLoopJob(t, orch, "prev", db.StateStuck, time.Now().Add(-time.Hour), prior.Signature, formatFailureSummary(prior))
	job := createLoopJob(t, orch, "next", db.StateCoding, time.Now(), "", "")

	priorErrors := orch.priorFailureSummary(job)
	if priorErrors == "" {
		t.Fatal("continue did not pick up prior job errors")
	}
	prompt := orch.buildCodingPrompt(&linear.Issue{Identifier: "UTA-94", Title: "Transfers"}, job.OperatorContext, priorErrors)
	if !strings.HasPrefix(prompt, "Step 1 (mandatory): fix these errors") {
		t.Fatalf("prompt does not start with mandatory step 1:\n%s", prompt)
	}
	for _, want := range []string{"TS6133", "'nextTransferId' is declared", "TS2740", tsMissing, "1. Complete Step 1 (mandatory)"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Count(prompt, "error TS2740") != 1 {
		t.Fatalf("prior errors not deduped in prompt:\n%s", prompt)
	}

	// Fresh (non-continue) assigns do not inherit prior errors.
	job.OperatorContext = ""
	if got := orch.priorFailureSummary(job); got != "" {
		t.Fatalf("non-continue job inherited prior errors: %q", got)
	}
	// Prior success means nothing to inject.
	orch2, _ := loopFixture(t)
	createLoopJob(t, orch2, "ok", db.StateDone, time.Now().Add(-time.Hour), "", "")
	if got := orch2.priorFailureSummary(createLoopJob(t, orch2, "n", db.StateCoding, time.Now(), "", "")); got != "" {
		t.Fatalf("unexpected injection after successful prior job: %q", got)
	}
}

func TestReviewPromptInjectsJobsPriorErrors(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	failure := tsFailure(job.WorktreePath)
	job.FailureSignature, job.FailureSummary = failure.Signature, formatFailureSummary(failure)
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	prompts := recordingDeliveryAgent(t, orch, job, false, func(int) {
		writeAgentWork(t, job)
	})
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"true"}
	if err := orch.ReviewJob(job.ID, "Please fix the store"); err != nil {
		t.Fatal(err)
	}
	sent := prompts()
	if len(sent) == 0 || !strings.Contains(sent[0], "Step 1 (mandatory): fix these errors") || !strings.Contains(sent[0], "TS2740") {
		t.Fatalf("review prompt missing prior errors: %q", sent)
	}
	stored, _ := orch.db.GetJob(job.ID)
	if stored.State != db.StatePROpen || stored.FailureSignature != "" || stored.FailureSummary != "" {
		t.Fatalf("successful review should clear failure fields: %+v", stored)
	}
}

func TestCodingPromptRequiresACChecklistAndStepwiseChecks(t *testing.T) {
	orch := &Orchestrator{}
	prompt := orch.buildCodingPrompt(&linear.Issue{Identifier: "UTA-97", Title: "Loop"}, "", "")
	if strings.Contains(prompt, "Step 1 (mandatory)") {
		t.Fatalf("fresh prompt should not contain prior-error step:\n%s", prompt)
	}
	for _, want := range []string{
		"Plan first: write an acceptance-criteria checklist",
		"After each step, run the repository's formatter, typecheck, and unit tests",
		"Self-review before finishing",
		"host pre-push validation gate",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRepairPromptContainsEveryDistinctError(t *testing.T) {
	var b strings.Builder
	b.WriteString(tsOutput("/wt"))
	b.WriteString("/wt/src/other.ts(3,3): error TS2304: Cannot find name 'transferRepo'.\n")
	failure := validation.NewFailure("pnpm run typecheck", errors.New("exit status 2"), []byte(b.String()), "/wt")
	prompt := buildValidationRepairPrompt(errors.Join(errors.New("local validation failed after 1 repair attempts"), failure))
	for _, e := range failure.TS.Errors {
		if !strings.Contains(prompt, e.Code+": "+strings.SplitN(e.Message, "\n", 2)[0]) {
			t.Fatalf("repair prompt missing %s:\n%s", e.Code, prompt)
		}
	}
	if failure.TS.Distinct() != 3 || strings.Contains(prompt, "output truncated") {
		t.Fatalf("unexpected summary (%d distinct):\n%s", failure.TS.Distinct(), prompt)
	}
}

func TestValidationRepairFallsBackToCodexAfterThreeOpenCodeAttempts(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	prompts := recordingDeliveryAgent(t, orch, job, false, nil)
	var codexCalls int
	orch.codexExecFn = func(worktreePath, prompt string, timeout time.Duration) ([]byte, error) {
		codexCalls++
		if !strings.Contains(prompt, "local pre-push validation failed") {
			t.Fatalf("Codex prompt missing validation failure: %q", prompt)
		}
		if err := os.WriteFile(filepath.Join(worktreePath, "output.txt"), []byte("fixed by codex\n"), 0644); err != nil {
			t.Fatal(err)
		}
		return []byte("repair complete"), nil
	}

	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := len(prompts()); got != maxOpenCodeRepairAttempts {
		t.Fatalf("OpenCode repair calls = %d, want %d", got, maxOpenCodeRepairAttempts)
	}
	if codexCalls != 1 {
		t.Fatalf("Codex repair calls = %d, want 1", codexCalls)
	}
	logs := jobLogText(t, orch, job.ID)
	if !strings.Contains(logs, "Codex repair attempt 4/6") || !strings.Contains(logs, "Codex repair output") {
		t.Fatalf("missing Codex fallback logs:\n%s", logs)
	}
}

func TestValidationRepairStopsAfterSixAttempts(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	prompts := recordingDeliveryAgent(t, orch, job, false, nil)
	var codexCalls int
	orch.codexExecFn = func(string, string, time.Duration) ([]byte, error) {
		codexCalls++
		return []byte("no fix"), nil
	}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"exit 9"}

	err := orch.pushBranch(job)
	if err == nil || !strings.Contains(err.Error(), "failed after 6 repair attempts") {
		t.Fatalf("expected six-attempt failure, got %v", err)
	}
	if got := len(prompts()); got != maxOpenCodeRepairAttempts {
		t.Fatalf("OpenCode repair calls = %d, want %d", got, maxOpenCodeRepairAttempts)
	}
	if codexCalls != maxCodexRepairAttempts {
		t.Fatalf("Codex repair calls = %d, want %d", codexCalls, maxCodexRepairAttempts)
	}
}
