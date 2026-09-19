package upgrade

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func copyAtomic(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular executable: %s", source)
	}
	out, err := os.CreateTemp(filepath.Dir(target), ".devbox-upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), target)
}

func (m *Manager) backup(s *State) error {
	for i := range s.Files {
		file := &s.Files[i]
		if _, err := os.Stat(file.Target); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		file.Backup = file.Built + ".previous"
		if err := copyAtomic(file.Target, file.Backup); err != nil {
			return fmt.Errorf("back up %s: %w", file.Target, err)
		}
	}
	return nil
}

func (m *Manager) rollback(s *State, cause error) error {
	for _, file := range s.Files {
		var err error
		if file.Backup == "" {
			err = os.Remove(file.Target)
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = copyAtomic(file.Backup, file.Target)
		}
		if err != nil {
			s.RollbackFailed = true
			cause = fmt.Errorf("%v; rollback %s: %w", cause, file.Target, err)
		}
	}
	return cause
}

// Restore only before opening SQLite or after closing it. Preserve the failed
// database and sidecars for diagnosis instead of discarding migration output.
func (m *Manager) restoreDatabase(s *State, cause error) error {
	if s.DatabaseBackup == "" {
		return cause
	}
	if _, err := os.Stat(s.DatabaseBackup); err != nil {
		s.RollbackFailed = true
		return fmt.Errorf("%v; database snapshot unavailable: %w", cause, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(s.DatabasePath+suffix, s.DatabaseBackup+".failed"+suffix); err != nil && !os.IsNotExist(err) {
			s.RollbackFailed = true
			return fmt.Errorf("%v; preserve failed database: %w", cause, err)
		}
	}
	if err := copyAtomic(s.DatabaseBackup, s.DatabasePath); err != nil {
		s.RollbackFailed = true
		return fmt.Errorf("%v; restore database: %w", cause, err)
	}
	return cause
}

func run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "CI=true")
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
	if err != nil {
		if len(output) > 8192 {
			output = output[len(output)-8192:]
		}
		return output, fmt.Errorf("%s failed: %w: %s", name, err, output)
	}
	return output, nil
}
