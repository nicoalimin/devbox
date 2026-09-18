package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliveryRefusesWrongBranchAndDirtyCleanup(t *testing.T) {
	repo := setupTestRepo(t)
	defer os.RemoveAll(repo)
	mgr := NewManager(repo, "main")
	worktree, err := mgr.CreateWorktree("TEST-SAFE")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(worktree.Path, "untracked.txt")
	if err := os.WriteFile(path, []byte("preserve"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveWorktree(worktree.Path); err == nil {
		t.Fatal("removed dirty worktree")
	}
	if _, err := mgr.RecreateWorktree("TEST-SAFE", worktree.BranchName); err == nil {
		t.Fatal("replaced existing worktree")
	}
	if err := mgr.PushBranch(worktree.Path, "main"); err == nil {
		t.Fatal("pushed to base branch")
	}
	if err := mgr.PushBranch(worktree.Path, "devbox/other"); err == nil {
		t.Fatal("pushed to wrong branch")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lost output: %v", err)
	}
	if err := mgr.CommitAll(worktree.Path, "checkpoint"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitAll(worktree.Path, "retry"); err != nil {
		t.Fatalf("commit retry is not idempotent: %v", err)
	}
	if err := mgr.VerifyPublished(worktree.Path, worktree.BranchName); err == nil {
		t.Fatal("unpublished commit passed verification")
	}
	if err := mgr.PushBranch(worktree.Path, worktree.BranchName); err != nil {
		t.Fatal(err)
	}
	if err := mgr.VerifyPublished(worktree.Path, worktree.BranchName); err != nil {
		t.Fatal(err)
	}
}

// setupTestRepo creates a temporary git repository for testing
func setupTestRepo(t *testing.T) string {
	t.Helper()

	// Create a "remote" repo first
	remoteDir, err := os.MkdirTemp("", "devbox-git-remote-*")
	if err != nil {
		t.Fatalf("failed to create temp remote dir: %v", err)
	}

	cmd := exec.Command("git", "init", "--bare")
	cmd.Dir = remoteDir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to init bare remote repo: %v", err)
	}

	// Create a local repo
	tmpDir, err := os.MkdirTemp("", "devbox-git-test-*")
	if err != nil {
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to create temp dir: %v", err)
	}

	// Initialize git repo
	cmd = exec.Command("git", "init")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Configure git
	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = tmpDir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = tmpDir
	cmd.Run()

	// Add origin remote
	cmd = exec.Command("git", "remote", "add", "origin", remoteDir)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to add remote: %v", err)
	}

	// Create initial commit
	readmePath := filepath.Join(tmpDir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Test Repo\n"), 0644); err != nil {
		os.RemoveAll(tmpDir)
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to create README: %v", err)
	}

	cmd = exec.Command("git", "add", "README.md")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to add README: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Initial commit")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Rename default branch to main (in case it's master)
	cmd = exec.Command("git", "branch", "-M", "main")
	cmd.Dir = tmpDir
	cmd.Run()

	// Push to remote
	cmd = exec.Command("git", "push", "-u", "origin", "main")
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		os.RemoveAll(remoteDir)
		t.Fatalf("failed to push to remote: %v", err)
	}

	// Store remote path for cleanup
	t.Cleanup(func() {
		os.RemoveAll(remoteDir)
	})

	return tmpDir
}

// createBranch creates a branch in the test repo
func createBranch(t *testing.T, repoPath, branchName string) {
	t.Helper()

	cmd := exec.Command("git", "branch", branchName)
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create branch %s: %v", branchName, err)
	}
}

func TestCreateWorktree_Success(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	worktree, err := mgr.CreateWorktree("TEST-123")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	if worktree.BranchName != "devbox/test-123" {
		t.Errorf("expected branch name 'devbox/test-123', got '%s'", worktree.BranchName)
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "@{upstream}")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err == nil {
		t.Fatal("new job branch must not track origin/main before its first push")
	}

	expectedPath := filepath.Join(repoPath, ".devbox-worktrees", "TEST-123")
	if worktree.Path != expectedPath {
		t.Errorf("expected path '%s', got '%s'", expectedPath, worktree.Path)
	}

	// Verify worktree exists
	if _, err := os.Stat(worktree.Path); os.IsNotExist(err) {
		t.Error("worktree path does not exist")
	}

	// Cleanup
	mgr.RemoveWorktree(worktree.Path)
}

