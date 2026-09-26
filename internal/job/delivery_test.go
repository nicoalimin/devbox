package job

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	gitmanager "github.com/nicoalimin/devbox/internal/git"
	"github.com/nicoalimin/devbox/internal/linear"
	"github.com/nicoalimin/devbox/internal/opencode"
)

func deliveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func deliveryFixture(t *testing.T) (*Orchestrator, *db.Job, string) {
	t.Helper()
	repo, remote := t.TempDir(), t.TempDir()
	deliveryGit(t, remote, "init", "--bare")
	deliveryGit(t, repo, "init", "-b", "main")
	deliveryGit(t, repo, "config", "user.name", "Test")
	deliveryGit(t, repo, "config", "user.email", "test@example.com")
	deliveryGit(t, repo, "commit", "--allow-empty", "-m", "initial")
	deliveryGit(t, repo, "remote", "add", "origin", remote)
	deliveryGit(t, repo, "push", "origin", "main")
	manager := gitmanager.NewManager(repo, "main")
	worktree, err := manager.CreateWorktree("TEST-1")
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	job := &db.Job{ID: "job", LinearIssueID: "TEST-1", RepoPath: repo,
		WorktreePath: worktree.Path, BranchName: worktree.BranchName,
		State: db.StatePROpen, PRURL: "https://example.com/pull/1", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := database.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{GitHub: config.GitHubConfig{DefaultBaseBranch: "wrong-default"},
		OpenCode: config.OpenCodeConfig{Timeout: 100 * time.Millisecond, ReviewTimeout: 100 * time.Millisecond},
		Repos:    []config.RepoConfig{{Repo: config.RepoInfo{Path: repo, BaseBranch: "main", ValidationCommands: []string{"test -f output.txt"}, FormatCommands: []string{}}}}}
	orch := NewOrchestrator(cfg, database)
	orch.linear = deliveryIssues{}
	// Tests that exhaust OpenCode repairs must never invoke the developer's
	// real local Codex installation.
	orch.codexExecFn = func(string, string, time.Duration) ([]byte, error) { return nil, nil }
	return orch, job, remote
}

type deliveryIssues struct{}

func (deliveryIssues) GetIssue(string) (*linear.Issue, error) {
	return &linear.Issue{Identifier: "TEST-1"}, nil
}
func (deliveryIssues) AddComment(string, string) error { return nil }

type titledIssues struct{ title string }

func (i titledIssues) GetIssue(string) (*linear.Issue, error) {
	return &linear.Issue{Identifier: "TEST-1", Title: i.title}, nil
}
func (titledIssues) AddComment(string, string) error { return nil }

// The model writes files but never commits. Optionally it also times out.
func deliveryAgent(t *testing.T, orch *Orchestrator, job *db.Job, timeout bool, repair func(int)) {
	t.Helper()
	recordingDeliveryAgent(t, orch, job, timeout, repair)
}

// recordingDeliveryAgent is deliveryAgent that also returns the prompt bodies
// the orchestrator sent, in order.
func recordingDeliveryAgent(t *testing.T, orch *Orchestrator, job *db.Job, timeout bool, repair func(int)) func() []string {
	t.Helper()
	var mu sync.Mutex
	active := false
	prompts := 0
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/session":
			fmt.Fprint(w, `{"data":{"id":"session"}}`)
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			active = true
			prompts++
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(body))
			if repair != nil {
				repair(prompts)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/session/active":
			if active {
				fmt.Fprint(w, `{"data":{"session":{}}}`)
			} else {
				fmt.Fprint(w, `{"data":{}}`)
			}
		case strings.HasSuffix(r.URL.Path, "/wait"):
			if timeout {
				w.WriteHeader(http.StatusGatewayTimeout)
			} else {
				active = false
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			active = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	orch.opencode = opencode.NewClient(server.URL, "", "", "v2")
	job.OpenCodeSessionID = "session"
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func TestReviewDeliveryCommitsUntrackedFilesAfterAgentTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			orch, job, remote := deliveryFixture(t)
			deliveryAgent(t, orch, job, timeout, func(int) {
				if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("review changes\n"), 0644); err != nil {
					t.Error(err)
				}
			})
			if err := orch.ReviewJob(job.LinearIssueID, "Finish the implementation"); err != nil {
				t.Fatal(err)
			}
			stored, err := orch.db.GetJob(job.ID)
			if err != nil || stored.State != db.StatePROpen || stored.BlockerReason != "" {
				t.Fatalf("job: %+v, err: %v", stored, err)
			}
			if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "review changes" {
				t.Fatal(got)
			}
			if got := deliveryGit(t, job.WorktreePath, "status", "--porcelain"); got != "" {
				t.Fatalf("left dirty: %s", got)
			}
		})
	}
}

