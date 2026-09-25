package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cloneRemote makes an independent clone of repo's origin, standing in for an
// operator or another machine pushing to the same branch.
func cloneRemote(t *testing.T, repo string) string {
	t.Helper()
	remote := strings.TrimSpace(runGit(t, repo, "remote", "get-url", "origin"))
	other := t.TempDir()
	runGit(t, other, "clone", remote, ".")
	runGit(t, other, "config", "user.name", "Operator")
	runGit(t, other, "config", "user.email", "op@example.com")
	return other
}

func commitFile(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", msg)
	return strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
}

// publishedBranch creates a worktree for UTA-1, commits a file, and pushes it.
func publishedBranch(t *testing.T) (*Manager, string, *WorktreeInfo) {
	t.Helper()
	repo := setupTestRepo(t)
	t.Cleanup(func() { os.RemoveAll(repo) })
	mgr := NewManager(repo, "main")
	wt, err := mgr.CreateWorktree("UTA-1")
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt.Path, "feature.txt", "v1\n", "feature")
	runGit(t, wt.Path, "push", "-u", "origin", wt.BranchName)
	return mgr, repo, wt
}

func TestSyncToRemote_StaleReusedBranchFastForwards(t *testing.T) {
	mgr, repo, wt := publishedBranch(t)
	branch := wt.BranchName
	if err := mgr.RemoveWorktree(wt.Path); err != nil {
		t.Fatal(err)
	}
	other := cloneRemote(t, repo)
	runGit(t, other, "checkout", branch)
	remoteTip := commitFile(t, other, "operator.txt", "fix\n", "operator fix")
	runGit(t, other, "push", "origin", branch)

	reused, err := mgr.CreateWorktreeOnRef("UTA-1", branch)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mgr.SyncToRemote(reused.Path, branch)
	if err != nil {
		t.Fatal(err)
	}
	if res != SyncFastForwarded {
		t.Fatalf("result=%s want %s", res, SyncFastForwarded)
	}
	if got := strings.TrimSpace(runGit(t, reused.Path, "rev-parse", "HEAD")); got != remoteTip {
		t.Fatalf("HEAD=%s want remote tip %s", got, remoteTip)
	}
	if _, err := os.Stat(filepath.Join(reused.Path, "operator.txt")); err != nil {
		t.Fatalf("operator commit missing: %v", err)
	}
}

func TestSyncToRemote_RebasesUnpublishedLocalCommits(t *testing.T) {
	mgr, repo, wt := publishedBranch(t)
	other := cloneRemote(t, repo)
	runGit(t, other, "checkout", wt.BranchName)
	remoteTip := commitFile(t, other, "operator.txt", "fix\n", "operator fix")
	runGit(t, other, "push", "origin", wt.BranchName)

	commitFile(t, wt.Path, "local.txt", "agent\n", "unpublished agent work")
	res, err := mgr.SyncToRemote(wt.Path, wt.BranchName)
	if err != nil {
		t.Fatal(err)
	}
	if res != SyncRebased {
		t.Fatalf("result=%s want %s", res, SyncRebased)
	}
	runGit(t, wt.Path, "merge-base", "--is-ancestor", remoteTip, "HEAD")
	if got := runGit(t, wt.Path, "log", "-1", "--format=%s"); strings.TrimSpace(got) != "unpublished agent work" {
		t.Fatalf("local commit not kept on top: %q", got)
	}
	for _, f := range []string{"local.txt", "operator.txt", "feature.txt"} {
		if _, err := os.Stat(filepath.Join(wt.Path, f)); err != nil {
			t.Fatalf("%s missing after rebase: %v", f, err)
		}
	}
}

func TestSyncToRemote_ConflictAbortsAndNamesFiles(t *testing.T) {
	mgr, repo, wt := publishedBranch(t)
	other := cloneRemote(t, repo)
	runGit(t, other, "checkout", wt.BranchName)
	commitFile(t, other, "feature.txt", "operator\n", "operator edit")
	runGit(t, other, "push", "origin", wt.BranchName)

	localTip := commitFile(t, wt.Path, "feature.txt", "agent\n", "agent edit")
	_, err := mgr.SyncToRemote(wt.Path, wt.BranchName)
	ce, ok := IsConflict(err)
	if !ok {
		t.Fatalf("want ConflictError, got %v", err)
	}
	if len(ce.Files) != 1 || ce.Files[0] != "feature.txt" {
		t.Fatalf("files=%v", ce.Files)
	}
	if got := strings.TrimSpace(runGit(t, wt.Path, "rev-parse", "HEAD")); got != localTip {
		t.Fatalf("HEAD moved after aborted rebase: %s want %s", got, localTip)
	}
	if st := runGit(t, wt.Path, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("worktree left dirty: %q", st)
	}
	gitDir := strings.TrimSpace(runGit(t, wt.Path, "rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wt.Path, gitDir)
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, d)); err == nil {
			t.Fatalf("rebase still in progress (%s)", d)
		}
	}
}

