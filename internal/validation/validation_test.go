package validation

import (
	"context"
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
