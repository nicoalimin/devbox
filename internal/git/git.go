package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Manager handles git operations
type Manager struct {
	repoPath   string
	baseBranch string
}

// NewManager creates a new git manager
func NewManager(repoPath, baseBranch string) *Manager {
	return &Manager{
		repoPath:   repoPath,
		baseBranch: baseBranch,
	}
}

// WorktreeInfo represents information about a git worktree
type WorktreeInfo struct {
	Path       string
	BranchName string
}

// CreateWorktree creates a new git worktree with a unique branch
func (m *Manager) CreateWorktree(identifier string) (*WorktreeInfo, error) {
	// Ensure we have the latest from remote
	if err := m.fetchBaseBranch(); err != nil {
		return nil, fmt.Errorf("failed to fetch base branch: %w", err)
	}

	// Generate base branch name (e.g., devbox/eng-123)
	baseBranchName := fmt.Sprintf("devbox/%s", strings.ToLower(identifier))

	// Find a unique branch name by adding suffix if needed
	branchName, err := m.findUniqueBranchName(baseBranchName)
	if err != nil {
		return nil, fmt.Errorf("failed to find unique branch name: %w", err)
	}

	// Generate worktree path, adding same suffix if branch name was modified
	worktreePath := filepath.Join(m.repoPath, ".devbox-worktrees", identifier)
	if branchName != baseBranchName {
		// Extract suffix from branch name (e.g., "-2" from "devbox/eng-123-2")
		suffix := strings.TrimPrefix(branchName, baseBranchName)
		worktreePath = filepath.Join(m.repoPath, ".devbox-worktrees", identifier+suffix)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktree directory: %w", err)
	}

	// Create worktree from remote base branch to ensure we have the latest changes
	// Use origin/<baseBranch> instead of local <baseBranch> to handle cases where
	// the local base branch is stale or has diverged from remote
	remoteBase := fmt.Sprintf("origin/%s", m.baseBranch)
	cmd := exec.Command("git", "worktree", "add", "--no-track", "-b", branchName, worktreePath, remoteBase)
	cmd.Dir = m.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to create worktree: %w\nOutput: %s", err, string(output))
	}

	return &WorktreeInfo{
		Path:       worktreePath,
		BranchName: branchName,
	}, nil
}

// findUniqueBranchName finds a unique branch name by adding numeric suffixes if needed
func (m *Manager) findUniqueBranchName(baseName string) (string, error) {
	// Check if base name is available
	if !m.branchExists(baseName) {
		return baseName, nil
	}

	// Try suffixed names: baseName-2, baseName-3, etc.
	for i := 2; i <= 100; i++ {
		candidate := fmt.Sprintf("%s-%d", baseName, i)
		if !m.branchExists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not find unique branch name after 100 attempts (base: %s)", baseName)
}

// branchExists checks if a branch exists locally or remotely
func (m *Manager) branchExists(branchName string) bool {
	// Check local branches
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", branchName))
	cmd.Dir = m.repoPath
	if err := cmd.Run(); err == nil {
		return true
	}

	// Check remote branches
	cmd = exec.Command("git", "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/remotes/origin/%s", branchName))
	cmd.Dir = m.repoPath
	if err := cmd.Run(); err == nil {
		return true
	}

	return false
}

// RecreateWorktree recreates a worktree for an existing branch
// This is useful when the worktree was cleaned up but we need it again for the branch
func (m *Manager) RecreateWorktree(identifier, branchName string) (*WorktreeInfo, error) {
	// Verify the branch exists
	if !m.branchExists(branchName) {
		return nil, fmt.Errorf("branch %s does not exist", branchName)
	}

	// Generate worktree path using the same logic as CreateWorktree
	baseBranchName := fmt.Sprintf("devbox/%s", strings.ToLower(identifier))
	worktreePath := filepath.Join(m.repoPath, ".devbox-worktrees", identifier)
	if branchName != baseBranchName {
		// Extract suffix from branch name
		suffix := strings.TrimPrefix(branchName, baseBranchName)
		worktreePath = filepath.Join(m.repoPath, ".devbox-worktrees", identifier+suffix)
	}

	// Never replace an existing directory: it may contain unpublished work.
	if _, err := os.Stat(worktreePath); err == nil {
		return nil, fmt.Errorf("refusing to replace existing worktree at %s", worktreePath)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktree directory: %w", err)
	}

	// Create worktree from existing branch (no -b flag, just checkout)
	cmd := exec.Command("git", "worktree", "add", worktreePath, branchName)
	cmd.Dir = m.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to recreate worktree: %w\nOutput: %s", err, string(output))
	}

	return &WorktreeInfo{
		Path:       worktreePath,
		BranchName: branchName,
	}, nil
}

