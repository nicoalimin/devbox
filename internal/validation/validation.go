package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const commandTimeout = 20 * time.Minute

// Command is one local quality gate run before Devbox pushes a branch.
type Command struct {
	Name string
	Args []string
}

// Run executes configured validation commands, or detects common CI checks
// when configured is nil. An explicitly empty list disables validation.
func Run(worktreePath string, configured []string, logFunc func(string)) error {
	commands, err := commandsFor(worktreePath, configured)
	if err != nil {
		return err
	}
	if len(commands) == 0 {
		if logFunc != nil {
			logFunc("No local validation commands detected")
		}
		return nil
	}

	for _, command := range commands {
		display := strings.Join(append([]string{command.Name}, command.Args...), " ")
		if logFunc != nil {
			logFunc(fmt.Sprintf("Running local validation: %s", display))
		}

		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		cmd := exec.CommandContext(ctx, command.Name, command.Args...)
		cmd.Dir = worktreePath
		output, runErr := cmd.CombinedOutput()
		cancel()

		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("local validation timed out after %v: %s", commandTimeout, display)
		}
		if runErr != nil {
			return fmt.Errorf("local validation failed: %s: %w\nOutput:\n%s", display, runErr, tail(output, 16*1024))
		}
		if logFunc != nil {
			logFunc(fmt.Sprintf("Local validation passed: %s", display))
		}
	}

	return nil
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

	packageJSONPath := filepath.Join(worktreePath, "package.json")
	if fileExists(packageJSONPath) {
		nodeCommands, err := nodeCommands(worktreePath, packageJSONPath)
		if err != nil {
			return nil, err
		}
		commands = append(commands, nodeCommands...)
	}

	return commands, nil
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