func TestCreateWorktree_CollisionHandling(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Create a branch that will conflict
	createBranch(t, repoPath, "devbox/test-456")

	// Now try to create a worktree with the same identifier
	worktree, err := mgr.CreateWorktree("TEST-456")
	if err != nil {
		t.Fatalf("CreateWorktree failed with collision: %v", err)
	}

	// Should have created with suffix
	if worktree.BranchName != "devbox/test-456-2" {
		t.Errorf("expected branch name 'devbox/test-456-2', got '%s'", worktree.BranchName)
	}

	expectedPath := filepath.Join(repoPath, ".devbox-worktrees", "TEST-456-2")
	if worktree.Path != expectedPath {
		t.Errorf("expected path '%s', got '%s'", expectedPath, worktree.Path)
	}

	// Verify worktree exists
	if _, err := os.Stat(worktree.Path); os.IsNotExist(err) {
		t.Error("worktree path does not exist")
	}

	// Cleanup
	mgr.RemoveWorktree(worktree.Path)
}

func TestCreateWorktree_MultipleCollisions(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Create branches that will conflict
	createBranch(t, repoPath, "devbox/test-789")
	createBranch(t, repoPath, "devbox/test-789-2")
	createBranch(t, repoPath, "devbox/test-789-3")

	// Now try to create a worktree with the same identifier
	worktree, err := mgr.CreateWorktree("TEST-789")
	if err != nil {
		t.Fatalf("CreateWorktree failed with multiple collisions: %v", err)
	}

	// Should have created with suffix -4
	if worktree.BranchName != "devbox/test-789-4" {
		t.Errorf("expected branch name 'devbox/test-789-4', got '%s'", worktree.BranchName)
	}

	expectedPath := filepath.Join(repoPath, ".devbox-worktrees", "TEST-789-4")
	if worktree.Path != expectedPath {
		t.Errorf("expected path '%s', got '%s'", expectedPath, worktree.Path)
	}

	// Cleanup
	mgr.RemoveWorktree(worktree.Path)
}

func TestBranchExists(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Test non-existent branch
	if mgr.branchExists("devbox/nonexistent") {
		t.Error("branchExists returned true for non-existent branch")
	}

	// Create a branch and test
	createBranch(t, repoPath, "devbox/exists")
	if !mgr.branchExists("devbox/exists") {
		t.Error("branchExists returned false for existing branch")
	}
}

func TestFindUniqueBranchName(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	tests := []struct {
		name             string
		baseName         string
		existingBranches []string
		expected         string
	}{
		{
			name:             "no collision",
			baseName:         "devbox/test-1",
			existingBranches: []string{},
			expected:         "devbox/test-1",
		},
		{
			name:             "one collision",
			baseName:         "devbox/test-2",
			existingBranches: []string{"devbox/test-2"},
			expected:         "devbox/test-2-2",
		},
		{
			name:             "multiple collisions",
			baseName:         "devbox/test-3",
			existingBranches: []string{"devbox/test-3", "devbox/test-3-2", "devbox/test-3-3"},
			expected:         "devbox/test-3-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create existing branches
			for _, branch := range tt.existingBranches {
				createBranch(t, repoPath, branch)
			}

			result, err := mgr.findUniqueBranchName(tt.baseName)
			if err != nil {
				t.Fatalf("findUniqueBranchName failed: %v", err)
			}

			if result != tt.expected {
				t.Errorf("expected '%s', got '%s'", tt.expected, result)
			}

			// Cleanup branches for next test
			for _, branch := range tt.existingBranches {
				exec.Command("git", "branch", "-D", branch).Run()
			}
		})
	}
}