func TestRepairTimeoutRechecksActualFiles(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	deliveryAgent(t, orch, job, true, func(int) {
		if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("fixed"), 0644); err != nil {
			t.Error(err)
		}
	})
	manager := gitmanager.NewManager(job.RepoPath, orch.baseBranch(job))
	if err := orch.validateWithOpenCodeRepair(job, manager); err != nil {
		t.Fatalf("repair succeeded before timeout but was rejected: %v", err)
	}
}

func TestFailedReviewPublishesCheckpointAndReportsFailure(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"exit 9"}
	deliveryAgent(t, orch, job, false, func(int) {
		if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("partial"), 0644); err != nil {
			t.Error(err)
		}
	})
	if err := orch.ReviewJob(job.ID, "Fix it"); err == nil {
		t.Fatal("broken validation reported success")
	}
	stored, err := orch.db.GetJob(job.ID)
	if err != nil || stored.State != db.StateFailed || !strings.Contains(stored.BlockerReason, "6 repair attempts") || !strings.Contains(stored.BlockerReason, "committed and pushed") {
		t.Fatalf("job: %+v, err: %v", stored, err)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "partial" {
		t.Fatal(got)
	}
	if stored.FailureSignature == "" || !strings.Contains(stored.FailureSummary, "exit 9") {
		t.Fatalf("validation failure signature/summary not recorded: %+v", stored)
	}
	// UTA-96: jobs with an open PR preserve the worktree so continue/review can reattach.
	if _, err := os.Stat(job.WorktreePath); err != nil {
		t.Fatalf("expected worktree preserved for open-PR iteration: %v", err)
	}
}

func TestFailedPushPreservesCommittedWorktree(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	orch.failJob(job, "validation failed")
	stored, _ := orch.db.GetJob(job.ID)
	if !strings.Contains(stored.BlockerReason, "recovery incomplete") {
		t.Fatal(stored.BlockerReason)
	}
	if got := deliveryGit(t, job.WorktreePath, "status", "--porcelain"); got != "" {
		t.Fatal(got)
	}
	if got := deliveryGit(t, job.WorktreePath, "show", "HEAD:output.txt"); got != "partial" {
		t.Fatal(got)
	}
}

func TestReviewAcceptanceIsDurableAndRejectsDuplicates(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	accepted, err := orch.acceptReview(job.LinearIssueID, "Persisted feedback")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := orch.db.GetJob(accepted.ID)
	if stored.State != db.StateReviewing || stored.ReviewFeedback != "Persisted feedback" {
		t.Fatalf("not durable: %+v", stored)
	}
	if _, err := orch.acceptReview(job.ID, "Duplicate"); err == nil {
		t.Fatal("concurrent review accepted")
	}
	if _, err := orch.CreateJob("TEST-2", ""); err == nil {
		t.Fatal("assignment accepted during review")
	}
}

// writeAgentWork simulates uncommitted agent output in the worktree.
func writeAgentWork(t *testing.T, job *db.Job) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(job.WorktreePath, "feature.txt"), []byte("agent work\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestHostFormattingRunsBeforeValidation(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	writeAgentWork(t, job)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\n' > output.txt"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"test \"$(cat output.txt)\" = formatted"}
	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "formatted" {
		t.Fatal(got)
	}
	if got := deliveryGit(t, remote, "log", "-1", "--format=%s", job.BranchName); got != "[TEST-1] Automated implementation changes" {
		t.Fatalf("work commit subject = %q", got)
	}
}

// UTA-97: agent code + formatter output land in ONE meaningful commit.
func TestAgentWorkAndFormattingFoldedIntoSingleCommit(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	orch.linear = titledIssues{title: "Warehouse transfer API"}
	writeAgentWork(t, job)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\n' > output.txt && printf 'agent work formatted\n' > feature.txt"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"test -f output.txt"}
	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, remote, "rev-list", "--count", "main.."+job.BranchName); got != "1" {
		t.Fatalf("expected exactly one delivery commit, got %s", got)
	}
	subject := deliveryGit(t, remote, "log", "-1", "--format=%s", job.BranchName)
	if subject != "[TEST-1] Warehouse transfer API" || strings.Contains(subject, "chore: format") {
		t.Fatalf("commit subject = %q", subject)
	}
	files := deliveryGit(t, remote, "show", "--name-only", "--format=", job.BranchName)
	if !strings.Contains(files, "feature.txt") || !strings.Contains(files, "output.txt") {
		t.Fatalf("commit does not contain both agent and formatter changes: %q", files)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":feature.txt"); got != "agent work formatted" {
		t.Fatal(got)
	}
}

