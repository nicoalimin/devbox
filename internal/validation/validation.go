package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const commandTimeout = 20 * time.Minute

// Command is one local quality gate run before Devbox pushes a branch.
type Command struct {
	Name string
	Args []string
	Dir  string // Relative to the worktree; empty means its root.
}

// Run executes configured validation commands, or detects common CI checks
// when configured is nil. An explicitly empty list disables validation.
func Run(worktreePath string, configured []string, logFunc func(string)) error {
	commands, err := commandsFor(worktreePath, configured)
	if err != nil {
		return err
	}
	return runCommands(worktreePath, commands, logFunc)
}

// optionalPackages may soft-fail install-only workspace protocol errors when a
// primary (non-optional) package gate has already passed in this run.
var optionalPackages = map[string]bool{
	"mobile": true,
}

func runCommands(worktreePath string, commands []Command, logFunc func(string)) error {
	if len(commands) == 0 {
		if logFunc != nil {
			logFunc("No local validation commands detected")
		}
		return nil
	}

	primaryPassed := false
	webPassed := false
	for _, command := range commands {
		display := strings.Join(append([]string{command.Name}, command.Args...), " ")
		if command.Dir != "" {
			display = "[" + command.Dir + "] " + display
		}
		if logFunc != nil {
			logFunc(fmt.Sprintf("Running local validation: %s", display))
		}

		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		output, runErr := runCommandFn(ctx, worktreePath, command)
		cancel()

		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("local validation timed out after %v: %s", commandTimeout, display)
		}
		if runErr != nil {
			if shouldSoftSkipMobileInstall(command, output, primaryPassed) {
				reason := "primary package gates already green"
				if webPassed {
					reason = "web gates already green"
				}
				if logFunc != nil {
					logFunc(fmt.Sprintf("Local validation soft-skipped: %s (EUNSUPPORTEDPROTOCOL; %s)", display, reason))
				}
				continue
			}
			return fmt.Errorf("local validation failed: %s: %w\nOutput:\n%s", display, runErr, tail(output, 16*1024))
		}
		if isPrimaryPackageDir(command.Dir) {
			primaryPassed = true
			if filepath.Base(command.Dir) == "web" || command.Dir == "web" {
				webPassed = true
			}
		}
		if logFunc != nil {
			logFunc(fmt.Sprintf("Local validation passed: %s", display))
		}
	}

	return nil
}

func isOptionalPackageDir(dir string) bool {
	if dir == "" {
		return false
	}
	
	// Check if it's an explicitly listed optional package
	if optionalPackages[filepath.Base(dir)] {
		return true
	}
	
	// Check if it's under packages/ directory (e.g., packages/application)
	return strings.HasPrefix(dir, "packages/")
}

func isPrimaryPackageDir(dir string) bool {
	// Only node package dirs count as primary gates — not bare go commands at "".
	if dir == "" {
		return false
	}
	return !isOptionalPackageDir(dir)
}

func isInstallCommand(command Command) bool {
	switch command.Name {
	case "pnpm", "npm", "yarn", "bun":
		if len(command.Args) == 0 {
			return false
		}
		return command.Args[0] == "install" || command.Args[0] == "ci"
	default:
		return false
	}
}

func isWorkspaceProtocolFailure(output []byte) bool {
	lower := strings.ToLower(string(output))
	if strings.Contains(lower, "eunsupportedprotocol") {
		return true
	}
	if strings.Contains(lower, "unsupported protocol") && strings.Contains(lower, "workspace:") {
		return true
	}
	return false
}

func shouldSoftSkipMobileInstall(command Command, output []byte, primaryPassed bool) bool {
	return primaryPassed && isOptionalPackageDir(command.Dir) && isInstallCommand(command) && isWorkspaceProtocolFailure(output)
}

// runCommandFn is swapped in tests to simulate install failures.
var runCommandFn = runCommand

// Devbox runs on Linux/macOS. Terminate the whole command group on timeout,
// including package-manager and shell children, before taking a Git snapshot.
func runCommand(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = filepath.Join(worktreePath, command.Dir)
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
	return cmd.CombinedOutput()
}

func commandsFor(worktreePath string, configured []string) ([]Command, error) {
	if configured != nil {
		commands := make([]Command, 0, len(configured))
		for _, command := range configured {
			if strings.TrimSpace(command) == "" {
				continue
			}
			commands = append(commands, Command{Name: "/bin/sh", Args: []string{"-c", command}})
		}
		return commands, nil
	}

	var commands []Command
	if fileExists(filepath.Join(worktreePath, "go.mod")) {
		commands = append(commands,
			Command{Name: "go", Args: []string{"test", "./..."}},
			Command{Name: "go", Args: []string{"vet", "./..."}},
		)
	}

	packages, err := packageDirs(worktreePath)
	if err != nil {
		return nil, err
	}
	for _, dir := range packages {
		nodeCommands, err := nodeCommands(filepath.Join(worktreePath, dir), filepath.Join(worktreePath, dir, "package.json"))
		if err != nil {
			return nil, err
		}
		for i := range nodeCommands {
			nodeCommands[i].Dir = dir
		}
		commands = append(commands, nodeCommands...)
	}

	return commands, nil
}

