package validation

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const missingMembers = "Type 'InMemoryCatalogStore' is missing the following properties from type 'CatalogStore': createTransferDraft, addTransferItems, findTransferById, findTransferWithItems, and 5 more."

// uta94Fixture mimics the UTA-94 typecheck output: the root TS6133 error at
// the head, then 44 repeats of the same TS2740 across two test files, padded
// well past the old 16KB tail window.
func uta94Fixture() string {
	var b strings.Builder
	b.WriteString("> @tokoboss/api@0.0.1 typecheck /tmp/devbox-abc/.devbox-worktrees/uta-94-3/packages/api\n> tsc --noEmit\n\n")
	b.WriteString("src/stores/in-memory-catalog-store.ts(41,11): error TS6133: 'nextTransferId' is declared but its value is never read.\n")
	for i := 0; i < 44; i++ {
		file := "test/transfers.test.ts"
		if i%2 == 1 {
			file = "test/catalog.test.ts"
		}
		fmt.Fprintf(&b, "%s(%d,%d): error TS2740: %s\n", file, 10+i, 20, missingMembers)
		b.WriteString(strings.Repeat("padding noise from a very chatty tool ", 12) + "\n")
	}
	b.WriteString("\nFound 45 errors in 3 files.\n")
	return b.String()
}

func TestExtractTSErrorsDedupesAndKeepsHeadErrors(t *testing.T) {
	out := uta94Fixture()
	if len(out) <= 16*1024 {
		t.Fatalf("fixture must exceed the old 16KB tail window, got %d bytes", len(out))
	}
	summary := ExtractTSErrors(out)
	if summary.Total != 45 || summary.Distinct() != 2 {
		t.Fatalf("total=%d distinct=%d, want 45/2: %+v", summary.Total, summary.Distinct(), summary.Errors)
	}
	if first := summary.Errors[0]; first.Code != "TS6133" || first.File != "src/stores/in-memory-catalog-store.ts" || first.Line != 41 {
		t.Fatalf("root head error not kept first: %+v", first)
	}
	if second := summary.Errors[1]; second.Code != "TS2740" || second.Count != 44 || len(second.Locations) != maxLocations {
		t.Fatalf("repeated error not collapsed: %+v", second)
	}
	if !summary.Elided {
		t.Fatal("'and 5 more' elision not flagged")
	}
	report := summary.Format(MaxReportedErrors)
	for _, want := range []string{"45 total, 2 distinct", "TS6133", "'nextTransferId' is declared", "TS2740", missingMembers, "44 occurrences", "Open the named interface"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	if strings.Count(report, "error TS2740") != 1 {
		t.Fatalf("duplicate TS2740 lines in report:\n%s", report)
	}
}

func TestExtractTSErrorsContinuationLinesAndStyles(t *testing.T) {
	out := strings.Join([]string{
		"packages/api typecheck: src/a.ts(3,7): error TS2345: Argument of type 'InMemoryCatalogStore' is not assignable to parameter of type 'CatalogStore'.",
		"packages/api typecheck:   " + missingMembers,
		"packages/api typecheck: src/b.ts(9,1): error TS2345: Argument of type 'InMemoryCatalogStore' is not assignable to parameter of type 'CatalogStore'.",
		"packages/api typecheck:   " + missingMembers,
		"\x1b[96msrc/c.ts\x1b[0m:\x1b[93m5\x1b[0m:\x1b[93m2\x1b[0m - \x1b[91merror\x1b[0m\x1b[90m TS2304: \x1b[0mCannot find name 'foo'.",
		"",
		"5 foo()",
		"packages/api typecheck: Failed",
	}, "\n")
	summary := ExtractTSErrors(out)
	if summary.Total != 3 || summary.Distinct() != 2 {
		t.Fatalf("total=%d distinct=%d: %+v", summary.Total, summary.Distinct(), summary.Errors)
	}
	if e := summary.Errors[0]; !strings.Contains(e.Message, "\n  "+missingMembers) || e.Count != 2 {
		t.Fatalf("continuation not captured: %+v", e)
	}
	if e := summary.Errors[1]; e.Code != "TS2304" || e.File != "src/c.ts" || e.Line != 5 || e.Col != 2 {
		t.Fatalf("pretty style not parsed: %+v", e)
	}
}

func TestFormatCapsDistinctErrors(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "src/f%d.ts(1,1): error TS2304: Cannot find name 'x%d'.\n", i, i)
	}
	report := ExtractTSErrors(b.String()).Format(MaxReportedErrors)
	if !strings.Contains(report, "40 total, 40 distinct (showing first 30)") || !strings.Contains(report, "10 more distinct error(s) omitted") {
		t.Fatal(report)
	}
	if strings.Contains(report, "x30'") {
		t.Fatalf("report exceeded cap:\n%s", report)
	}
}

