package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/nicoalimin/devbox/internal/db"
)

// PRStatus represents the lifecycle state of a GitHub PR relevant to the reconciler.
type PRStatus string

const (
	PRStatusOpen   PRStatus = "OPEN"
	PRStatusMerged PRStatus = "MERGED"
	PRStatusClosed PRStatus = "CLOSED"
)

// ErrPRNotFound is returned when the PR cannot be found on GitHub.
// Policy choice (documented per UTA-68): treat not-found as CLOSED/cancelled,
// because a missing PR will never merge and would otherwise leave the job
// stuck in pr_open forever. Transient gh/API errors (non-not-found) are
// logged and skipped without mutation.
var ErrPRNotFound = errors.New("PR not found")

// defaultReconcilerInterval is used when no interval is configured.
const defaultReconcilerInterval = 2 * time.Minute

// isReconcilerEnabled reports whether the periodic reconciler should run.
func (o *Orchestrator) isReconcilerEnabled() bool {
	if o.cfg == nil {
		return false
	}
	return o.cfg.Reconciler.Enabled
}

// getReconcilerInterval returns the configured interval with a safe default.
func (o *Orchestrator) getReconcilerInterval() time.Duration {
	if o.cfg == nil || o.cfg.Reconciler.Interval <= 0 {
		return defaultReconcilerInterval
	}
	return o.cfg.Reconciler.Interval
}

// StartReconciler launches the periodic GitHub reconciler goroutine.
// It polls GitHub for jobs stuck in pr_open and advances them to a terminal
// state when the PR is merged or closed. It returns immediately; the loop
// runs in the background until the process exits.
func (o *Orchestrator) StartReconciler() {
	go o.StartReconcilerWithStop(nil)
}

// StartReconcilerWithStop is StartReconciler with an optional stop channel
// (nil = run forever). Exposed for tests.
func (o *Orchestrator) StartReconcilerWithStop(stop <-chan struct{}) {
	if !o.isReconcilerEnabled() {
		log.Printf("[reconciler] disabled, not starting")
		return
	}
	interval := o.getReconcilerInterval()
	log.Printf("[reconciler] starting with interval %s", interval)

	// Reconcile once immediately so a merged PR doesn't wait a full interval
	// after a restart, then on every tick.
	o.reconcileJobs()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			o.reconcileJobs()
		case <-stop:
			// nil channel blocks forever, so this case only fires when a
			// non-nil stop channel is closed.
			log.Printf("[reconciler] stopping")
			return
		}
	}
}

// reconcileJobs advances pr_open jobs whose GitHub PR has reached a terminal
// state. It never mutates non-pr_open jobs or jobs without a pr_url.
func (o *Orchestrator) reconcileJobs() {
	jobs, err := o.db.ListJobs(0)
	if err != nil {
		log.Printf("[reconciler] failed to list jobs: %v", err)
		return
	}

	for _, job := range jobs {
		// Only consider jobs waiting on external PR state.
		if job.State != db.StatePROpen {
			continue
		}
		// Skip jobs with no PR to check (e.g. PR creation failed silently).
		if strings.TrimSpace(job.PRURL) == "" {
			continue
		}

		status, prNumber, err := o.getPRStatus(job.PRURL)
		if err != nil {
			if errors.Is(err, ErrPRNotFound) {
				o.reconcileNotFound(job, prNumber)
				continue
			}
			msg := fmt.Sprintf("reconciler: failed to check PR %s, skipping: %v", job.PRURL, err)
			o.log(job.ID, "warn", msg)
			log.Printf("[reconciler] job %s: %s", job.ID, msg)
			continue
		}

		switch status {
		case PRStatusMerged:
			o.reconcileMerged(job, prNumber)
		case PRStatusClosed:
			o.reconcileClosed(job, prNumber)
		case PRStatusOpen:
			// Leave unchanged.
		default:
			msg := fmt.Sprintf("reconciler: unknown PR status %q for %s, skipping", status, job.PRURL)
			o.log(job.ID, "warn", msg)
			log.Printf("[reconciler] job %s: %s", job.ID, msg)
		}
	}
}

