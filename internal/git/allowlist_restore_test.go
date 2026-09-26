package git

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRestorePathsToBase_DropsOutOfScopeEdits(t *testing.T) {
	repo := setupTestRepo(t)
	t.Cleanup(func() { os.RemoveAll(repo) })
	mgr := NewManager(repo, "main")
	wt, err := mgr.CreateWorktree("UTA-9")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt.Path, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt.Path, "src/in-scope.ts", "ok\n", "in scope")
	commitFile(t, wt.Path, "junk.md", "junk\n", "junk")

	if err := mgr.RestorePathsToBase(wt.Path, []string{"junk.md"}, "revert"); err != nil {
		t.Fatal(err)
	}
	changed, err := mgr.ChangedFilesVsBase(wt.Path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/in-scope.ts"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed=%v want %v", changed, want)
	}
}