func TestSignatureStableAcrossOrderPathsAndTimestamps(t *testing.T) {
	a := strings.Join([]string{
		"[10:31:02 AM] Starting compilation",
		"/tmp/devbox-111/.devbox-worktrees/uta-94-1/src/a.ts(3,7): error TS2740: " + missingMembers,
		"src/b.ts(9,1): error TS6133: 'nextTransferId' is declared but its value is never read.",
		"src/b.ts(12,1): error TS6133: 'nextTransferId' is declared but its value is never read.",
	}, "\n")
	b := strings.Join([]string{
		"[11:02:59 PM] Starting compilation",
		"src/b.ts(40,3): error TS6133: 'nextTransferId' is declared but its value is never read.",
		"/home/box/repos/tokoboss/.devbox-worktrees/uta-94-7/src/a.ts(80,2): error TS2740: " + missingMembers,
	}, "\n")
	if sa, sb := Signature("pnpm run typecheck", a, ""), Signature("pnpm run typecheck", b, ""); sa != sb || sa == "" {
		t.Fatalf("signatures differ: %s vs %s", sa, sb)
	}
	root := "/srv/work/wt-1"
	c := root + "/src/a.ts(1,1): error TS2740: " + missingMembers
	d := "src/a.ts(2,2): error TS2740: " + missingMembers
	if Signature("x", c, root) != Signature("y", d, "/other") {
		t.Fatal("worktree root not stripped from signature")
	}
	changed := strings.Replace(a, "nextTransferId", "nextTransferItemId", -1)
	if Signature("pnpm run typecheck", a, "") == Signature("pnpm run typecheck", changed, "") {
		t.Fatal("different errors produced the same signature")
	}
}

func TestGenericFailureKeepsHeadAndTail(t *testing.T) {
	out := "ROOT CAUSE: missing env\n" + strings.Repeat("x", 40*1024) + "\nfinal summary line\n"
	f := NewFailure("make test", errors.New("exit status 2"), []byte(out), "")
	msg := f.Error()
	if !strings.Contains(msg, "ROOT CAUSE: missing env") || !strings.Contains(msg, "final summary line") || !strings.Contains(msg, "bytes omitted") {
		t.Fatalf("generic fallback lost head or tail: %d bytes", len(msg))
	}
	if len(msg) > genericHeadLimit+genericTailLimit+512 {
		t.Fatalf("generic summary too large: %d", len(msg))
	}
	var failure *Failure
	if !errors.As(error(f), &failure) || failure.Signature == "" {
		t.Fatal("failure not discoverable via errors.As")
	}
	if Signature("make test", "12:00:01 FAIL a (31ms)", "") != Signature("make test", "13:10:09 FAIL a (95ms)", "") {
		t.Fatal("generic signature not stable across timestamps/durations")
	}
}

func TestRunReturnsDedupedFailure(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir+"/out.txt", uta94Fixture())
	err := Run(dir, []string{"cat out.txt; exit 2"}, nil)
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("expected *Failure, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "TS6133") || strings.Count(err.Error(), "error TS2740") != 1 {
		t.Fatalf("Run error not deduped:\n%s", err)
	}
}