func TestRecreateWorktree_Success(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// First create a worktree and get a commit on it
	worktree, err := mgr.CreateWorktree("TEST-RECREATE")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	branchName := worktree.BranchName

	// Add a commit to the worktree
	testFile := filepath.Join(worktree.Path, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content\n"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	cmd := exec.Command("git", "add", "test.txt")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Add test file")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Now remove the worktree (simulating cleanup after job completion)
	if err := mgr.RemoveWorktree(worktree.Path); err != nil {
		t.Fatalf("RemoveWorktree failed: %v", err)
	}

	// Verify worktree is gone
	if _, err := os.Stat(worktree.Path); !os.IsNotExist(err) {
		t.Error("worktree still exists after removal")
	}

	// Now recreate the worktree from the existing branch
	recreated, err := mgr.RecreateWorktree("TEST-RECREATE", branchName)
	if err != nil {
		t.Fatalf("RecreateWorktree failed: %v", err)
	}

	// Verify the worktree was recreated with the same branch
	if recreated.BranchName != branchName {
		t.Errorf("expected branch name '%s', got '%s'", branchName, recreated.BranchName)
	}

	expectedPath := filepath.Join(repoPath, ".devbox-worktrees", "TEST-RECREATE")
	if recreated.Path != expectedPath {
		t.Errorf("expected path '%s', got '%s'", expectedPath, recreated.Path)
	}

	// Verify worktree exists
	if _, err := os.Stat(recreated.Path); os.IsNotExist(err) {
		t.Error("recreated worktree path does not exist")
	}

	// Verify the commit is still there
	testFilePath := filepath.Join(recreated.Path, "test.txt")
	if _, err := os.Stat(testFilePath); os.IsNotExist(err) {
		t.Error("test file from original branch not found in recreated worktree")
	}

	// Cleanup
	mgr.RemoveWorktree(recreated.Path)
}

func TestRecreateWorktree_NonExistentBranch(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Try to recreate from a non-existent branch
	_, err := mgr.RecreateWorktree("TEST-NONEXISTENT", "devbox/nonexistent")
	if err == nil {
		t.Error("expected error when recreating worktree from non-existent branch")
	}
}

func TestRecreateWorktree_WithSuffix(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Create a branch that would conflict (so we get a suffixed branch name)
	createBranch(t, repoPath, "devbox/test-suffix")

	// Create a worktree which will get suffix -2
	worktree, err := mgr.CreateWorktree("TEST-SUFFIX")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	branchName := worktree.BranchName // Should be "devbox/test-suffix-2"
	if branchName != "devbox/test-suffix-2" {
		t.Fatalf("expected branch name 'devbox/test-suffix-2', got '%s'", branchName)
	}

	// Remove the worktree
	if err := mgr.RemoveWorktree(worktree.Path); err != nil {
		t.Fatalf("RemoveWorktree failed: %v", err)
	}

	// Recreate with the suffixed branch name
	recreated, err := mgr.RecreateWorktree("TEST-SUFFIX", branchName)
	if err != nil {
		t.Fatalf("RecreateWorktree failed: %v", err)
	}

	// Verify path has the same suffix
	expectedPath := filepath.Join(repoPath, ".devbox-worktrees", "TEST-SUFFIX-2")
	if recreated.Path != expectedPath {
		t.Errorf("expected path '%s', got '%s'", expectedPath, recreated.Path)
	}

	// Cleanup
	mgr.RemoveWorktree(recreated.Path)
}