// publishedFixture pushes one existing commit so the remote PR branch is at
// HEAD, as on a continue of an open PR.
func publishedFixture(t *testing.T) (*Orchestrator, *db.Job, string, string) {
	t.Helper()
	orch, job, remote := deliveryFixture(t)
	if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("unformatted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	deliveryGit(t, job.WorktreePath, "add", "output.txt")
	deliveryGit(t, job.WorktreePath, "commit", "-m", "[TEST-1] earlier work")
	deliveryGit(t, job.WorktreePath, "push", "origin", job.BranchName)
	head := deliveryGit(t, job.WorktreePath, "rev-parse", "HEAD")
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\n' > output.txt"}
	return orch, job, remote, head
}

func jobLogText(t *testing.T, orch *Orchestrator, jobID string) string {
	t.Helper()
	logs, err := orch.db.GetLogs(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range logs {
		b.WriteString(l.Message + "\n")
	}
	return b.String()
}

// UTA-97: a format-only diff (agent changed nothing) makes no commit and no push.
func TestFormatOnlyDiffMakesNoCommitAndNoPush(t *testing.T) {
	orch, job, remote, head := publishedFixture(t)
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"true"}
	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, job.WorktreePath, "rev-parse", "HEAD"); got != head {
		t.Fatalf("format-only diff created a commit: %s != %s", got, head)
	}
	if got := deliveryGit(t, remote, "rev-parse", "refs/heads/"+job.BranchName); got != head {
		t.Fatalf("remote moved: %s", got)
	}
	if got := deliveryGit(t, job.WorktreePath, "status", "--porcelain"); got != "" {
		t.Fatalf("format-only output left in worktree: %s", got)
	}
	logs := jobLogText(t, orch, job.ID)
	if !strings.Contains(logs, "format-only") || !strings.Contains(logs, "skipping push") || strings.Contains(logs, "pushing branch to remote") {
		t.Fatalf("expected format-only no-push path, logs:\n%s", logs)
	}
}

// UTA-97: the failure checkpoint must not commit or push a format-only diff.
func TestFormatOnlyDiffCheckpointDoesNotCommitOrPush(t *testing.T) {
	orch, job, remote, head := publishedFixture(t)
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"exit 3"}
	job.OpenCodeSessionID = "" // no repair attempts
	err := orch.pushBranch(job)
	if err == nil {
		t.Fatal("expected validation failure")
	}
	orch.failJob(job, fmt.Sprintf("Failed to push branch: %v", err))
	stored, _ := orch.db.GetJob(job.ID)
	if stored.State != db.StateFailed || strings.Contains(stored.BlockerReason, "committed and pushed") {
		t.Fatalf("job: %s %q", stored.State, stored.BlockerReason)
	}
	if got := deliveryGit(t, job.WorktreePath, "rev-parse", "HEAD"); got != head {
		t.Fatalf("checkpoint committed format-only diff: %s", got)
	}
	if got := deliveryGit(t, remote, "rev-parse", "refs/heads/"+job.BranchName); got != head {
		t.Fatalf("remote moved: %s", got)
	}
	if logs := jobLogText(t, orch, job.ID); !strings.Contains(logs, "skipping push") {
		t.Fatalf("expected checkpoint push skip, logs:\n%s", logs)
	}
}

func TestHostFormattingDoesNotCreateEmptyCommit(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"true"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{}
	if err := os.WriteFile(filepath.Join(job.WorktreePath, "feature.txt"), []byte("done\n"), 0644); err != nil {
		t.Fatal(err)
	}
	deliveryGit(t, job.WorktreePath, "add", "feature.txt")
	deliveryGit(t, job.WorktreePath, "commit", "-m", "feature: complete work")
	head := deliveryGit(t, job.WorktreePath, "rev-parse", "HEAD")
	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, job.WorktreePath, "rev-parse", "HEAD"); got != head {
		t.Fatalf("clean formatter created commit: got %s, want %s", got, head)
	}
}

