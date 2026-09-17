package validation

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
