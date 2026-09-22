package validation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTimeoutStopsShellChildren(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := runCommand(ctx, dir, Command{Name: "/bin/sh", Args: []string{"-c", "sleep 0.2; echo orphan > orphan.txt"}})
	if err == nil {
		t.Fatal("expected command timeout")
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "orphan.txt")); !os.IsNotExist(err) {
		t.Fatalf("child kept writing after timeout: %v", err)
	}
}

func TestCommandsForDetectsCommonGoAndPnpmCIGates(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module example.com/test\n\ngo 1.24\n")
	writeTestFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	writeTestFile(t, filepath.Join(dir, "package.json"), `{
  "packageManager": "pnpm@10.33.3",
  "scripts": {
    "format:check": "prettier --check .",
    "lint": "eslint .",
    "typecheck": "tsc --noEmit",
    "test": "vitest run",
    "build": "next build",
    "dev": "next dev"
  }
}`)

	commands, err := commandsFor(dir, nil)
	if err != nil {
		t.Fatalf("commandsFor failed: %v", err)
	}
	want := []Command{
		{Name: "go", Args: []string{"test", "./..."}},
		{Name: "go", Args: []string{"vet", "./..."}},
		{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}},
		{Name: "pnpm", Args: []string{"run", "format:check"}},
		{Name: "pnpm", Args: []string{"run", "lint"}},
		{Name: "pnpm", Args: []string{"run", "typecheck"}},
		{Name: "pnpm", Args: []string{"run", "test"}},
		{Name: "pnpm", Args: []string{"run", "build"}},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestNestedPackageValidationAndFormatting(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "web", "package.json"), `{"packageManager":"pnpm@10","scripts":{"format:check":"prettier --check .","test":"vitest run"}}`)
	commands, err := commandsFor(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 3 {
		t.Fatalf("commands: %+v", commands)
	}
	for _, command := range commands {
		if command.Dir != "web" || command.Name != "pnpm" {
			t.Fatalf("wrong directory/manager: %+v", command)
		}
	}
	formatter := formatterCommand("pnpm", map[string]string{"format:check": `prettier --check "src/**/*.ts" --ignore-path .prettierignore`})
	if formatter == nil || !strings.Contains(strings.Join(formatter.Args, " "), `--write "src/**/*.ts" --ignore-path .prettierignore`) {
		t.Fatalf("formatter: %+v", formatter)
	}
}

