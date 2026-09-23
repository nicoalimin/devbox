package job

import (
	"regexp"
	"strings"

	"github.com/nicoalimin/devbox/internal/db"
)

// ContinuePROptions holds parsed continue-existing-PR delivery hints from
// free-form job.OperatorContext. When PushRef is set, the host aligns the
// worktree onto the assigned branch (keeping commits), dual-pushes to PushRef,
// and prefers the existing PR for that head instead of opening a new one.
//
// UTA-96: when reuse is active, prepareWorktree checks out PushRef (or the
// prior job's branch) directly so assigned branch == push_ref and dual-push
// becomes a no-op.
type ContinuePROptions struct {
	PushRef    string
	ContinuePR bool
}

var (
	pushRefRe    = regexp.MustCompile(`(?im)^\s*push_ref\s*[:=]\s*(\S+)\s*$`)
	continuePRRe = regexp.MustCompile(`(?im)^\s*continue_pr\s*[:=]\s*(true|yes|1)\s*$`)
)

// ParseContinuePRContext extracts push_ref / continue_pr from free-form
// operator context. Keys are case-insensitive. PushRef alone is enough to
// enable continue delivery; continue_pr without push_ref is recorded but does
// not activate align/dual-push by itself (CoS should include push_ref, or the
// host resolves it from a prior job — see ResolveReusePlan).
func ParseContinuePRContext(operatorContext string) ContinuePROptions {
	opts := ContinuePROptions{}
	if strings.TrimSpace(operatorContext) == "" {
		return opts
	}
	if m := pushRefRe.FindStringSubmatch(operatorContext); len(m) == 2 {
		opts.PushRef = strings.TrimSpace(m[1])
	}
	if continuePRRe.MatchString(operatorContext) {
		opts.ContinuePR = true
	}
	return opts
}

// Active reports whether continue delivery (align + dual-push + prefer existing PR) should run.
func (o ContinuePROptions) Active() bool {
	return o.PushRef != ""
}

// ReusePlan describes whether prepareWorktree should reattach / checkout an
// existing PR branch instead of creating a new numbered branch from main.
type ReusePlan struct {
	Reuse              bool
	Ref                string // assigned branch == PR head to iterate on
	PreferWorktreePath string
	PRURL              string
	RepoPath           string
}

// ResolveReusePlan decides whether to reuse an existing worktree/branch/PR.
// Reuse when:
//   - operator_context has push_ref, or
//   - operator_context has continue_pr and a prior job supplies a branch, or
//   - a prior job for the same issue already has an open PR (or branch+worktree).
func ResolveReusePlan(opts ContinuePROptions, prior *db.Job) ReusePlan {
	ref := strings.TrimSpace(opts.PushRef)
	var preferPath, prURL, repoPath string
	if prior != nil {
		preferPath = prior.WorktreePath
		prURL = prior.PRURL
		repoPath = prior.RepoPath
		if ref == "" && prior.BranchName != "" {
			if opts.ContinuePR || prior.PRURL != "" || prior.WorktreePath != "" {
				ref = prior.BranchName
			}
		}
	}
	if ref == "" {
		return ReusePlan{}
	}
	return ReusePlan{
		Reuse:              true,
		Ref:                ref,
		PreferWorktreePath: preferPath,
		PRURL:              prURL,
		RepoPath:           repoPath,
	}
}

// EnsureContinueMarker returns operator context with continue_pr: true injected
// when missing. Used by `devbox assign --continue`.
func EnsureContinueMarker(operatorContext string) string {
	if continuePRRe.MatchString(operatorContext) {
		return operatorContext
	}
	marker := "continue_pr: true"
	if strings.TrimSpace(operatorContext) == "" {
		return marker
	}
	return marker + "\n\n" + operatorContext
}
