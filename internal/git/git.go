package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// Create worktree
	cmd := exec.Command("git", "worktree", "add", "-b", branchName, worktreePath, m.baseBranch)
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

// RemoveWorktree removes a git worktree
func (m *Manager) RemoveWorktree(worktreePath string) error {
	cmd := exec.Command("git", "worktree", "remove", worktreePath, "--force")
	cmd.Dir = m.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove worktree: %w\nOutput: %s", err, string(output))
	}
	return nil
}

// PushBranch pushes a branch to the remote
func (m *Manager) PushBranch(worktreePath, branchName string) error {
	cmd := exec.Command("git", "push", "-u", "origin", branchName)
	cmd.Dir = worktreePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to push branch: %w\nOutput: %s", err, string(output))
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