// RemoveWorktree removes a git worktree
func (m *Manager) RemoveWorktree(worktreePath string) error {
	// Check if the worktree path exists first
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		// Worktree already removed, that's fine
		return nil
	}

	cmd := exec.Command("git", "worktree", "remove", worktreePath)
	cmd.Dir = m.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove worktree: %w\nOutput: %s", err, string(output))
	}
	return nil
}

// PushBranch pushes a branch to the remote
func (m *Manager) PushBranch(worktreePath, branchName string) error {
	if err := m.AssertBranch(worktreePath, branchName); err != nil {
		return err
	}
	output, err := runDeliveryGit(worktreePath, "push", "-u", "origin", "HEAD:refs/heads/"+branchName)
	if err != nil {
		return fmt.Errorf("failed to push branch: %w\nOutput: %s", err, string(output))
	}
	return m.VerifyPublished(worktreePath, branchName)
}

// Bound credential helpers, hooks and transport processes as well as Git
// itself so host recovery cannot hang indefinitely after a model timeout.
func runDeliveryGit(worktreePath string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktreePath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}

// AssertBranch prevents an agent's checkout of another branch (or detached HEAD)
// from causing the host to commit or push to the wrong destination.
func (m *Manager) AssertBranch(worktreePath, branchName string) error {
	if branchName == "" || branchName == m.baseBranch {
		return fmt.Errorf("refusing delivery to empty or base branch %q", branchName)
	}
	cmd := exec.Command("git", "symbolic-ref", "--quiet", "--short", "HEAD")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) != branchName {
		return fmt.Errorf("worktree must be on assigned branch %q (found %q, error: %v)", branchName, strings.TrimSpace(string(output)), err)
	}
	return nil
}

// VerifyPublished checks the actual remote, not a potentially stale tracking ref.
func (m *Manager) VerifyPublished(worktreePath, branchName string) error {
	if err := m.AssertBranch(worktreePath, branchName); err != nil {
		return err
	}
	dirty, err := m.HasUncommittedChanges(worktreePath)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("worktree still contains uncommitted changes")
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	head, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to read HEAD: %w", err)
	}
	remote, err := runDeliveryGit(worktreePath, "ls-remote", "--exit-code", "origin", "refs/heads/"+branchName)
	fields := strings.Fields(string(remote))
	if err != nil || len(fields) != 2 || fields[0] != strings.TrimSpace(string(head)) {
		return fmt.Errorf("remote branch %s does not match local HEAD: %s (error: %v)", branchName, remote, err)
	}
	return nil
}

// HasUncommittedChanges reports whether a worktree contains staged, unstaged,
// or untracked changes.
func (m *Manager) HasUncommittedChanges(worktreePath string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to inspect worktree changes: %w", err)
	}
	return len(output) > 0, nil
}

// CommitAll commits every staged, unstaged, and untracked change in a
// worktree. The host delivery gate uses it after formatting and validation.
func (m *Manager) CommitAll(worktreePath, message string) error {
	if output, err := runDeliveryGit(worktreePath, "add", "--all"); err != nil {
		return fmt.Errorf("failed to stage worktree changes: %w\nOutput: %s", err, string(output))
	}
	if _, err := runDeliveryGit(worktreePath, "diff", "--cached", "--quiet"); err == nil {
		return nil // Restart/retry after the commit already succeeded.
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		return fmt.Errorf("failed to inspect staged changes: %w", err)
	}

	// Deliberately use the repository/user Git configuration so the delivery
	// commit has the same identity as a normal commit made in this checkout.
	if output, err := runDeliveryGit(worktreePath, "commit", "-m", message); err != nil {
		return fmt.Errorf("failed to commit worktree changes: %w\nOutput: %s", err, string(output))
	}

	return nil
}

