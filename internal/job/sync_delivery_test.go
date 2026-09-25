package job

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicoalimin/devbox/internal/db"
	gitmanager "github.com/nicoalimin/devbox/internal/git"
	"github.com/nicoalimin/devbox/internal/linear"
)

type syncRecordingIssues struct {
	mu       sync.Mutex
	comments []string
}

func (r *syncRecordingIssues) GetIssue(string) (*linear.Issue, error) {
	return &linear.Issue{ID: "issue-uuid", Identifier: "TEST-1"}, nil
}

func (r *syncRecordingIssues) AddComment(_ string, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.comments = append(r.comments, body)
	return nil
}

func syncCommit(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	deliveryGit(t, dir, "add", name)
	deliveryGit(t, dir, "commit", "-m", msg)
	return deliveryGit(t, dir, "rev-parse", "HEAD")
}

// operatorClone checks out branch in an independent clone of remote.
func operatorClone(t *testing.T, remote, branch string) string {
	t.Helper()
	dir := t.TempDir()
	deliveryGit(t, dir, "clone", remote, ".")
	deliveryGit(t, dir, "config", "user.name", "Operator")
	deliveryGit(t, dir, "config", "user.email", "op@example.com")
	deliveryGit(t, dir, "checkout", branch)
	return dir
}

// publishJobBranch commits shared.txt on the job branch and pushes it.
func publishJobBranch(t *testing.T, job *db.Job) {
	t.Helper()
	syncCommit(t, job.WorktreePath, "shared.txt", "base\n", "published work")
	deliveryGit(t, job.WorktreePath, "push", "-u", "origin", job.BranchName)
}

// (a) A commit pushed between jobs is the starting point of the next reuse.
func TestReuseWorktreeStartsFromRemoteTip(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	publishJobBranch(t, job)
	other := operatorClone(t, remote, job.BranchName)
	remoteTip := syncCommit(t, other, "operator.txt", "fix\n", "operator fix between jobs")
	deliveryGit(t, other, "push", "origin", job.BranchName)

	next := &db.Job{ID: "job-2", LinearIssueID: "TEST-1", State: db.StatePreparing, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := orch.db.CreateJob(next); err != nil {
		t.Fatal(err)
	}
	mgr := gitmanager.NewManager(job.RepoPath, "main")
	plan := ReusePlan{Reuse: true, Ref: job.BranchName, PreferWorktreePath: job.WorktreePath}
	if err := orch.prepareReuseWorktree(next, mgr, "TEST-1", job.RepoPath, plan); err != nil {
		t.Fatal(err)
	}
	if got := deliveryGit(t, next.WorktreePath, "rev-parse", "HEAD"); got != remoteTip {
		t.Fatalf("reuse HEAD=%s want remote tip %s", got, remoteTip)
	}
	if got := deliveryGit(t, remote, "rev-parse", "refs/heads/"+job.BranchName); got != remoteTip {
		t.Fatalf("remote branch rewritten: %s", got)
	}
}

// (b) A commit pushed mid-job is rebased under the job's work and kept.
func TestPushRebasesOntoMidJobRemoteCommit(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	publishJobBranch(t, job)
	other := operatorClone(t, remote, job.BranchName)
	operatorTip := syncCommit(t, other, "operator.txt", "fix\n", "operator fix mid-job")
	deliveryGit(t, other, "push", "origin", job.BranchName)

	syncCommit(t, job.WorktreePath, "agent.txt", "work\n", "agent work")
	mgr := gitmanager.NewManager(job.RepoPath, "main")
	if err := orch.pushWithRetry(job, mgr); err != nil {
		t.Fatalf("push: %v", err)
	}
	tip := deliveryGit(t, remote, "rev-parse", "refs/heads/"+job.BranchName)
	deliveryGit(t, remote, "merge-base", "--is-ancestor", operatorTip, tip)
	if got := deliveryGit(t, remote, "show", tip+":agent.txt"); got != "work" {
		t.Fatalf("agent work missing on remote: %q", got)
	}
	if got := deliveryGit(t, remote, "show", tip+":operator.txt"); got != "fix" {
		t.Fatalf("operator commit lost: %q", got)
	}
}

// (c) A conflicting remote commit fails the job, names the files, publishes a
// fallback ref, and comments on Linear. Nothing is force-pushed.
func TestConflictingRemoteCommitPublishesFallbackRef(t *testing.T) {
	orch, job, remote := deliveryFixture(t)
	issues := &syncRecordingIssues{}
	orch.linear = issues
	job.ID = "060a218e-1111-2222-3333-444455556666"
	if err := orch.db.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	publishJobBranch(t, job)
	other := operatorClone(t, remote, job.BranchName)
	operatorTip := syncCommit(t, other, "shared.txt", "operator\n", "operator edit")
	deliveryGit(t, other, "push", "origin", job.BranchName)

	if err := os.WriteFile(filepath.Join(job.WorktreePath, "shared.txt"), []byte("agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	orch.failJob(job, "validation failed")

	stored, err := orch.db.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := "devbox/test-1-checkpoint-060a218e"
	if stored.State != db.StateFailed || !strings.Contains(stored.BlockerReason, ref) || !strings.Contains(stored.BlockerReason, "shared.txt") {
		t.Fatalf("blocker: %q (state %s)", stored.BlockerReason, stored.State)
	}
	if strings.Contains(stored.BlockerReason, "recovery incomplete") {
		t.Fatalf("fallback succeeded but blocker claims recovery incomplete: %q", stored.BlockerReason)
	}
	if got := deliveryGit(t, remote, "show", "refs/heads/"+ref+":shared.txt"); got != "agent" {
		t.Fatalf("fallback ref content %q", got)
	}
	if got := deliveryGit(t, remote, "rev-parse", "refs/heads/"+job.BranchName); got != operatorTip {
		t.Fatalf("assigned branch was overwritten: %s want %s", got, operatorTip)
	}
	issues.mu.Lock()
	defer issues.mu.Unlock()
	if len(issues.comments) != 1 || !strings.Contains(issues.comments[0], ref) || !strings.Contains(issues.comments[0], "shared.txt") {
		t.Fatalf("linear comments: %q", issues.comments)
	}
}
