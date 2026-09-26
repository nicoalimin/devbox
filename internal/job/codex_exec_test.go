package job

import (
	"os"
	"strings"
	"testing"
)

func TestCodexExecArgvOmitsApproveForMe(t *testing.T) {
	// Guard against regressing the UTA-104 Codex CLI flag conflict:
	// --sandbox and --approve-for-me are mutually exclusive.
	src, err := os.ReadFile("codex_exec.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, `"--sandbox"`) || !strings.Contains(body, `"workspace-write"`) {
		t.Fatal("expected --sandbox workspace-write in runCodexExec")
	}
	if strings.Contains(body, `"--approve-for-me"`) {
		t.Fatal("runCodexExec still passes --approve-for-me (conflicts with --sandbox)")
	}
}