// packageDirs includes nested apps such as web/, while respecting Git's ignores
// so dependencies, generated output, and other worktrees aren't traversed.
func packageDirs(worktreePath string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "package.json", "**/package.json")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		// Non-Git directories are useful for validation in isolation.
		if fileExists(filepath.Join(worktreePath, "package.json")) {
			return []string{""}, nil
		}
		return nil, nil
	}
	seen := map[string]bool{}
	for _, path := range strings.Split(string(output), "\x00") {
		if path == "" || !fileExists(filepath.Join(worktreePath, path)) {
			continue
		}
		if strings.HasPrefix(path, ".devbox-worktrees/") || strings.Contains("/"+path, "/node_modules/") {
			continue
		}
		dir := filepath.Dir(path)
		if dir == "." {
			dir = ""
		}
		seen[dir] = true
	}
	var dirs []string
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// Format runs declared write-mode formatters independently of the model. Custom
// commands are supported for repositories whose tooling cannot be autodetected.
// An explicitly empty list disables automatic formatting.
func Format(worktreePath string, configured []string, logFunc func(string)) error {
	if configured != nil {
		return Run(worktreePath, configured, logFunc)
	}
	dirs, err := packageDirs(worktreePath)
	if err != nil {
		return err
	}
	var commands []Command
	for _, dir := range dirs {
		path := filepath.Join(worktreePath, dir)
		data, err := os.ReadFile(filepath.Join(path, "package.json"))
		if err != nil {
			return err
		}
		var manifest struct {
			PackageManager string            `json:"packageManager"`
			Scripts        map[string]string `json:"scripts"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("invalid package.json in %s: %w", dir, err)
		}
		manager := detectPackageManager(path, manifest.PackageManager)
		formatter := formatterCommand(manager, manifest.Scripts)
		if formatter == nil {
			continue
		}
		// Install before formatting in fresh worktrees. nodeCommands puts the
		// repository's frozen install first, ahead of its checks.
		if _, err := os.Stat(filepath.Join(path, "node_modules")); os.IsNotExist(err) {
			checks, err := nodeCommands(path, filepath.Join(path, "package.json"))
			if err != nil {
				return err
			}
			if len(checks) > 0 {
				checks[0].Dir = dir
				commands = append(commands, checks[0])
			}
		}
		formatter.Dir = dir
		commands = append(commands, *formatter)
	}
	return runCommands(worktreePath, commands, logFunc)
}

func formatterCommand(manager string, scripts map[string]string) *Command {
	for _, name := range []string{"format:write", "format:fix", "format"} {
		if body := strings.TrimSpace(scripts[name]); body != "" && !strings.Contains(body, "--check") && !strings.Contains(body, "--list-different") {
			return &Command{Name: manager, Args: []string{"run", name}}
		}
	}
	// Only infer write mode for a simple declared Prettier check. Preserve its
	// globs/config/ignore options, and never rewrite compound shell programs.
	body := strings.TrimSpace(scripts["format:check"])
	if strings.HasPrefix(body, "prettier ") && strings.Contains(body, "--check") && !strings.ContainsAny(body, ";&|\n`$") {
		body = strings.Replace(body, "--check", "--write", 1)
		prefix := manager + " exec "
		if manager == "npm" {
			prefix = "npm exec --no -- "
		} else if manager == "yarn" || manager == "bun" {
			prefix = manager + " run "
		}
		return &Command{Name: "/bin/sh", Args: []string{"-c", prefix + body}}
	}
	return nil
}

func nodeCommands(worktreePath, packageJSONPath string) ([]Command, error) {
	data, err := os.ReadFile(packageJSONPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read package.json for local validation: %w", err)
	}
	var manifest struct {
		PackageManager string            `json:"packageManager"`
		Scripts        map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse package.json for local validation: %w", err)
	}

	manager := detectPackageManager(worktreePath, manifest.PackageManager)
	if manager == "" {
		return nil, nil
	}

	var commands []Command
	switch manager {
	case "pnpm":
		commands = append(commands, Command{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}})
	case "npm":
		args := []string{"install", "--no-package-lock"}
		if fileExists(filepath.Join(worktreePath, "package-lock.json")) {
			args = []string{"ci"}
		}
		commands = append(commands, Command{Name: "npm", Args: args})
	case "yarn":
		commands = append(commands, Command{Name: "yarn", Args: []string{"install", "--immutable"}})
	case "bun":
		commands = append(commands, Command{Name: "bun", Args: []string{"install", "--frozen-lockfile"}})
	}

	for _, script := range []string{"format:check", "lint", "typecheck", "test", "build"} {
		body, ok := manifest.Scripts[script]
		if !ok || strings.TrimSpace(body) == "" || strings.Contains(body, "no test specified") {
			continue
		}
		commands = append(commands, Command{Name: manager, Args: []string{"run", script}})
	}

	return commands, nil
}

func detectPackageManager(worktreePath, declared string) string {
	if declared != "" {
		name, _, _ := strings.Cut(declared, "@")
		switch name {
		case "pnpm", "npm", "yarn", "bun":
			return name
		}
	}
	for _, candidate := range []struct {
		file    string
		manager string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"package-lock.json", "npm"},
		{"yarn.lock", "yarn"},
		{"bun.lock", "bun"},
		{"bun.lockb", "bun"},
	} {
		if fileExists(filepath.Join(worktreePath, candidate.file)) {
			return candidate.manager
		}
	}
	return "npm"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func tail(output []byte, limit int) string {
	if len(output) <= limit {
		return string(output)
	}
	return "... output truncated ...\n" + string(output[len(output)-limit:])
}