func TestFormatterDetection(t *testing.T) {
	for _, check := range []string{"eslint .", "prettier --check . && deploy", "prettier --check $(command)"} {
		if cmd := formatterCommand("npm", map[string]string{"format:check": check}); cmd != nil {
			t.Fatalf("unsafe inferred formatter for %s: %+v", check, cmd)
		}
	}
	cmd := formatterCommand("pnpm", map[string]string{"format": "prettier --write .", "format:check": "prettier --check ."})
	if cmd == nil || !reflect.DeepEqual(cmd.Args, []string{"run", "format"}) {
		t.Fatalf("did not prefer declared formatter: %+v", cmd)
	}
	if err := Format(t.TempDir(), []string{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCommandsForUsesConfiguredCommandsExactly(t *testing.T) {
	commands, err := commandsFor(t.TempDir(), []string{"make ci", "./scripts/check.sh"})
	if err != nil {
		t.Fatalf("commandsFor failed: %v", err)
	}
	want := []Command{
		{Name: "/bin/sh", Args: []string{"-c", "make ci"}},
		{Name: "/bin/sh", Args: []string{"-c", "./scripts/check.sh"}},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestRunStopsOnValidationFailure(t *testing.T) {
	err := Run(t.TempDir(), []string{"exit 7"}, nil)
	if err == nil {
		t.Fatal("expected validation failure")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func TestShouldSoftSkipMobileInstallHelpers(t *testing.T) {
	cmd := Command{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "mobile"}
	out := []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nnpm ERR! Unsupported URL Type \"workspace:\": workspace:*\n")
	if !shouldSoftSkipMobileInstall(cmd, out, true) {
		t.Fatal("expected soft-skip when primary passed + mobile install protocol error")
	}
	if shouldSoftSkipMobileInstall(cmd, out, false) {
		t.Fatal("must hard-fail when no primary package succeeded yet")
	}
	lint := Command{Name: "pnpm", Args: []string{"run", "lint"}, Dir: "mobile"}
	if shouldSoftSkipMobileInstall(lint, out, true) {
		t.Fatal("non-install mobile failures must stay hard-fail")
	}
	other := []byte("npm ERR! code E404\nnpm ERR! 404 Not Found\n")
	if shouldSoftSkipMobileInstall(cmd, other, true) {
		t.Fatal("non-protocol mobile install failures must stay hard-fail")
	}
	webInstall := Command{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "web"}
	if shouldSoftSkipMobileInstall(webInstall, out, true) {
		t.Fatal("web install must never soft-skip")
	}
}

func TestShouldSoftSkipPackagesInstallHelpers(t *testing.T) {
	cmd := Command{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "packages/application"}
	out := []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nnpm ERR! Unsupported URL Type \"workspace:\": workspace:*\n")
	if !shouldSoftSkipMobileInstall(cmd, out, true) {
		t.Fatal("expected soft-skip when primary passed + packages install protocol error")
	}
	if shouldSoftSkipMobileInstall(cmd, out, false) {
		t.Fatal("must hard-fail when no primary package succeeded yet")
	}
	lint := Command{Name: "npm", Args: []string{"run", "lint"}, Dir: "packages/application"}
	if shouldSoftSkipMobileInstall(lint, out, true) {
		t.Fatal("non-install packages failures must stay hard-fail")
	}
	other := []byte("npm ERR! code E404\nnpm ERR! 404 Not Found\n")
	if shouldSoftSkipMobileInstall(cmd, other, true) {
		t.Fatal("non-protocol packages install failures must stay hard-fail")
	}
	// Test that non-packages directory doesn't soft-skip
	appInstall := Command{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "app"}
	if shouldSoftSkipMobileInstall(appInstall, out, true) {
		t.Fatal("non-packages install must not soft-skip")
	}
}

func TestRunCommandsSoftSkipsMobileProtocolAfterWeb(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	var logs []string
	logFunc := func(s string) { logs = append(logs, s) }

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		if command.Dir == "web" {
			return []byte("ok"), nil
		}
		if command.Dir == "mobile" && isInstallCommand(command) {
			return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nUnsupported URL Type \"workspace:\": workspace:*\n"), fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "web"},
		{Name: "pnpm", Args: []string{"run", "typecheck"}, Dir: "web"},
		{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "mobile"},
	}, logFunc)
	if err != nil {
		t.Fatalf("expected soft-skip success, got %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "Local validation soft-skipped: [mobile] pnpm install --frozen-lockfile (EUNSUPPORTEDPROTOCOL; web gates already green)") {
		t.Fatalf("missing soft-skip log: %s", joined)
	}
}

func TestRunCommandsSoftSkipsPackagesProtocolAfterPrimary(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	var logs []string
	logFunc := func(s string) { logs = append(logs, s) }

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		if command.Dir == "web" {
			return []byte("ok"), nil
		}
		if command.Dir == "packages/application" && isInstallCommand(command) {
			return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nUnsupported URL Type \"workspace:\": workspace:*\n"), fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "web"},
		{Name: "pnpm", Args: []string{"run", "typecheck"}, Dir: "web"},
		{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "packages/application"},
	}, logFunc)
	if err != nil {
		t.Fatalf("expected soft-skip success, got %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "Local validation soft-skipped: [packages/application] npm install --no-package-lock (EUNSUPPORTEDPROTOCOL; web gates already green)") {
		t.Fatalf("missing soft-skip log: %s", joined)
	}
}

func TestRunCommandsHardFailsPackagesProtocolWithoutPrimary(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nworkspace:*\n"), fmt.Errorf("exit status 1")
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "packages/application"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "local validation failed") {
		t.Fatalf("expected hard-fail, got %v", err)
	}
}