func TestCreateWorktree_UsesRemoteTip(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Get the current commit SHA of local main (initial state)
	cmd := exec.Command("git", "rev-parse", "main")
	cmd.Dir = repoPath
	initialSHA, err := cmd.Output()
	if err != nil {
		t.Fatalf("Failed to get initial main SHA: %v", err)
	}
	initialCommit := strings.TrimSpace(string(initialSHA))

	// Simulate a remote update by:
	// 1. Creating a new branch with an additional commit
	// 2. Pushing that branch to update remote main
	// 3. Not updating local main (keeping it stale)

	// Create a temporary branch
	cmd = exec.Command("git", "checkout", "-b", "temp-remote-update")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to create temp branch: %v", err)
	}

	// Create a new commit (simulating another developer's work pushed to remote)
	updateFile := filepath.Join(repoPath, "update.txt")
	if err := os.WriteFile(updateFile, []byte("remote update\n"), 0644); err != nil {
		t.Fatalf("Failed to create update file: %v", err)
	}

	cmd = exec.Command("git", "add", "update.txt")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add update file: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Remote update")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Get the new commit SHA
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoPath
	newSHA, err := cmd.Output()
	if err != nil {
		t.Fatalf("Failed to get new SHA: %v", err)
	}
	newCommit := strings.TrimSpace(string(newSHA))

	// Push this branch to remote main (updating remote without updating local main)
	cmd = exec.Command("git", "push", "origin", "temp-remote-update:main", "--force")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to push to remote main: %v", err)
	}

	// Switch back to local main (which is still at the old commit)
	cmd = exec.Command("git", "checkout", "main")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to checkout main: %v", err)
	}

	// Delete temp branch
	cmd = exec.Command("git", "branch", "-D", "temp-remote-update")
	cmd.Dir = repoPath
	cmd.Run()

	// Verify local main is still at the old commit
	cmd = exec.Command("git", "rev-parse", "main")
	cmd.Dir = repoPath
	currentMainSHA, err := cmd.Output()
	if err != nil {
		t.Fatalf("Failed to get current main SHA: %v", err)
	}
	currentMain := strings.TrimSpace(string(currentMainSHA))

	if currentMain != initialCommit {
		t.Fatal("Local main should still be at initial commit (stale)")
	}

	// Now CreateWorktree should fetch and use the up-to-date origin/main
	worktree, err := mgr.CreateWorktree("TEST-REMOTE-TIP")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
	defer mgr.RemoveWorktree(worktree.Path)

	// Get the first commit of the new worktree's branch
	// (the commit it branched from)
	cmd = exec.Command("git", "rev-parse", worktree.BranchName+"~0")
	cmd.Dir = repoPath
	worktreeTipSHA, err := cmd.Output()
	if err != nil {
		t.Fatalf("Failed to get worktree tip SHA: %v", err)
	}
	worktreeTip := strings.TrimSpace(string(worktreeTipSHA))

	// The worktree should be at the new commit (remote tip), not the stale local main
	if worktreeTip != newCommit {
		t.Errorf("Worktree should be based on remote tip %s, but is at %s (stale local main: %s)",
			newCommit, worktreeTip, initialCommit)
	}

	// Additionally, verify that the update.txt file exists in the worktree
	// (proving it was created from the remote tip, not the stale local main)
	updateFilePath := filepath.Join(worktree.Path, "update.txt")
	if _, err := os.Stat(updateFilePath); os.IsNotExist(err) {
		t.Error("update.txt from remote tip not found in worktree - worktree was created from stale local main")
	}
}

func TestHasCommitsAheadOfBase(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")

	// Create a worktree
	worktree, err := mgr.CreateWorktree("TEST-COMMIT")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
	defer mgr.RemoveWorktree(worktree.Path)

	// Test: No commits yet (freshly created worktree)
	hasCommits, err := mgr.HasCommitsAheadOfBase(worktree.Path)
	if err != nil {
		t.Fatalf("HasCommitsAheadOfBase failed: %v", err)
	}
	if hasCommits {
		t.Error("Expected no commits ahead of base for fresh worktree")
	}

	// Add a commit to the worktree
	testFile := filepath.Join(worktree.Path, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content\n"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	cmd := exec.Command("git", "add", "test.txt")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add test file: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Add test file")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// Test: Should now have commits ahead
	hasCommits, err = mgr.HasCommitsAheadOfBase(worktree.Path)
	if err != nil {
		t.Fatalf("HasCommitsAheadOfBase failed: %v", err)
	}
	if !hasCommits {
		t.Error("Expected commits ahead of base after adding commit")
	}

	// Add another commit
	testFile2 := filepath.Join(worktree.Path, "test2.txt")
	if err := os.WriteFile(testFile2, []byte("more content\n"), 0644); err != nil {
		t.Fatalf("Failed to create second test file: %v", err)
	}

	cmd = exec.Command("git", "add", "test2.txt")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to add second test file: %v", err)
	}

	cmd = exec.Command("git", "commit", "-m", "Add second test file")
	cmd.Dir = worktree.Path
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to commit second file: %v", err)
	}

	// Test: Should still have commits ahead
	hasCommits, err = mgr.HasCommitsAheadOfBase(worktree.Path)
	if err != nil {
		t.Fatalf("HasCommitsAheadOfBase failed: %v", err)
	}
	if !hasCommits {
		t.Error("Expected commits ahead of base after adding multiple commits")
	}
}

