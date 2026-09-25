package git

import (
	"errors"
	"fmt"
	"strings"
)

// SyncResult describes what SyncToRemote did to the local branch.
type SyncResult string

const (
	// SyncNoRemote: the branch does not exist on origin yet; nothing to sync.
	SyncNoRemote SyncResult = "no-remote"
	// SyncUpToDate: local already contains origin/<branch>.
	SyncUpToDate SyncResult = "up-to-date"
	// SyncFastForwarded: local had no unpublished commits and was moved to origin/<branch>.
	SyncFastForwarded SyncResult = "fast-forwarded"
	// SyncRebased: unpublished local commits were replayed on top of origin/<branch>.
	SyncRebased SyncResult = "rebased"
)

// ConflictError is returned when local work cannot be rebased onto the remote
// tip without conflicts. The rebase has been aborted and the worktree restored.
type ConflictError struct {
	Branch string
	Files  []string
	Output string
}

func (e *ConflictError) Error() string {
	files := "(unknown files)"
	if len(e.Files) > 0 {
		files = strings.Join(e.Files, ", ")
	}
	return fmt.Sprintf("rebase onto origin/%s conflicts in: %s", e.Branch, files)
}

// IsConflict reports whether err is (or wraps) a *ConflictError.
func IsConflict(err error) (*ConflictError, bool) {
	var ce *ConflictError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// IsNonFastForward reports whether a push error is a deterministic rejection
// because the remote has commits the local branch lacks. Retrying the
// identical push can never succeed; the caller must fetch and rebase first.
func IsNonFastForward(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "fetch first") ||
		(strings.Contains(msg, "[rejected]") && !strings.Contains(msg, "pre-receive hook declined"))
}

// FetchBranch updates refs/remotes/origin/<branch> from the real remote.
// Returns exists=false (and no error) when the branch is not on origin.
func (m *Manager) FetchBranch(worktreePath, branch string) (exists bool, err error) {
	if branch == "" {
		return false, fmt.Errorf("branch is required")
	}
	out, lsErr := runDeliveryGit(worktreePath, "ls-remote", "--exit-code", "origin", "refs/heads/"+branch)
	if lsErr != nil {
		if strings.TrimSpace(string(out)) == "" {
			// --exit-code returns 2 with no output when the ref is absent.
			return false, nil
		}
		return false, fmt.Errorf("ls-remote origin %s failed: %w\nOutput: %s", branch, lsErr, out)
	}
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)
	if out, err := runDeliveryGit(worktreePath, "fetch", "origin", refspec); err != nil {
		return false, fmt.Errorf("git fetch origin %s failed: %w\nOutput: %s", branch, err, out)
	}
	return true, nil
}

// SyncToRemote brings the worktree's current branch up to date with
// origin/<branch> without ever discarding local work or force-pushing:
//   - branch missing on origin: no-op
//   - local already contains origin/<branch>: no-op
//   - local is behind with no unpublished commits: fast-forward
//   - local has unpublished commits: rebase them onto origin/<branch>
//
// Uncommitted changes are carried across with --autostash. On conflict the
// rebase is aborted (worktree restored) and a *ConflictError names the files.
func (m *Manager) SyncToRemote(worktreePath, branch string) (SyncResult, error) {
	exists, err := m.FetchBranch(worktreePath, branch)
	if err != nil {
		return "", err
	}
	if !exists {
		return SyncNoRemote, nil
	}
	remoteRef := "refs/remotes/origin/" + branch
	if runGitOK(worktreePath, "merge-base", "--is-ancestor", remoteRef, "HEAD") {
		return SyncUpToDate, nil
	}
	result := SyncRebased
	if runGitOK(worktreePath, "merge-base", "--is-ancestor", "HEAD", remoteRef) {
		result = SyncFastForwarded
	}
	out, err := runDeliveryGit(worktreePath, "-c", "rebase.autoSquash=false", "rebase", "--autostash", remoteRef)
	if err != nil {
		files := conflictedFiles(worktreePath)
		_, _ = runDeliveryGit(worktreePath, "rebase", "--abort")
		return "", &ConflictError{Branch: branch, Files: files, Output: string(out)}
	}
	return result, nil
}

// PushHeadToRef publishes HEAD to refs/heads/<ref> (never forced) and verifies
// the remote now points at HEAD. Used for fallback checkpoint refs.
func (m *Manager) PushHeadToRef(worktreePath, ref string) error {
	if ref == "" || ref == m.baseBranch {
		return fmt.Errorf("refusing push to empty or base ref %q", ref)
	}
	if out, err := runDeliveryGit(worktreePath, "push", "origin", "HEAD:refs/heads/"+ref); err != nil {
		return fmt.Errorf("failed to push HEAD to %s: %w\nOutput: %s", ref, err, out)
	}
	head, err := runDeliveryGit(worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("failed to read HEAD: %w", err)
	}
	remote, err := runDeliveryGit(worktreePath, "ls-remote", "--exit-code", "origin", "refs/heads/"+ref)
	fields := strings.Fields(string(remote))
	if err != nil || len(fields) != 2 || fields[0] != strings.TrimSpace(string(head)) {
		return fmt.Errorf("fallback ref %s does not match local HEAD: %s (error: %v)", ref, remote, err)
	}
	return nil
}

func runGitOK(dir string, args ...string) bool {
	_, err := runDeliveryGit(dir, args...)
	return err == nil
}

func conflictedFiles(worktreePath string) []string {
	out, err := runDeliveryGit(worktreePath, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}
