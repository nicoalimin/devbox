package job

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type codexExecFunc func(worktreePath, prompt string, timeout time.Duration) ([]byte, error)

// runCodexExec uses a fresh, non-interactive Codex session for the stronger
// fourth through sixth repair attempts. Devbox remains responsible for the
// branch, commit, validation, and push after Codex leaves fixes in the worktree.
func runCodexExec(worktreePath, prompt string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = commitRecoveryWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", "exec",
		"--ephemeral",
		"--color", "never",
		"--sandbox", "workspace-write",
		"--approve-for-me",
		"--cd", worktreePath,
		"-",
	)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(), "CI=true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("codex exec timed out after %s", timeout)
	}
	if err != nil {
		return output, fmt.Errorf("codex exec failed: %w", err)
	}
	return output, nil
}

func summarizeCodexOutput(output []byte) string {
	const maxLogBytes = 8 * 1024
	trimmed := strings.TrimSpace(string(output))
	if len(trimmed) <= maxLogBytes {
		return trimmed
	}
	return "... output truncated ...\n" + trimmed[len(trimmed)-maxLogBytes:]
}