func TestCommitAllCreatesFallbackCommit(t *testing.T) {
	repoPath := setupTestRepo(t)
	defer os.RemoveAll(repoPath)

	mgr := NewManager(repoPath, "main")
	worktree, err := mgr.CreateWorktree("TEST-FALLBACK-COMMIT")
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}
	defer mgr.RemoveWorktree(worktree.Path)

	hasChanges, err := mgr.HasUncommittedChanges(worktree.Path)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed: %v", err)
	}
	if hasChanges {
		t.Fatal("fresh worktree unexpectedly contains changes")
	}

	if err := os.WriteFile(filepath.Join(worktree.Path, "generated.txt"), []byte("agent output\n"), 0644); err != nil {
		t.Fatalf("failed to create generated file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Path, "README.md"), []byte("# Updated by agent\n"), 0644); err != nil {
		t.Fatalf("failed to modify tracked file: %v", err)
	}

	hasChanges, err = mgr.HasUncommittedChanges(worktree.Path)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed: %v", err)
	}
	if !hasChanges {
		t.Fatal("expected staged, unstaged, or untracked changes to be detected")
	}

	const message = "[TEST-123] Apply automated changes"
	if err := mgr.CommitAll(worktree.Path, message); err != nil {
		t.Fatalf("CommitAll failed: %v", err)
	}

	hasChanges, err = mgr.HasUncommittedChanges(worktree.Path)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed after commit: %v", err)
	}
	if hasChanges {
		t.Fatal("worktree remained dirty after fallback commit")
	}

	hasCommits, err := mgr.HasCommitsAheadOfBase(worktree.Path)
	if err != nil {
		t.Fatalf("HasCommitsAheadOfBase failed: %v", err)
	}
	if !hasCommits {
		t.Fatal("fallback commit is not ahead of the base branch")
	}

	cmd := exec.Command("git", "log", "-1", "--pretty=%s")
	cmd.Dir = worktree.Path
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to read fallback commit message: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != message {
		t.Fatalf("fallback commit message = %q, want %q", got, message)
	}

	cmd = exec.Command("git", "log", "-1", "--pretty=%an <%ae>")
	cmd.Dir = worktree.Path
	output, err = cmd.Output()
	if err != nil {
		t.Fatalf("failed to read fallback commit author: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != "Test User <test@example.com>" {
		t.Fatalf("fallback commit author = %q, want repository Git identity", got)
	}
}

func TestExtractPRURLFromError(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected string
	}{
		{
			name:     "standard error format with URL on same line",
			output:   "a pull request for branch \"devbox/test-123\" into branch \"main\" already exists: https://github.com/owner/repo/pull/9",
			expected: "https://github.com/owner/repo/pull/9",
		},
		{
			name: "multiline error with URL on next line",
			output: `Error: a pull request for branch "devbox/test-456" already exists
https://github.com/owner/repo/pull/15`,
			expected: "https://github.com/owner/repo/pull/15",
		},
		{
			name: "exact production error format from UTA-12",
			output: `a pull request for branch "devbox/uta-12-2" into branch "main" already exists:
https://github.com/nicoalimin/tokoboss/pull/9`,
			expected: "https://github.com/nicoalimin/tokoboss/pull/9",
		},
		{
			name:     "error with extra whitespace",
			output:   "a pull request for branch ... already exists:    https://github.com/owner/repo/pull/42   ",
			expected: "https://github.com/owner/repo/pull/42",
		},
		{
			name:     "no URL in error",
			output:   "some other error message",
			expected: "",
		},
		{
			name:     "already exists but no URL",
			output:   "a pull request for branch ... already exists but no URL provided",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPRURLFromError(tt.output)
			if result != tt.expected {
				t.Errorf("expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}