// reconcileMerged marks a pr_open job done when its PR was merged.
func (o *Orchestrator) reconcileMerged(job *db.Job, prNumber int) {
	now := time.Now()
	job.State = db.StateDone
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		msg := fmt.Sprintf("reconciler: failed to mark job done after PR merge: %v", err)
		o.log(job.ID, "error", msg)
		log.Printf("[reconciler] job %s: %s", job.ID, msg)
		return
	}
	msg := fmt.Sprintf("PR #%d merged → done", prNumber)
	if prNumber == 0 {
		msg = fmt.Sprintf("PR %s merged → done", job.PRURL)
	}
	o.log(job.ID, "info", msg)
	log.Printf("[reconciler] job %s %s", job.ID, msg)
}

// reconcileClosed marks a pr_open job cancelled when its PR was closed unmerged.
func (o *Orchestrator) reconcileClosed(job *db.Job, prNumber int) {
	now := time.Now()
	job.State = db.StateCancelled
	job.BlockerReason = "PR closed without merge"
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		msg := fmt.Sprintf("reconciler: failed to mark job cancelled after PR close: %v", err)
		o.log(job.ID, "error", msg)
		log.Printf("[reconciler] job %s: %s", job.ID, msg)
		return
	}
	msg := fmt.Sprintf("PR #%d closed without merge → cancelled", prNumber)
	if prNumber == 0 {
		msg = fmt.Sprintf("PR %s closed without merge → cancelled", job.PRURL)
	}
	o.log(job.ID, "info", msg)
	log.Printf("[reconciler] job %s %s", job.ID, msg)
}

// reconcileNotFound treats a missing PR as closed/cancelled (see ErrPRNotFound).
func (o *Orchestrator) reconcileNotFound(job *db.Job, prNumber int) {
	now := time.Now()
	job.State = db.StateCancelled
	job.BlockerReason = "PR not found (treated as closed without merge)"
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		msg := fmt.Sprintf("reconciler: failed to mark job cancelled after PR not found: %v", err)
		o.log(job.ID, "error", msg)
		log.Printf("[reconciler] job %s: %s", job.ID, msg)
		return
	}
	msg := fmt.Sprintf("PR %s not found (treated as closed) → cancelled", job.PRURL)
	if prNumber != 0 {
		msg = fmt.Sprintf("PR #%d not found (treated as closed) → cancelled", prNumber)
	}
	o.log(job.ID, "info", msg)
	log.Printf("[reconciler] job %s %s", job.ID, msg)
}

// getPRStatus returns the current GitHub PR status for prURL.
// It is injectable via Orchestrator.prStatusFunc for tests.
func (o *Orchestrator) getPRStatus(prURL string) (PRStatus, int, error) {
	if o.prStatusFunc != nil {
		return o.prStatusFunc(prURL)
	}
	return defaultGetPRStatus(prURL)
}

// ghPRView is the subset of `gh pr view --json` output we need.
type ghPRView struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	MergedAt *string `json:"mergedAt"`
	URL      string  `json:"url"`
}

// defaultGetPRStatus queries GitHub via `gh pr view`.
func defaultGetPRStatus(prURL string) (PRStatus, int, error) {
	cmd := exec.Command("gh", "pr", "view", prURL, "--json", "number,state,mergedAt,url")
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.ToLower(string(out) + " " + err.Error())
		if strings.Contains(output, "not found") ||
			strings.Contains(output, "no pull requests") ||
			strings.Contains(output, "no pull request") ||
			strings.Contains(output, "404") ||
			strings.Contains(output, "could not resolve") {
			return "", 0, fmt.Errorf("%w: %s", ErrPRNotFound, strings.TrimSpace(string(out)))
		}
		return "", 0, fmt.Errorf("gh pr view failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	var v ghPRView
	if err := json.Unmarshal(out, &v); err != nil {
		return "", 0, fmt.Errorf("failed to parse gh pr view output: %w", err)
	}

	// Merged takes precedence: gh may report state MERGED, or CLOSED + mergedAt.
	if v.MergedAt != nil && strings.TrimSpace(*v.MergedAt) != "" && strings.TrimSpace(*v.MergedAt) != "null" {
		return PRStatusMerged, v.Number, nil
	}
	switch strings.ToUpper(strings.TrimSpace(v.State)) {
	case "MERGED":
		return PRStatusMerged, v.Number, nil
	case "CLOSED":
		return PRStatusClosed, v.Number, nil
	case "OPEN":
		return PRStatusOpen, v.Number, nil
	default:
		return "", v.Number, fmt.Errorf("unknown PR state %q", v.State)
	}
}
