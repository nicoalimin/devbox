package job

import (
	"strings"
	"testing"
)

func TestParseAllowlistBlock(t *testing.T) {
	text := `## Scope

ALLOWLIST:
packages/database/src/repositories/
packages/database/src/__tests__/
` + "`packages/database/src/repositories/drizzle-catalog-repository.ts`" + `

## Forbidden
packages/application/**
`
	got := ParseAllowlist(text)
	if len(got) != 3 {
		t.Fatalf("expected 3 prefixes, got %v", got)
	}
	if got[0] != "packages/database/src/repositories/" {
		t.Errorf("got[0]=%q", got[0])
	}
	if got[1] != "packages/database/src/__tests__/" {
		t.Errorf("got[1]=%q", got[1])
	}
}

func TestParseAllowlistAbsent(t *testing.T) {
	if got := ParseAllowlist("no allowlist here"); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestAllowlistViolationsForbidden(t *testing.T) {
	allow := []string{"packages/database/src/repositories/", "packages/database/src/__tests__/"}
	changed := []string{
		"UTA-101-AC.md",
		"infra/drizzle/meta/0013_snapshot.json",
	}
	bad := AllowlistViolations(allow, changed)
	if len(bad) != 2 {
		t.Fatalf("expected 2 violations, got %v", bad)
	}
}

func TestAllowlistViolationsAllowedOnly(t *testing.T) {
	allow := []string{"packages/database/src/repositories/", "packages/database/src/__tests__/"}
	changed := []string{
		"packages/database/src/repositories/drizzle-catalog-repository.ts",
		"packages/database/src/__tests__/transfers-drizzle-header.test.ts",
	}
	if bad := AllowlistViolations(allow, changed); len(bad) != 0 {
		t.Fatalf("expected no violations, got %v", bad)
	}
}

func TestAllowlistInactiveWhenEmpty(t *testing.T) {
	if bad := AllowlistViolations(nil, []string{"anything.go"}); bad != nil {
		t.Fatalf("expected nil, got %v", bad)
	}
}

func TestFormatAllowlistFailure(t *testing.T) {
	msg := FormatAllowlistFailure([]string{"a/"}, []string{"b.md"})
	if !strings.Contains(msg, "allowlist push gate failed") || !strings.Contains(msg, "b.md") {
		t.Fatalf("unexpected msg: %s", msg)
	}
}

func TestParseAllowlistMergeSources(t *testing.T) {
	linear := "ALLOWLIST:\npackages/database/src/repositories/\n"
	op := "ALLOWLIST:\npackages/database/src/__tests__/\n"
	got := ParseAllowlist(linear, op)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %v", got)
	}
}
