package job

import (
	"regexp"
	"strings"
)

// ContinuePROptions holds parsed continue-existing-PR delivery hints from
// free-form job.OperatorContext. When PushRef is set, the host aligns the
// worktree onto the assigned branch (keeping commits), dual-pushes to PushRef,
// and prefers the existing PR for that head instead of opening a new one.
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
// not activate align/dual-push (CoS must include push_ref).
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
