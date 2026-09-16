package main

import "testing"

func TestShouldUseTUIByDefault(t *testing.T) {
	t.Setenv("DEVBOX_NO_TUI", "")

	if !shouldUseTUI(false) {
		t.Fatal("expected TUI to be enabled by default")
	}
}

func TestShouldUseTUICanBeDisabled(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		t.Setenv("DEVBOX_NO_TUI", "")
		if shouldUseTUI(true) {
			t.Fatal("expected --no-tui to disable the TUI")
		}
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv("DEVBOX_NO_TUI", "1")
		if shouldUseTUI(false) {
			t.Fatal("expected DEVBOX_NO_TUI to disable the TUI")
		}
	})
}