func TestHostFormattingCommitDoesNotBypassValidation(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	writeAgentWork(t, job)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\\n' > output.txt"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"exit 7"}
	if err := orch.pushBranch(job); err == nil || !strings.Contains(err.Error(), "local validation failed") {
		t.Fatalf("expected validation failure after formatting, got %v", err)
	}
	if got := deliveryGit(t, job.WorktreePath, "log", "-1", "--format=%s"); got != "[TEST-1] Automated implementation changes" {
		t.Fatalf("work + format changes were not committed before failed check: %q", got)
	}
}

func TestHostFormattingReportsCommitFailure(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	writeAgentWork(t, job)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\\n' > output.txt"}
	if err := os.WriteFile(filepath.Join(job.RepoPath, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	err := orch.pushBranch(job)
	if err == nil || !strings.Contains(err.Error(), "failed to commit host-formatted worktree") {
		t.Fatalf("expected clear host commit error, got %v", err)
	}
}

func TestHostDeliveryFormatsAndCommitsWithoutOpenCode(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	writeAgentWork(t, job)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\\n' > output.txt"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"test \"$(cat output.txt)\" = formatted"}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	orch.opencode = opencode.NewClient(server.URL, "", "", "v2")
	job.OpenCodeSessionID = "session-that-must-not-be-used"

	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("host delivery contacted OpenCode %d times", got)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "formatted" {
		t.Fatal(got)
	}
	if subject := deliveryGit(t, job.WorktreePath, "show", "-s", "--format=%s", "HEAD"); subject != "[TEST-1] Automated implementation changes" {
		t.Fatalf("commit subject = %q", subject)
	}
}

func TestAcceptedReviewResumesBeforeSessionCreation(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	deliveryAgent(t, orch, job, false, func(int) {
		if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("resumed"), 0644); err != nil {
			t.Error(err)
		}
	})
	job.OpenCodeSessionID = ""
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.acceptReview(job.ID, "Finish after restart"); err != nil {
		t.Fatal(err)
	}
	stored, err := orch.db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	orch.resumeJobFromState(stored)
	stored, _ = orch.db.GetJob(job.ID)
	if stored.State != db.StatePROpen {
		t.Fatalf("review not resumed: %+v", stored)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "resumed" {
		t.Fatal(got)
	}
}

func TestUnsafeInterruptionPreservesUncommittedOutput(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("still editing"), 0644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	orch.opencode = opencode.NewClient(server.URL, "", "", "v2")
	job.OpenCodeSessionID = "session"
	orch.failJob(job, "review timeout")
	stored, _ := orch.db.GetJob(job.ID)
	if !strings.Contains(stored.BlockerReason, "cannot stop agent safely") {
		t.Fatal(stored.BlockerReason)
	}
	if got := deliveryGit(t, job.WorktreePath, "status", "--porcelain"); !strings.Contains(got, "output.txt") {
		t.Fatalf("output changed while model may be active: %s", got)
	}
}

func TestRejectedCommitPreservesOutputAndReportsRecoveryFailure(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	if err := os.WriteFile(filepath.Join(job.WorktreePath, "output.txt"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job.RepoPath, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	orch.failJob(job, "repair exhausted")
	stored, _ := orch.db.GetJob(job.ID)
	if !strings.Contains(stored.BlockerReason, "failed to commit") || !strings.Contains(stored.BlockerReason, "recovery incomplete") {
		t.Fatal(stored.BlockerReason)
	}
	if got := deliveryGit(t, job.WorktreePath, "status", "--porcelain"); !strings.Contains(got, "output.txt") {
		t.Fatalf("lost staged output: %s", got)
	}
}

func TestCancelJobStopsOpenCodeBeforeWorktreeCleanup(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	job.State = db.StateCoding
	job.PRURL = ""
	job.CompletedAt = nil
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []string
	active := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			events = append(events, "interrupt")
			active = false
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/session/active":
			events = append(events, "active")
			if active {
				fmt.Fprint(w, `{"data":{"session":{}}}`)
			} else {
				fmt.Fprint(w, `{"data":{}}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	orch.opencode = opencode.NewClient(server.URL, "", "", "v2")
	job.OpenCodeSessionID = "session"
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}

	if err := orch.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) == 0 || got[0] != "interrupt" {
		t.Fatalf("expected StopSession interrupt before cleanup, events=%v", got)
	}

	stored, err := orch.db.GetJob(job.ID)
	if err != nil || stored.State != db.StateCancelled {
		t.Fatalf("job: %+v err=%v", stored, err)
	}
	if _, err := os.Stat(job.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed after cancel: %v", err)
	}
}

func TestCancelDuringCodingWaitRemainsCancelled(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	job.State = db.StateCoding
	job.PRURL = ""
	job.CompletedAt = nil
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	active := true
	interrupted := false
	waitStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			mu.Lock()
			interrupted = true
			active = false
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/session/active":
			mu.Lock()
			busy := active
			mu.Unlock()
			if busy {
				fmt.Fprint(w, `{"data":{"session":{}}}`)
			} else {
				fmt.Fprint(w, `{"data":{}}`)
			}
		case strings.HasSuffix(r.URL.Path, "/wait"):
			select {
			case waitStarted <- struct{}{}:
			default:
			}
			// Block until CancelJob interrupts the session.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				done := interrupted
				mu.Unlock()
				if done {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			w.WriteHeader(http.StatusGatewayTimeout)
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	orch.opencode = opencode.NewClient(server.URL, "", "", "v2")
	job.OpenCodeSessionID = "session"
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}

	beforeRefs := deliveryGit(t, remote, "for-each-ref", "--format=%(refname)")

	done := make(chan struct{})
	go func() {
		defer close(done)
		fresh, err := orch.db.GetJob(job.ID)
		if err != nil {
			t.Errorf("reload job: %v", err)
			return
		}
		orch.resumeJobFromState(fresh)
	}()

	select {
	case <-waitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("coding wait never started")
	}

	if err := orch.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("pipeline did not exit after cancel")
	}

	stored, err := orch.db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != db.StateCancelled {
		t.Fatalf("expected cancelled, got %s (%s)", stored.State, stored.BlockerReason)
	}
	if stored.PRURL != "" {
		t.Fatalf("unexpected PR after cancel: %s", stored.PRURL)
	}
	mu.Lock()
	wasInterrupted := interrupted
	mu.Unlock()
	if !wasInterrupted {
		t.Fatal("expected OpenCode interrupt on cancel")
	}
	afterRefs := deliveryGit(t, remote, "for-each-ref", "--format=%(refname)")
	if afterRefs != beforeRefs {
		t.Fatalf("cancel must not push/repair; refs before=%q after=%q", beforeRefs, afterRefs)
	}
	if _, err := os.Stat(job.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be cleaned: %v", err)
	}
}

func TestFailJobDoesNotOverwriteCancelled(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	now := time.Now()
	job.State = db.StateCancelled
	job.CompletedAt = &now
	job.BlockerReason = ""
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}

	deliveryAgent(t, orch, job, false, nil)

	stale := *job
	stale.State = db.StateCoding
	orch.failJob(&stale, "zombie coding failure after cancel")

	stored, err := orch.db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != db.StateCancelled {
		t.Fatalf("cancelled overwritten to %s (%s)", stored.State, stored.BlockerReason)
	}
	if strings.Contains(stored.BlockerReason, "committed and pushed") {
		t.Fatalf("checkpoint claimed after cancel: %s", stored.BlockerReason)
	}
}

func TestFailJobIdenticalBranchDoesNotClaimCheckpoint(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	job.State = db.StateCoding
	job.PRURL = ""
	if err := orch.db.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	deliveryAgent(t, orch, job, false, nil)

	orch.failJob(job, "review timeout with no local changes")

	stored, err := orch.db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != db.StateFailed {
		t.Fatalf("expected failed, got %s", stored.State)
	}
	if strings.Contains(stored.BlockerReason, "committed and pushed") {
		t.Fatalf("identical branch must not be reported as recovered work: %s", stored.BlockerReason)
	}
	if strings.Contains(stored.BlockerReason, "Incomplete work checkpoint") {
		t.Fatalf("unexpected checkpoint claim: %s", stored.BlockerReason)
	}
}

// An agent that committed itself (unpublished) still gets formatting folded
// into a delivery commit rather than silently discarded.
func TestFormattingOfUnpublishedAgentCommitIsCommitted(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	writeAgentWork(t, job)
	deliveryGit(t, job.WorktreePath, "add", "feature.txt")
	deliveryGit(t, job.WorktreePath, "commit", "-m", "agent commit")
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\\n' > output.txt"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"true"}
	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "formatted" {
		t.Fatal(got)
	}
	if got := deliveryGit(t, remote, "log", "-1", "--format=%s", job.BranchName); got != "[TEST-1] Automated implementation changes" {
		t.Fatalf("subject = %q", got)
	}
}
