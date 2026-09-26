package job

import (
	"testing"

	"github.com/nicoalimin/devbox/internal/db"
)

func TestParseContinuePRContext(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantRef string
		wantPR  bool
		active  bool
	}{
		{name: "empty", input: "", wantRef: "", wantPR: false, active: false},
		{
			name:    "colon form",
			input:   "continue_pr: true\npush_ref: devbox/uta-82-32\n",
			wantRef: "devbox/uta-82-32",
			wantPR:  true,
			active:  true,
		},
		{
			name:    "equals form case-insensitive",
			input:   "CONTINUE_PR=true\nPUSH_REF=devbox/uta-82-32",
			wantRef: "devbox/uta-82-32",
			wantPR:  true,
			active:  true,
		},
		{
			name:    "push_ref alone activates",
			input:   "Please continue.\npush_ref: feat/existing\n",
			wantRef: "feat/existing",
			wantPR:  false,
			active:  true,
		},
		{
			name:    "continue_pr alone does not activate",
			input:   "continue_pr: true\nreset onto existing tip",
			wantRef: "",
			wantPR:  true,
			active:  false,
		},
		{
			name:    "false continue_pr ignored",
			input:   "continue_pr: false\npush_ref: keep-me",
			wantRef: "keep-me",
			wantPR:  false,
			active:  true,
		},
		{
			name:    "inline prose with keys on own lines",
			input:   "Operator notes:\n\npush_ref = cos/old-branch\ncontinue_pr = yes\n\nDo the thing.",
			wantRef: "cos/old-branch",
			wantPR:  true,
			active:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseContinuePRContext(tt.input)
			if got.PushRef != tt.wantRef {
				t.Fatalf("PushRef=%q want %q", got.PushRef, tt.wantRef)
			}
			if got.ContinuePR != tt.wantPR {
				t.Fatalf("ContinuePR=%v want %v", got.ContinuePR, tt.wantPR)
			}
			if got.Active() != tt.active {
				t.Fatalf("Active()=%v want %v", got.Active(), tt.active)
			}
		})
	}
}

func TestResolveReusePlan_FreshVsReuse(t *testing.T) {
	prior := &db.Job{
		ID:           "prior-1",
		BranchName:   "devbox/uta-94-5",
		WorktreePath: "/tmp/wt-uta-94-5",
		PRURL:        "https://github.com/example/repo/pull/35",
		RepoPath:     "/code/tokoboss",
	}

	t.Run("fresh assign no prior", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{}, nil, PRStatusUnknown)
		if plan.Reuse {
			t.Fatal("expected fresh assign")
		}
	})

	t.Run("push_ref alone triggers reuse", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{PushRef: "devbox/uta-94-5"}, nil, PRStatusUnknown)
		if !plan.Reuse || plan.Ref != "devbox/uta-94-5" {
			t.Fatalf("plan=%+v", plan)
		}
	})

	t.Run("continue_pr with prior branch reuses prior", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{ContinuePR: true}, prior, PRStatusOpen)
		if !plan.Reuse || plan.Ref != "devbox/uta-94-5" {
			t.Fatalf("plan=%+v", plan)
		}
		if plan.PreferWorktreePath != prior.WorktreePath || plan.PRURL != prior.PRURL {
			t.Fatalf("did not inherit prior metadata: %+v", plan)
		}
	})

	t.Run("prior open PR auto-reuses without markers", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{}, prior, PRStatusOpen)
		if !plan.Reuse || plan.Ref != "devbox/uta-94-5" {
			t.Fatalf("expected auto-reuse from prior PR, got %+v", plan)
		}
	})

	t.Run("push_ref wins over prior branch", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{PushRef: "devbox/uta-94-3"}, prior, PRStatusOpen)
		if !plan.Reuse || plan.Ref != "devbox/uta-94-3" {
			t.Fatalf("plan=%+v", plan)
		}
		// Still inherit worktree/PR hints from prior for reattach / PR URL.
		if plan.PRURL != prior.PRURL {
			t.Fatalf("expected prior PRURL inheritance, got %+v", plan)
		}
	})

	t.Run("prior without pr or worktree does not auto-reuse", func(t *testing.T) {
		thin := &db.Job{ID: "thin", BranchName: "devbox/uta-1"}
		plan := ResolveReusePlan(ContinuePROptions{}, thin, PRStatusUnknown)
		if plan.Reuse {
			t.Fatalf("unexpected reuse: %+v", plan)
		}
	})

	t.Run("closed prior PR does not auto-reuse (UTA-101 regression)", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{}, prior, PRStatusClosed)
		if plan.Reuse {
			t.Fatalf("closed PR must not be auto-reused: %+v", plan)
		}
	})

	t.Run("unverified prior PR does not auto-reuse", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{}, prior, PRStatusUnknown)
		if plan.Reuse {
			t.Fatalf("unverified PR must not be auto-reused: %+v", plan)
		}
	})

	t.Run("leftover worktree without PR does not auto-reuse", func(t *testing.T) {
		wtOnly := &db.Job{ID: "wt", BranchName: "devbox/uta-2", WorktreePath: "/tmp/wt"}
		plan := ResolveReusePlan(ContinuePROptions{}, wtOnly, PRStatusUnknown)
		if plan.Reuse {
			t.Fatalf("worktree alone must not trigger reuse: %+v", plan)
		}
	})

	t.Run("explicit continue on closed PR reuses branch but drops PR URL", func(t *testing.T) {
		plan := ResolveReusePlan(ContinuePROptions{ContinuePR: true}, prior, PRStatusClosed)
		if !plan.Reuse || plan.Ref != prior.BranchName {
			t.Fatalf("plan=%+v", plan)
		}
		if plan.PRURL != "" {
			t.Fatalf("closed PR URL must not be inherited: %+v", plan)
		}
	})
}

func TestEnsureContinueMarker(t *testing.T) {
	if got := EnsureContinueMarker(""); got != "continue_pr: true" {
		t.Fatalf("empty: %q", got)
	}
	got := EnsureContinueMarker("do the thing")
	if got != "continue_pr: true\n\ndo the thing" {
		t.Fatalf("inject: %q", got)
	}
	existing := "continue_pr: true\npush_ref: x"
	if got := EnsureContinueMarker(existing); got != existing {
		t.Fatalf("idempotent: %q", got)
	}
}
