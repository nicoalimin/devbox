package job

import (
	"fmt"
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
	return orch, job, remote
}

type deliveryIssues struct{}

func (deliveryIssues) GetIssue(string) (*linear.Issue, error) {
	return &linear.Issue{Identifier: "TEST-1"}, nil
}
func (deliveryIssues) AddComment(string, string) error { return nil }

// The model writes files but never commits. Optionally it also times out.
func deliveryAgent(t *testing.T, orch *Orchestrator, job *db.Job, timeout bool, repair func(int)) {
	t.Helper()
	var mu sync.Mutex
	active := false
	prompts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/session":
			fmt.Fprint(w, `{"data":{"id":"session"}}`)
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			active = true
			prompts++
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
	if err := orch.validateWithOpenCodeRepair(job); err != nil {
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
	if err != nil || stored.State != db.StateFailed || !strings.Contains(stored.BlockerReason, "3 repair attempts") || !strings.Contains(stored.BlockerReason, "committed and pushed") {
		t.Fatalf("job: %+v, err: %v", stored, err)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "partial" {
		t.Fatal(got)
	}
	if _, err := os.Stat(job.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("published failed worktree was not cleaned: %v", err)
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

func TestHostFormattingRunsBeforeValidation(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	orch.cfg.Repos[0].Repo.FormatCommands = []string{"printf 'formatted\n' > output.txt"}
	orch.cfg.Repos[0].Repo.ValidationCommands = []string{"test \"$(cat output.txt)\" = formatted"}
	if err := orch.pushBranch(job); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+job.BranchName+":output.txt"); got != "formatted" {
		t.Fatal(got)
	}
}

func TestHostDeliveryFormatsAndCommitsWithoutOpenCode(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
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
	if subject := deliveryGit(t, job.WorktreePath, "show", "-s", "--format=%s", "HEAD"); subject != "[TEST-1] Apply validated changes" {
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