func TestSyncToRemote_UnpublishedBranchIsNoRemote(t *testing.T) {
	repo := setupTestRepo(t)
	defer os.RemoveAll(repo)
	mgr := NewManager(repo, "main")
	wt, err := mgr.CreateWorktree("UTA-2")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mgr.SyncToRemote(wt.Path, wt.BranchName)
	if err != nil || res != SyncNoRemote {
		t.Fatalf("res=%s err=%v", res, err)
	}
	res, err = mgr.SyncToRemote(wt.Path, "main")
	if err != nil || res != SyncUpToDate {
		t.Fatalf("main: res=%s err=%v", res, err)
	}
}

func TestPushHeadToRefPublishesWithoutTouchingBranch(t *testing.T) {
	mgr, repo, wt := publishedBranch(t)
	branchTip := strings.TrimSpace(runGit(t, wt.Path, "rev-parse", "HEAD"))
	head := commitFile(t, wt.Path, "local.txt", "x\n", "local")
	ref := "devbox/uta-1-checkpoint-abcdef12"
	if err := mgr.PushHeadToRef(wt.Path, ref); err != nil {
		t.Fatal(err)
	}
	out := runGit(t, repo, "ls-remote", "origin", "refs/heads/"+ref, "refs/heads/"+wt.BranchName)
	if !strings.Contains(out, head+"\trefs/heads/"+ref) || !strings.Contains(out, branchTip+"\trefs/heads/"+wt.BranchName) {
		t.Fatalf("ls-remote:\n%s", out)
	}
}

func TestIsNonFastForward(t *testing.T) {
	cases := map[string]bool{
		" ! [rejected]        b -> b (non-fast-forward)":          true,
		" ! [rejected]        b -> b (fetch first)":               true,
		"Updates were rejected because the tip is behind":         false,
		" ! [remote rejected] b -> b (pre-receive hook declined)": false,
		"fatal: could not read from remote repository":            false,
	}
	for msg, want := range cases {
		if got := IsNonFastForward(errors.New(msg)); got != want {
			t.Errorf("%q: got %v want %v", msg, got, want)
		}
	}
	if IsNonFastForward(nil) {
		t.Error("nil should not be non-fast-forward")
	}
}

// A dirty reused worktree whose uncommitted edit conflicts with a remote commit
// must fail with the file named and leave HEAD + the uncommitted edit exactly as
// before: no conflict markers, no leftover stash entry.
func TestSyncToRemote_DirtyWorktreeConflictRestoresPreSyncState(t *testing.T) {
	mgr, repo, wt := publishedBranch(t)
	other := cloneRemote(t, repo)
	runGit(t, other, "checkout", wt.BranchName)
	commitFile(t, other, "feature.txt", "operator\n", "operator edit")
	runGit(t, other, "push", "origin", wt.BranchName)

	pre := strings.TrimSpace(runGit(t, wt.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(wt.Path, "feature.txt"), []byte("agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("untracked\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := mgr.SyncToRemote(wt.Path, wt.BranchName)
	ce, ok := IsConflict(err)
	if !ok {
		t.Fatalf("want ConflictError, got %v", err)
	}
	if len(ce.Files) == 0 || ce.Files[0] != "feature.txt" {
		t.Fatalf("files=%v", ce.Files)
	}
	if got := strings.TrimSpace(runGit(t, wt.Path, "rev-parse", "HEAD")); got != pre {
		t.Fatalf("HEAD=%s want pre-sync %s", got, pre)
	}
	b, _ := os.ReadFile(filepath.Join(wt.Path, "feature.txt"))
	if string(b) != "agent\n" {
		t.Fatalf("uncommitted edit not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(wt.Path, "new.txt")); string(b) != "untracked\n" {
		t.Fatalf("untracked file not restored: %q", b)
	}
	st := runGit(t, wt.Path, "status", "--porcelain")
	if strings.Contains(st, "UU") || strings.Contains(st, "AA") {
		t.Fatalf("unmerged paths left: %q", st)
	}
	if s := strings.TrimSpace(runGit(t, wt.Path, "stash", "list")); s != "" {
		t.Fatalf("stash entry left behind: %q", s)
	}
}

// A dirty worktree whose uncommitted edit does not overlap the remote commit is
// synced and keeps the edit uncommitted.
func TestSyncToRemote_DirtyWorktreeCarriesUncommittedWork(t *testing.T) {
	mgr, repo, wt := publishedBranch(t)
	other := cloneRemote(t, repo)
	runGit(t, other, "checkout", wt.BranchName)
	remoteTip := commitFile(t, other, "operator.txt", "fix\n", "operator fix")
	runGit(t, other, "push", "origin", wt.BranchName)

	if err := os.WriteFile(filepath.Join(wt.Path, "feature.txt"), []byte("agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := mgr.SyncToRemote(wt.Path, wt.BranchName)
	if err != nil || res != SyncFastForwarded {
		t.Fatalf("res=%s err=%v", res, err)
	}
	if got := strings.TrimSpace(runGit(t, wt.Path, "rev-parse", "HEAD")); got != remoteTip {
		t.Fatalf("HEAD=%s want %s", got, remoteTip)
	}
	if b, _ := os.ReadFile(filepath.Join(wt.Path, "feature.txt")); string(b) != "agent\n" {
		t.Fatalf("uncommitted edit lost: %q", b)
	}
	if s := strings.TrimSpace(runGit(t, wt.Path, "stash", "list")); s != "" {
		t.Fatalf("stash entry left behind: %q", s)
	}
}