// fetchBaseBranch fetches the latest changes for the base branch
func (m *Manager) fetchBaseBranch() error {
	cmd := exec.Command("git", "fetch", "origin", m.baseBranch)
	cmd.Dir = m.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch failed: %w\nOutput: %s", err, string(output))
	}
	return nil
}

// CreatePR creates a pull request using gh CLI
// If a PR already exists for the branch, returns the existing PR URL instead of failing
func CreatePR(worktreePath, title, body, baseBranch string) (string, error) {
	args := []string{
		"pr", "create",
		"--title", title,
		"--body", body,
		"--base", baseBranch,
	}

	cmd := exec.Command("gh", args...)
	cmd.Dir = worktreePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if error is due to PR already existing
		outputStr := string(output)
		if strings.Contains(outputStr, "already exists") {
			// Try to extract PR URL from error message
			// Format: "a pull request for branch ... already exists: https://..."
			if prURL := extractPRURLFromError(outputStr); prURL != "" {
				return prURL, nil
			}

			// Fallback: use gh pr view to get existing PR URL
			if prURL, viewErr := getExistingPRURL(worktreePath); viewErr == nil {
				return prURL, nil
			}
		}

		return "", fmt.Errorf("failed to create PR: %w\nOutput: %s", err, string(output))
	}

	// Extract PR URL from output (gh returns the URL on the last line)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > 0 {
		prURL := strings.TrimSpace(lines[len(lines)-1])
		if strings.HasPrefix(prURL, "http") {
			return prURL, nil
		}
	}

	return string(output), nil
}

// extractPRURLFromError attempts to extract a PR URL from gh CLI error output
func extractPRURLFromError(output string) string {
	// Look for URL pattern in error message
	// Example: "a pull request for branch ... already exists: https://github.com/owner/repo/pull/123"
	lines := strings.Split(output, "\n")
	foundAlreadyExists := false

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.Contains(line, "already exists:") {
			// Find URL after "already exists:"
			parts := strings.SplitN(line, "already exists:", 2)
			if len(parts) == 2 {
				url := strings.TrimSpace(parts[1])
				if strings.HasPrefix(url, "http") {
					return url
				}
			}
			foundAlreadyExists = true
		} else if foundAlreadyExists && strings.HasPrefix(line, "http") {
			// URL is on the next line after "already exists"
			return line
		} else if strings.Contains(line, "already exists") && !strings.Contains(line, "already exists:") {
			// "already exists" without colon, URL might be on next line
			foundAlreadyExists = true
		}
	}
	return ""
}

// getExistingPRURL retrieves the URL of an existing PR for the current branch
func getExistingPRURL(worktreePath string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", "--json", "url", "--jq", ".url")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get existing PR URL: %w", err)
	}

	url := strings.TrimSpace(string(output))
	if url == "" || !strings.HasPrefix(url, "http") {
		return "", fmt.Errorf("invalid PR URL: %s", url)
	}

	return url, nil
}

// GetRepoURL gets the repository URL for a worktree
func GetRepoURL(worktreePath string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get repo URL: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// HasCommitsAheadOfBase checks if the branch has commits ahead of the base branch
func (m *Manager) HasCommitsAheadOfBase(worktreePath string) (bool, error) {
	// Get the current branch name
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to get current branch: %w", err)
	}
	currentBranch := strings.TrimSpace(string(output))

	// Count commits ahead of base
	rangeSpec := fmt.Sprintf("origin/%s..%s", m.baseBranch, currentBranch)
	cmd = exec.Command("git", "rev-list", "--count", rangeSpec)
	cmd.Dir = worktreePath
	output, err = cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to count commits: %w", err)
	}

	count := strings.TrimSpace(string(output))
	if count == "" || count == "0" {
		return false, nil
	}

	return true, nil
}