func TestRunCommandsHardFailsNonPackagesProtocol(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nworkspace:*\n"), fmt.Errorf("exit status 1")
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "some-other-dir"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "local validation failed") {
		t.Fatalf("expected hard-fail, got %v", err)
	}
}

func TestRunCommandsHardFailsMobileProtocolWithoutPrimary(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nworkspace:*\n"), fmt.Errorf("exit status 1")
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "mobile"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "local validation failed") {
		t.Fatalf("expected hard-fail, got %v", err)
	}
}

func TestRunCommandsHardFailsNonProtocolMobile(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		if command.Dir == "web" {
			return []byte("ok"), nil
		}
		return []byte("eslint found 3 errors\n"), fmt.Errorf("exit status 1")
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "pnpm", Args: []string{"run", "lint"}, Dir: "web"},
		{Name: "pnpm", Args: []string{"run", "lint"}, Dir: "mobile"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "local validation failed") {
		t.Fatalf("expected hard-fail for mobile lint, got %v", err)
	}
}

func TestShouldSoftSkipTestsInstallHelpers(t *testing.T) {
	cmd := Command{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "tests/example-use-case"}
	out := []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nnpm ERR! Unsupported URL Type \"workspace:\": workspace:*\n")
	if !shouldSoftSkipMobileInstall(cmd, out, true) {
		t.Fatal("expected soft-skip when primary passed + tests/* install protocol error")
	}
	if shouldSoftSkipMobileInstall(cmd, out, false) {
		t.Fatal("must hard-fail when no primary package succeeded yet")
	}
	lint := Command{Name: "npm", Args: []string{"run", "lint"}, Dir: "tests/example-use-case"}
	if shouldSoftSkipMobileInstall(lint, out, true) {
		t.Fatal("non-install tests/* failures must stay hard-fail")
	}
	other := []byte("npm ERR! code E404\nnpm ERR! 404 Not Found\n")
	if shouldSoftSkipMobileInstall(cmd, other, true) {
		t.Fatal("non-protocol tests/* install failures must stay hard-fail")
	}
}

func TestRunCommandsSoftSkipsTestsProtocolAfterPrimary(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	var logs []string
	logFunc := func(s string) { logs = append(logs, s) }

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		if command.Dir == "web" {
			return []byte("ok"), nil
		}
		if command.Dir == "tests/example-use-case" && isInstallCommand(command) {
			return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nUnsupported URL Type \"workspace:\": workspace:*\n"), fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}, Dir: "web"},
		{Name: "pnpm", Args: []string{"run", "typecheck"}, Dir: "web"},
		{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "tests/example-use-case"},
	}, logFunc)
	if err != nil {
		t.Fatalf("expected soft-skip success, got %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "Local validation soft-skipped: [tests/example-use-case] npm install --no-package-lock (EUNSUPPORTEDPROTOCOL; web gates already green)") {
		t.Fatalf("missing soft-skip log: %s", joined)
	}
}

func TestRunCommandsHardFailsTestsProtocolWithoutPrimary(t *testing.T) {
	orig := runCommandFn
	defer func() { runCommandFn = orig }()

	runCommandFn = func(ctx context.Context, worktreePath string, command Command) ([]byte, error) {
		return []byte("npm ERR! code EUNSUPPORTEDPROTOCOL\nworkspace:*\n"), fmt.Errorf("exit status 1")
	}

	err := runCommands(t.TempDir(), []Command{
		{Name: "npm", Args: []string{"install", "--no-package-lock"}, Dir: "tests/example-use-case"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "local validation failed") {
		t.Fatalf("expected hard-fail, got %v", err)
	}
}
