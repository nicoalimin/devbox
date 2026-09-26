package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/git"
	"github.com/nicoalimin/devbox/internal/linear"
	"github.com/nicoalimin/devbox/internal/opencode"
	"github.com/nicoalimin/devbox/internal/validation"
)

// Orchestrator manages job lifecycle
type Orchestrator struct {
	cfg             *config.Config
	db              *db.DB
	linear          issueClient
	opencode        *opencode.Client
	activeJobs      map[string]bool   // Track actively running jobs to prevent double-resume
	healingAttempts map[string]int    // Track session healing attempts per job (jobID -> count)
	streamStopFuncs map[string]func() // Stop functions for active event streams (jobID -> stopFunc)
	streamMu        sync.Mutex        // Mutex to protect streamStopFuncs map
	activeMu        sync.Mutex
	admissionMu     sync.Mutex // Serialize job/review admission before persisting busy state.
	maintenance     bool
	prStatusFn      func(prURL string) (prStatus, int, error) // Override for getPRStatus (tests)
	// validationFailures holds the latest deduped validation failure per job
	// until failJob records its signature (UTA-97).
	validationFailures map[string]*validation.Failure
	failureMu          sync.Mutex
	codexExecFn        codexExecFunc // Overridden by tests; defaults to the local `codex exec` CLI.
}

type issueClient interface {
	GetIssue(string) (*linear.Issue, error)
	AddComment(string, string) error
}

const (
	maxHealingAttempts        = 2 // Max number of session recreate attempts per phase
	maxReviewWait             = 15 * time.Minute
	commitRecoveryWait        = 10 * time.Minute
	maxOpenCodeRepairAttempts = 3
	maxCodexRepairAttempts    = 3
	maxRepairAttempts         = maxOpenCodeRepairAttempts + maxCodexRepairAttempts
)

const agentQualityInstructions = `
Quality and delivery requirements:
- Treat formatting, lint, typecheck, test, and build failures as work to fix, not blockers to merely report.
- Detect and use the repository's declared package manager and scripts.
- If a Prettier or other formatting check fails, run its write-mode formatter (for example, pnpm exec prettier --write .), including every file reported anywhere in the repository—not only files you originally edited.
- Rerun the exact failing command, then the repository's other relevant CI-equivalent checks. Continue fixing issues until they exit successfully.
- Do not switch branches, force-push, reset, discard files, or bypass hooks.
- Do not stop after describing commands, errors, or suggested fixes. Execute the implementation and leave all completed changes in the assigned worktree.
- Devbox owns final formatting, validation, commit, and push from its host delivery gate; do not wait for or depend on a model-side commit or push.
`

// errJobCancelled is returned when an in-flight pipeline observes that the
// job was cancelled (typically via CancelJob). Callers must exit without
// calling failJob so cancelled is not overwritten to failed.
var errJobCancelled = errors.New("job cancelled")

// checkCancelled re-reads the job from the DB and returns errJobCancelled when
// the current persisted state is cancelled.
func (o *Orchestrator) checkCancelled(jobID string) error {
	current, err := o.db.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("failed to re-read job for cancel check: %w", err)
	}
	if current != nil && current.State == db.StateCancelled {
		return errJobCancelled
	}
	return nil
}

// handlePipelineErr exits quietly on cancel; otherwise marks the job failed.
// Returns true when the caller should abort the pipeline.
func (o *Orchestrator) handlePipelineErr(job *db.Job, phase string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errJobCancelled) {
		o.takeValidationFailure(job.ID)
		o.log(job.ID, "info", fmt.Sprintf("Job cancelled; aborting %s", phase))
		return true
	}
	o.failJob(job, fmt.Sprintf("Failed to %s: %v", phase, err))
	return true
}

// NewOrchestrator creates a new job orchestrator
func NewOrchestrator(cfg *config.Config, database *db.DB) *Orchestrator {
	oc := opencode.NewClient(cfg.OpenCode.BaseURL, cfg.OpenCode.Username, cfg.OpenCode.Password, cfg.OpenCode.Version)

	// Set up OpenCode telemetry logging (logs to server logs, not job logs)
	oc.SetLogFunc(func(level, message string) {
		// Log to standard logger for server logs
		log.Printf("[OpenCode:%s] %s", level, message)
	})

	return &Orchestrator{
		cfg:             cfg,
		db:              database,
		linear:          linear.NewClient(cfg.Linear.APIKey),
		opencode:        oc,
		activeJobs:      make(map[string]bool),
		healingAttempts: make(map[string]int),
		streamStopFuncs: make(map[string]func()),
		codexExecFn:     runCodexExec,
	}
}

// markJobActive marks a job as actively running
func (o *Orchestrator) markJobActive(jobID string) {
	o.activeMu.Lock()
	defer o.activeMu.Unlock()
	o.activeJobs[jobID] = true
}

// markJobInactive marks a job as no longer running
func (o *Orchestrator) markJobInactive(jobID string) {
	o.activeMu.Lock()
	defer o.activeMu.Unlock()
	delete(o.activeJobs, jobID)
}

// isJobActive checks if a job is currently running
func (o *Orchestrator) isJobActive(jobID string) bool {
	o.activeMu.Lock()
	defer o.activeMu.Unlock()
	return o.activeJobs[jobID]
}

// CreateJob creates a new job for a Linear issue with optional operator context
func (o *Orchestrator) CreateJob(linearIssueID string, operatorContext string) (*db.Job, error) {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	if o.maintenance {
		return nil, fmt.Errorf("server is draining for upgrade; retry after restart")
	}
	// Check if server is busy
	currentJob, err := o.db.GetCurrentJob()
	if err != nil {
		return nil, fmt.Errorf("failed to check current job: %w", err)
	}
	if currentJob != nil {
		if o.cfg.Queue.Enabled {
			// TODO: Implement queueing
			return nil, fmt.Errorf("queueing not yet implemented")
		}
		return nil, fmt.Errorf("server busy with job %s (state: %s)", currentJob.ID, currentJob.State)
	}

	// Create job record
	job := &db.Job{
		ID:              uuid.New().String(),
		LinearIssueID:   linearIssueID,
		OperatorContext: operatorContext,
		State:           db.StateFetching,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	if err := o.db.CreateJob(job); err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	o.log(job.ID, "info", fmt.Sprintf("Created job for Linear issue %s", linearIssueID))
	if operatorContext != "" {
		o.log(job.ID, "info", "Operator context provided")
	}

	// Start processing asynchronously
	go o.ProcessJob(job.ID)

	return job, nil
}

// ProcessJob processes a job through its lifecycle
func (o *Orchestrator) ProcessJob(jobID string) {
	// Mark job as active
	o.markJobActive(jobID)
	defer o.markJobInactive(jobID)

	job, err := o.db.GetJob(jobID)
	if err != nil {
		o.log(jobID, "error", fmt.Sprintf("Failed to get job: %v", err))
		return
	}

	runPhase := func(phase string, fn func(*db.Job) error) bool {
		if err := o.checkCancelled(job.ID); err != nil {
			return o.handlePipelineErr(job, phase, err)
		}
		if err := fn(job); err != nil {
			return o.handlePipelineErr(job, phase, err)
		}
		return false
	}

	if runPhase("fetch Linear issue", o.fetchLinearIssue) {
		return
	}
	if runPhase("prepare worktree", o.prepareWorktree) {
		return
	}
	if runPhase("execute coding", o.executeCoding) {
		return
	}
	if runPhase("review code", o.reviewCode) {
		return
	}
	if runPhase("push branch", o.pushBranch) {
		return
	}
	if runPhase("create PR", o.createPullRequest) {
		return
	}

	if err := o.checkCancelled(job.ID); err != nil {
		if errors.Is(err, errJobCancelled) {
			o.log(job.ID, "info", "Job cancelled; skipping completion")
			return
		}
		o.log(job.ID, "error", fmt.Sprintf("Cancel check before complete failed: %v", err))
		return
	}

	// Mark complete
	o.completeJob(job)
}

// fetchLinearIssue fetches the Linear issue details
func (o *Orchestrator) fetchLinearIssue(job *db.Job) error {
	o.log(job.ID, "info", "Fetching Linear issue")

	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil {
		return err
	}

	// Update job with issue details
	job.LinearURL = issue.URL
	job.UpdatedAt = time.Now()
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", fmt.Sprintf("Fetched issue: %s - %s", issue.Identifier, issue.Title))
	return nil
}

// prepareWorktree creates a git worktree for the job.
// UTA-96: when continue/reuse applies, reattach or checkout the existing PR
// branch instead of minting a new numbered branch from main.
func (o *Orchestrator) prepareWorktree(job *db.Job) error {
	job.State = db.StatePreparing
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", "Preparing git worktree")

	// Fetch issue details to determine repo
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil {
		return err
	}

	// Find matching repository
	labels := make([]string, len(issue.Labels))
	for i, label := range issue.Labels {
		labels[i] = label.Name
	}

	projectName := ""
	if issue.Project != nil {
		projectName = issue.Project.Name
	}

	repo := o.cfg.FindRepo(issue.Team.Key, projectName, labels)
	if repo == nil {
		return fmt.Errorf("no repository configured for team=%s, project=%s, labels=%v",
			issue.Team.Key, projectName, labels)
	}

	gitMgr := git.NewManager(repo.Path, repo.BaseBranch)
	opts := o.continueOptions(job)
	prior, priorErr := o.db.GetPriorReusableJob(job.LinearIssueID, job.ID)
	if priorErr != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to look up prior job for reuse: %v", priorErr))
	}
	plan := ResolveReusePlan(opts, prior, o.priorPRStatus(job, repo.Path, prior))
	if plan.Reuse {
		return o.prepareReuseWorktree(job, gitMgr, issue.Identifier, repo.Path, plan)
	}

	// Fresh assign: new numbered branch from main
	worktree, err := gitMgr.CreateWorktree(issue.Identifier)
	if err != nil {
		return err
	}

	job.RepoPath = repo.Path
	job.BranchName = worktree.BranchName
	job.WorktreePath = worktree.Path
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", fmt.Sprintf("Created worktree at %s (branch: %s)", worktree.Path, worktree.BranchName))
	return nil
}

// priorPRStatus checks whether the prior job's PR is still open so a closed or
// merged PR is never auto-reused.
func (o *Orchestrator) priorPRStatus(job *db.Job, dir string, prior *db.Job) PRStatus {
	if prior == nil || prior.PRURL == "" {
		return PRStatusUnknown
	}
	state, err := git.PRState(dir, prior.PRURL)
	if err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Could not check prior PR %s: %v", prior.PRURL, err))
		return PRStatusUnknown
	}
	if state == "OPEN" {
		return PRStatusOpen
	}
	o.log(job.ID, "info", fmt.Sprintf("Prior PR %s is %s; starting a fresh branch from main unless continue was requested", prior.PRURL, state))
	return PRStatusClosed
}

// prepareReuseWorktree reattaches an existing worktree or creates one checked
// out on plan.Ref (the PR head). Assigned branch == plan.Ref so dual-push is a no-op.
func (o *Orchestrator) prepareReuseWorktree(job *db.Job, gitMgr *git.Manager, identifier, repoPath string, plan ReusePlan) error {
	o.log(job.ID, "info", fmt.Sprintf("Reuse path: iterating on existing branch %q (not a new branch from main)", plan.Ref))

	job.RepoPath = repoPath
	if plan.RepoPath != "" {
		job.RepoPath = plan.RepoPath
		gitMgr = git.NewManager(job.RepoPath, o.baseBranch(job))
	}
	job.BranchName = plan.Ref
	if job.PRURL == "" && plan.PRURL != "" {
		job.PRURL = plan.PRURL
	}
	// Ensure operator context carries push_ref so delivery + coding prompt
	// treat this as continue even when only auto-detected from a prior PR.
	job.OperatorContext = ensurePushRefInContext(job.OperatorContext, plan.Ref)

	var worktreePath string

	// 1) Prefer last job's worktree_path if still on disk
	if git.WorktreePathExists(plan.PreferWorktreePath) {
		if err := gitMgr.EnsureWorktreeOnBranch(plan.PreferWorktreePath, plan.Ref); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Could not reattach preferred worktree %s: %v", plan.PreferWorktreePath, err))
		} else {
			worktreePath = plan.PreferWorktreePath
			o.log(job.ID, "info", fmt.Sprintf("Reattached existing worktree at %s on %s", worktreePath, plan.Ref))
		}
	}

	// 2) Else any registered worktree already on that branch
	if worktreePath == "" {
		existing, err := gitMgr.FindWorktreeForBranch(plan.Ref)
		if err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("FindWorktreeForBranch: %v", err))
		} else if existing != "" {
			if err := gitMgr.EnsureWorktreeOnBranch(existing, plan.Ref); err != nil {
				return fmt.Errorf("found worktree for %q but could not ensure checkout: %w", plan.Ref, err)
			}
			worktreePath = existing
			o.log(job.ID, "info", fmt.Sprintf("Reusing registered worktree at %s on %s", worktreePath, plan.Ref))
		}
	}

	// 3) Else create worktree checked out on push_ref / prior branch
	if worktreePath == "" {
		worktree, err := gitMgr.CreateWorktreeOnRef(identifier, plan.Ref)
		if err != nil {
			return fmt.Errorf("failed to create reuse worktree on %q: %w", plan.Ref, err)
		}
		worktreePath = worktree.Path
		o.log(job.ID, "info", fmt.Sprintf("Created reuse worktree at %s (branch: %s)", worktree.Path, worktree.BranchName))
	}

	job.WorktreePath = worktreePath
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	// UTA-98: every reuse path must start from the remote tip. A local branch
	// left behind by an earlier job may be missing commits pushed from
	// elsewhere (operator fixes, another machine).
	result, err := gitMgr.SyncToRemote(worktreePath, plan.Ref)
	if err != nil {
		return fmt.Errorf("failed to sync reused worktree onto origin/%s before coding: %w", plan.Ref, err)
	}
	o.log(job.ID, "info", fmt.Sprintf("Synced reused worktree with origin/%s (%s)", plan.Ref, result))
	return nil
}

// executeCoding drives OpenCode to implement the issue
func (o *Orchestrator) executeCoding(job *db.Job) error {
	job.State = db.StateCoding
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", "Starting OpenCode coding session")

	// Fetch issue for prompt
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil {
		return err
	}

	// Create OpenCode session
	sessionName := fmt.Sprintf("%s: %s", issue.Identifier, issue.Title)
	session, err := o.opencode.CreateSession(sessionName, job.WorktreePath)
	if err != nil {
		return err
	}

	job.OpenCodeSessionID = session.ID
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	// Get job for operator context
	currentJob, err := o.db.GetJob(job.ID)
	if err != nil {
		return err
	}

	// Build prompt with context
	priorErrors := o.priorFailureSummary(currentJob)
	if priorErrors != "" {
		o.log(job.ID, "info", "Previous attempt failed validation; injecting its deduped errors as mandatory step 1")
	}
	prompt := o.buildCodingPrompt(issue, currentJob.OperatorContext, priorErrors)

	// Send task to OpenCode
	if err := o.opencode.SendMessage(session.ID, prompt, job.WorktreePath); err != nil {
		return err
	}

	o.log(job.ID, "info", "Sent task to OpenCode, waiting for completion")

	// Start event stream to capture OpenCode logs
	if err := o.startEventStream(job, session.ID); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to start event stream: %v", err))
		// Continue without streaming - not fatal
	}
	defer o.stopEventStream(job.ID)

	// Wait for OpenCode to complete the coding task with healing support
	logFunc := func(msg string) {
		o.log(job.ID, "info", msg)
	}

	sessionID, err := o.waitForSessionWithHealing(job, "coding", logFunc)
	if err != nil {
		o.log(job.ID, "error", fmt.Sprintf("OpenCode session did not complete: %v", err))
		return fmt.Errorf("OpenCode coding session timed out or failed: %w", err)
	}

	// Update job with final session ID (in case it was healed)
	if sessionID != job.OpenCodeSessionID {
		job.OpenCodeSessionID = sessionID
		if err := o.db.UpdateJob(job); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to update final session ID: %v", err))
		}
	}

	o.log(job.ID, "info", "OpenCode coding session completed")

	return nil
}

// reviewCode performs a code review pass
func (o *Orchestrator) reviewCode(job *db.Job) error {
	job.State = db.StateReviewing
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", "Starting code review")

	// Send review prompt to OpenCode
	reviewPrompt := `Please review the changes you just made:
1. Check for code quality issues
2. Verify tests are passing
3. Ensure documentation is updated
4. Confirm the implementation matches requirements
5. **TUI changes (internal/tui/)**: Verify the dashboard fits entirely in one terminal screen with no overflow or clipped header/footer. Left column height (jobs + errors + integrations) must equal right column height (server logs + job logs).

If you find issues, fix them now.` + agentQualityInstructions

	if err := o.opencode.SendMessage(job.OpenCodeSessionID, reviewPrompt, job.WorktreePath); err != nil {
		return err
	}

	o.log(job.ID, "info", "Sent review prompt, waiting for completion")

	// Start event stream to capture OpenCode logs
	if err := o.startEventStream(job, job.OpenCodeSessionID); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to start event stream: %v", err))
		// Continue without streaming - not fatal
	}
	defer o.stopEventStream(job.ID)

	// Wait for review to complete with healing support
	logFunc := func(msg string) {
		o.log(job.ID, "info", msg)
	}

	sessionID, err := o.waitForSessionWithHealing(job, "reviewing", logFunc)
	if err != nil {
		return o.recoverFromReviewWaitFailure(job, err)
	}

	// Update job with final session ID (in case it was healed)
	if sessionID != job.OpenCodeSessionID {
		job.OpenCodeSessionID = sessionID
		if err := o.db.UpdateJob(job); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to update final session ID: %v", err))
		}
	}

	o.log(job.ID, "info", "Code review completed")
	return nil
}

func (o *Orchestrator) recoverFromReviewWaitFailure(job *db.Job, waitErr error) error {
	o.log(job.ID, "warn", fmt.Sprintf("OpenCode review did not finish cleanly; interrupting it before delivery recovery: %v", waitErr))
	if interruptErr := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); interruptErr != nil {
		return fmt.Errorf("OpenCode review failed (%v) and could not be interrupted safely: %w", waitErr, interruptErr)
	}

	job.ReviewingWaitStartedAt = nil
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to clear review wait timestamp after interruption: %v", err))
	}
	o.log(job.ID, "warn", "OpenCode review was interrupted; continuing with local validation and delivery recovery")
	return nil
}

// continueOptions parses OperatorContext for continue-existing-PR delivery.
func (o *Orchestrator) continueOptions(job *db.Job) ContinuePROptions {
	if job == nil {
		return ContinuePROptions{}
	}
	return ParseContinuePRContext(job.OperatorContext)
}

// alignAssignedBranchIfContinue renames the worktree local branch to the job's
// assigned branch when push_ref continue context is set (keeps commits).
func (o *Orchestrator) alignAssignedBranchIfContinue(job *db.Job, gitMgr *git.Manager) error {
	opts := o.continueOptions(job)
	if !opts.Active() {
		return nil
	}
	o.log(job.ID, "info", fmt.Sprintf("Continue PR: aligning worktree onto assigned branch %q (push_ref=%q)", job.BranchName, opts.PushRef))
	if err := gitMgr.AlignAssignedBranch(job.WorktreePath, job.BranchName); err != nil {
		return err
	}
	return nil
}

// pushBranch pushes the branch to the remote
func (o *Orchestrator) pushBranch(job *db.Job) error {
	job.State = db.StatePushing
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", "Preparing validated commits before push")

	gitMgr := git.NewManager(job.RepoPath, o.baseBranch(job))
	if err := o.alignAssignedBranchIfContinue(job, gitMgr); err != nil {
		return err
	}
	if err := gitMgr.AssertBranch(job.WorktreePath, job.BranchName); err != nil {
		return err
	}

	if err := o.validateWithOpenCodeRepair(job, gitMgr); err != nil {
		return err
	}
	if err := gitMgr.AssertBranch(job.WorktreePath, job.BranchName); err != nil {
		return err
	}

	hasChanges, err := gitMgr.HasUncommittedChanges(job.WorktreePath)
	if err != nil {
		return fmt.Errorf("failed to inspect worktree after validation: %w", err)
	}
	if hasChanges {
		o.log(job.ID, "warn", "Validation commands left uncommitted changes; committing them with the current Git identity")
		message := fmt.Sprintf("[%s] Commit validation output", job.LinearIssueID)
		if err := gitMgr.CommitAll(job.WorktreePath, message); err != nil {
			return fmt.Errorf("failed to commit validation output using current Git configuration: %w", err)
		}
		o.log(job.ID, "info", "Committed changes produced by local validation")
	}

	// Check if there are commits ahead of base
	hasCommits, err := gitMgr.HasCommitsAheadOfBase(job.WorktreePath)
	if err != nil {
		return fmt.Errorf("failed to check for commits: %w", err)
	}

	if !hasCommits {
		return fmt.Errorf("no file changes or commits found on branch %s compared to base branch; there is nothing to push or open as a pull request", job.BranchName)
	}

	if err := o.enforceAllowlistPushGate(job, gitMgr); err != nil {
		return err
	}

	if o.alreadyPublished(job, gitMgr) {
		o.log(job.ID, "info", "Nothing new to push: remote branch already at HEAD; skipping push")
		return nil
	}

	o.log(job.ID, "info", "Commits verified, pushing branch to remote")

	if err := o.pushWithRetry(job, gitMgr); err != nil {
		return err
	}

	o.log(job.ID, "info", "Branch pushed successfully")
	return nil
}

// enforceAllowlistPushGate enforces an ALLOWLIST: block from OperatorContext
// and/or the Linear description before push (UTA-103). Out-of-scope paths
// (for example formatter churn or invented files) are restored to base and
// committed away; the job fails only when nothing in scope remains. Absent
// allowlist is a no-op (backward compatible).
func (o *Orchestrator) enforceAllowlistPushGate(job *db.Job, gitMgr *git.Manager) error {
	linearDesc := ""
	if issue, err := o.linear.GetIssue(job.LinearIssueID); err == nil && issue != nil {
		linearDesc = issue.Description
	}
	allow := ParseAllowlist(job.OperatorContext, linearDesc)
	if len(allow) == 0 {
		return nil
	}
	changed, err := gitMgr.ChangedFilesVsBase(job.WorktreePath)
	if err != nil {
		return fmt.Errorf("allowlist push gate: %w", err)
	}
	violations := AllowlistViolations(allow, changed)
	if len(violations) == 0 {
		o.log(job.ID, "info", fmt.Sprintf("Allowlist push gate OK (%d path(s) within %v)", len(changed), allow))
		return nil
	}
	o.log(job.ID, "warn", fmt.Sprintf("Allowlist push gate: restoring %d out-of-scope path(s) to base: %v", len(violations), violations))
	message := fmt.Sprintf("[%s] Revert changes outside ALLOWLIST", job.LinearIssueID)
	if err := gitMgr.RestorePathsToBase(job.WorktreePath, violations, message); err != nil {
		return fmt.Errorf("allowlist push gate: %w", err)
	}
	remaining, err := gitMgr.ChangedFilesVsBase(job.WorktreePath)
	if err != nil {
		return fmt.Errorf("allowlist push gate: %w", err)
	}
	if bad := AllowlistViolations(allow, remaining); len(bad) > 0 {
		msg := FormatAllowlistFailure(allow, bad)
		o.log(job.ID, "error", msg)
		return fmt.Errorf("%s", msg)
	}
	if len(remaining) == 0 {
		msg := fmt.Sprintf("allowlist push gate failed: no changes within ALLOWLIST %v (only out-of-scope edits %v were produced)", allow, violations)
		o.log(job.ID, "error", msg)
		return fmt.Errorf("%s", msg)
	}
	o.log(job.ID, "info", fmt.Sprintf("Allowlist push gate OK after restore (%d in-scope path(s))", len(remaining)))
	return nil
}

// validateWithOpenCodeRepair formats locally and retries the full gate after
// each bounded repair, including interrupted repairs that may have succeeded.
//
// UTA-97: formatter output is folded into the agent-work commit
// ("[ISSUE] <title>"), never a standalone "chore: format" commit. When the
// agent changed nothing and the only diff is the formatter's, nothing is
// committed and the formatter output is discarded after validation so no
// format-only change is committed or pushed (including by checkpoints).
func (o *Orchestrator) validateWithOpenCodeRepair(job *db.Job, gitMgr *git.Manager) error {
	commitMessage := ""
	runValidation := func() error {
		// A repair agent can run arbitrary Git commands. Reassert the assigned
		// branch before every host-owned format/commit pass.
		if err := o.alignAssignedBranchIfContinue(job, gitMgr); err != nil {
			return err
		}
		if err := gitMgr.AssertBranch(job.WorktreePath, job.BranchName); err != nil {
			return err
		}
		headTree, err := gitMgr.HeadTree(job.WorktreePath)
		if err != nil {
			return err
		}
		agentTree, err := gitMgr.WorktreeTree(job.WorktreePath)
		if err != nil {
			return fmt.Errorf("failed to snapshot agent worktree: %w", err)
		}

		o.log(job.ID, "info", "Running deterministic host formatting before validation")
		if err := validation.Format(job.WorktreePath, o.formatCommands(job), func(msg string) {
			o.log(job.ID, "info", msg)
		}); err != nil {
			return fmt.Errorf("host formatting failed: %w", err)
		}

		formattedTree, err := gitMgr.WorktreeTree(job.WorktreePath)
		if err != nil {
			return fmt.Errorf("failed to inspect formatted worktree: %w", err)
		}
		formatOnly := false
		switch {
		case formattedTree == headTree:
			// Nothing to commit.
		case agentTree == headTree && !o.hasUnpublishedCommits(job, gitMgr):
			formatOnly = true
			o.log(job.ID, "info", "Agent made no changes; host formatting diff is format-only and will not be committed or pushed")
		default:
			// Agent edits (or agent-made commits not yet published) plus the
			// formatter's output go into one meaningful commit.
			if commitMessage == "" {
				commitMessage = o.workCommitMessage(job)
			}
			o.log(job.ID, "info", fmt.Sprintf("Committing agent work with host formatting folded in: %q", commitMessage))
			if err := gitMgr.CommitAll(job.WorktreePath, commitMessage); err != nil {
				return fmt.Errorf("failed to commit host-formatted worktree: %w", err)
			}
		}

		o.log(job.ID, "info", "Running local CI-equivalent validation before push")
		validationErr := validation.Run(job.WorktreePath, o.validationCommands(job), func(msg string) {
			o.log(job.ID, "info", msg)
		})
		if formatOnly {
			if err := gitMgr.DiscardUncommittedChanges(job.WorktreePath); err != nil {
				return errors.Join(validationErr, fmt.Errorf("failed to discard format-only changes: %w", err))
			}
			o.log(job.ID, "info", "Discarded format-only host formatter output")
		}
		return validationErr
	}

	for attempt := 0; ; attempt++ {
		if err := o.checkCancelled(job.ID); err != nil {
			return err
		}
		validationErr := runValidation()
		o.recordValidationFailure(job.ID, validationErr)
		if validationErr == nil {
			return nil
		}
		if attempt == maxRepairAttempts {
			return fmt.Errorf("local validation failed after %d repair attempts: %w", attempt, validationErr)
		}
		repairAttempt := attempt + 1
		if repairAttempt <= maxOpenCodeRepairAttempts && job.OpenCodeSessionID == "" {
			return fmt.Errorf("local validation failed after %d repair attempts: %w", attempt, validationErr)
		}
		repairer := "OpenCode"
		if repairAttempt > maxOpenCodeRepairAttempts {
			repairer = "Codex"
		}
		o.log(job.ID, "warn", fmt.Sprintf("%s repair attempt %d/%d: %v", repairer, repairAttempt, maxRepairAttempts, validationErr))
		before, _ := gitMgr.WorktreeTree(job.WorktreePath)

		prompt := buildValidationRepairPrompt(validationErr)
		if repairAttempt <= maxOpenCodeRepairAttempts {
			// Keep the OpenCode event stream running so a silent repair timeout is
			// visible in the job log.
			o.stopEventStream(job.ID)
			if err := o.startEventStream(job, job.OpenCodeSessionID); err != nil {
				o.log(job.ID, "warn", fmt.Sprintf("Failed to start event stream for repair: %v", err))
			}
			err := o.opencode.SendMessage(job.OpenCodeSessionID, prompt, job.WorktreePath)
			if err == nil {
				wait := commitRecoveryWait
				if o.cfg.OpenCode.Timeout > 0 && o.cfg.OpenCode.Timeout < wait {
					wait = o.cfg.OpenCode.Timeout
				}
				err = o.opencode.WaitForSessionIdle(job.OpenCodeSessionID, wait, job.WorktreePath, func(msg string) { o.log(job.ID, "info", msg) })
			}
			o.stopEventStream(job.ID)
			if err != nil {
				o.log(job.ID, "warn", fmt.Sprintf("OpenCode repair did not finish cleanly; stopping session and rechecking actual files: %v", err))
				if stopErr := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); stopErr != nil {
					return fmt.Errorf("repair failed (%v); cannot safely take over worktree: %w", err, stopErr)
				}
			}
		} else {
			wait := commitRecoveryWait
			if o.cfg.OpenCode.Timeout > 0 && o.cfg.OpenCode.Timeout < wait {
				wait = o.cfg.OpenCode.Timeout
			}
			output, err := o.codexExecFn(job.WorktreePath, prompt, wait)
			if summary := summarizeCodexOutput(output); summary != "" {
				o.log(job.ID, "info", "Codex repair output:\n"+summary)
			}
			if err != nil {
				o.log(job.ID, "warn", fmt.Sprintf("Codex repair did not finish cleanly; rechecking actual files: %v", err))
			}
		}
		if after, afterErr := gitMgr.WorktreeTree(job.WorktreePath); before != "" && afterErr == nil && after == before {
			o.log(job.ID, "warn", fmt.Sprintf("%s repair attempt %d/%d made no changes to the worktree", repairer, repairAttempt, maxRepairAttempts))
		}
	}
}

// workCommitMessage is the subject for the agent-work commit, e.g.
// "[UTA-97] Validation-repair loop".
func (o *Orchestrator) workCommitMessage(job *db.Job) string {
	identifier, title := job.LinearIssueID, ""
	if issue, err := o.linear.GetIssue(job.LinearIssueID); err == nil && issue != nil {
		if issue.Identifier != "" {
			identifier = issue.Identifier
		}
		title = strings.TrimSpace(issue.Title)
	}
	if title == "" {
		title = "Automated implementation changes"
	}
	return fmt.Sprintf("[%s] %s", identifier, title)
}

// recordValidationFailure remembers the latest deduped validation failure so
// failJob can persist its signature; any other outcome clears it.
func (o *Orchestrator) recordValidationFailure(jobID string, err error) {
	o.failureMu.Lock()
	defer o.failureMu.Unlock()
	var failure *validation.Failure
	if err != nil && errors.As(err, &failure) {
		if o.validationFailures == nil {
			o.validationFailures = make(map[string]*validation.Failure)
		}
		o.validationFailures[jobID] = failure
		return
	}
	delete(o.validationFailures, jobID)
}

func (o *Orchestrator) takeValidationFailure(jobID string) *validation.Failure {
	o.failureMu.Lock()
	defer o.failureMu.Unlock()
	failure := o.validationFailures[jobID]
	delete(o.validationFailures, jobID)
	return failure
}

// alreadyPublished reports whether every ref delivery would push already
// points at HEAD on the remote, i.e. a push would publish nothing new.
func (o *Orchestrator) alreadyPublished(job *db.Job, manager *git.Manager) bool {
	refs := []string{job.BranchName}
	if opts := o.continueOptions(job); opts.Active() && opts.PushRef != job.BranchName {
		refs = append(refs, opts.PushRef)
	}
	for _, ref := range refs {
		ok, err := manager.RemoteBranchAtHead(job.WorktreePath, ref)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// hasUnpublishedCommits reports whether HEAD carries commits that delivery
// would still publish (ahead of base and not yet on the remote branch).
func (o *Orchestrator) hasUnpublishedCommits(job *db.Job, manager *git.Manager) bool {
	ahead, err := manager.HasCommitsAheadOfBase(job.WorktreePath)
	if err != nil || !ahead {
		return false
	}
	return !o.alreadyPublished(job, manager)
}

func buildValidationRepairPrompt(validationErr error) string {
	return fmt.Sprintf(`Devbox's local pre-push validation failed. Resolve the failure completely in the current worktree.

Validation errors (deduplicated; fix every distinct error listed, starting with the first):
---
%s
---

For formatting failures such as Prettier, run the formatter in write mode across every path reported (for example, pnpm exec prettier --write .), even when many files or files outside your original edit are listed. Then rerun the exact failing command and all relevant repository checks until they pass. Leave the resulting fixes in the worktree; Devbox will format, validate, commit, and push them from the host. Do not only report the failure or tell the operator what to run.`, validationErr) + agentQualityInstructions
}

func (o *Orchestrator) baseBranch(job *db.Job) string {
	for _, repo := range o.cfg.Repos {
		if repo.Repo.Path == job.RepoPath && repo.Repo.BaseBranch != "" {
			return repo.Repo.BaseBranch
		}
	}
	return o.cfg.GitHub.DefaultBaseBranch
}

func (o *Orchestrator) formatCommands(job *db.Job) []string {
	for _, repo := range o.cfg.Repos {
		if repo.Repo.Path == job.RepoPath {
			return repo.Repo.FormatCommands
		}
	}
	return nil
}

func (o *Orchestrator) pushWithRetry(job *db.Job, manager *git.Manager) error {
	opts := o.continueOptions(job)
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		// UTA-98: integrate the remote tip before every push attempt so a
		// commit that landed mid-job is preserved instead of rejected.
		if syncErr := o.syncBeforePush(job, manager, opts); syncErr != nil {
			return syncErr
		}
		if opts.Active() {
			err = manager.PushBranchAlso(job.WorktreePath, job.BranchName, opts.PushRef)
		} else {
			err = manager.PushBranch(job.WorktreePath, job.BranchName)
		}
		if err == nil {
			if opts.Active() && opts.PushRef != job.BranchName {
				o.log(job.ID, "info", fmt.Sprintf("Continue PR: dual-pushed HEAD to push_ref %q", opts.PushRef))
			}
			return nil
		}
		if git.IsNonFastForward(err) {
			o.log(job.ID, "warn", fmt.Sprintf("Push attempt %d/3 rejected as non-fast-forward; fetching and rebasing before retry", attempt))
			continue
		}
		o.log(job.ID, "warn", fmt.Sprintf("Push attempt %d/3 failed: %v", attempt, err))
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return err
}

// syncBeforePush rebases local work onto origin/<branch> (and origin/<push_ref>
// for continues) so the push is a fast-forward. Never force-pushes.
func (o *Orchestrator) syncBeforePush(job *db.Job, manager *git.Manager, opts ContinuePROptions) error {
	refs := []string{job.BranchName}
	if opts.Active() && opts.PushRef != "" && opts.PushRef != job.BranchName {
		refs = append(refs, opts.PushRef)
	}
	for _, ref := range refs {
		result, err := manager.SyncToRemote(job.WorktreePath, ref)
		if err != nil {
			return fmt.Errorf("failed to integrate origin/%s before push: %w", ref, err)
		}
		if result == git.SyncRebased || result == git.SyncFastForwarded {
			o.log(job.ID, "info", fmt.Sprintf("Integrated origin/%s before push (%s)", ref, result))
		}
	}
	return nil
}

// validationCommands returns the per-repository override. A nil slice enables
// automatic detection; an explicitly empty slice disables local validation.
func (o *Orchestrator) validationCommands(job *db.Job) []string {
	jobRepo := filepath.Clean(job.RepoPath)
	for _, candidate := range o.cfg.Repos {
		if filepath.Clean(candidate.Repo.Path) == jobRepo {
			return candidate.Repo.ValidationCommands
		}
	}
	return nil
}

// createPullRequest creates a PR using gh CLI
func (o *Orchestrator) createPullRequest(job *db.Job) error {
	o.log(job.ID, "info", "Creating pull request")

	// Fetch issue for PR details
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil {
		return err
	}

	prTitle := fmt.Sprintf("[%s] %s", issue.Identifier, issue.Title)
	prBody := fmt.Sprintf(`## Linear Issue
%s

## Description
%s

---
*Automated PR created by devboxd*`, issue.URL, issue.Description)

	// Determine base branch
	baseBranch := o.baseBranch(job)

	var prURL string
	// Prefer PR already inherited from a prior continue/reuse job, but only
	// while it is still open. Never deliver onto a closed/merged PR.
	if job.PRURL != "" {
		state, stateErr := git.PRState(job.WorktreePath, job.PRURL)
		switch {
		case stateErr != nil:
			o.log(job.ID, "warn", fmt.Sprintf("Reuse: could not verify PR %s is open (%v); looking up open PR by head instead", job.PRURL, stateErr))
		case state == "OPEN":
			prURL = job.PRURL
			o.log(job.ID, "info", fmt.Sprintf("Reuse: keeping existing PR %s", prURL))
		default:
			o.log(job.ID, "warn", fmt.Sprintf("Reuse: inherited PR %s is %s; not reusing it", job.PRURL, state))
		}
	}
	if prURL == "" {
		head := job.BranchName
		if opts := o.continueOptions(job); opts.Active() {
			head = opts.PushRef
		}
		if head != "" {
			existing, findErr := git.FindPRURLByHead(job.WorktreePath, head)
			if findErr != nil {
				o.log(job.ID, "warn", fmt.Sprintf("Continue/reuse: could not look up existing PR for head %q: %v", head, findErr))
			} else if existing != "" {
				o.log(job.ID, "info", fmt.Sprintf("Continue/reuse: reusing existing PR for head %q: %s", head, existing))
				prURL = existing
			}
		}
	}
	if prURL == "" {
		var createErr error
		prURL, createErr = git.CreatePR(job.WorktreePath, prTitle, prBody, baseBranch)
		if createErr != nil {
			return createErr
		}
	}

	job.PRURL = prURL
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", fmt.Sprintf("Pull request ready: %s", prURL))

	// Add comment to Linear
	comment := fmt.Sprintf("🚀 Pull Request Ready\n\nPR: %s\n\n*Automated by devboxd*", prURL)
	if err := o.linear.AddComment(issue.ID, comment); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to add Linear comment: %v", err))
		// Don't fail the job if commenting fails
	}

	return nil
}

// completeJob marks a job as complete
func (o *Orchestrator) completeJob(job *db.Job) {
	manager := git.NewManager(job.RepoPath, o.baseBranch(job))
	if err := manager.VerifyPublished(job.WorktreePath, job.BranchName); err != nil {
		o.failJob(job, fmt.Sprintf("Delivery verification failed: %v", err))
		return
	}
	now := time.Now()
	job.State = db.StateDone
	job.FailureSignature, job.FailureSummary = "", ""
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to mark job complete: %v", err))
		return
	}

	o.log(job.ID, "info", "Job completed successfully")

	// Preserve worktree when a PR is still open so assign --continue / review
	// can reattach and iterate in place (UTA-96). Reconciler cleans up after
	// the PR is merged or closed.
	if job.WorktreePath != "" {
		if job.PRURL != "" || o.shouldPreserveWorktree(job) {
			o.log(job.ID, "info", fmt.Sprintf("Preserving worktree at %s for PR iteration", job.WorktreePath))
		} else {
			gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
			if err := gitMgr.RemoveWorktree(job.WorktreePath); err != nil {
				o.log(job.ID, "warn", fmt.Sprintf("Failed to remove worktree: %v", err))
			}
		}
	}
}

// ensurePushRefInContext adds push_ref / continue_pr markers when missing so
// reuse jobs share the continue delivery path.
func ensurePushRefInContext(operatorContext, ref string) string {
	if ref == "" {
		return operatorContext
	}
	opts := ParseContinuePRContext(operatorContext)
	var b strings.Builder
	if !opts.ContinuePR {
		b.WriteString("continue_pr: true\n")
	}
	if opts.PushRef == "" {
		b.WriteString(fmt.Sprintf("push_ref: %s\n", ref))
	}
	if b.Len() == 0 {
		return operatorContext
	}
	prefix := strings.TrimSpace(b.String())
	if strings.TrimSpace(operatorContext) == "" {
		return prefix
	}
	return prefix + "\n\n" + operatorContext
}

// shouldPreserveWorktree reports whether the worktree should stay on disk after
// a terminal state so a successor continue/review can reattach.
func (o *Orchestrator) shouldPreserveWorktree(job *db.Job) bool {
	if job == nil {
		return false
	}
	if job.PRURL != "" {
		return true
	}
	opts := o.continueOptions(job)
	return opts.Active() || opts.ContinuePR
}

// failJob marks a job as failed
func (o *Orchestrator) failJob(job *db.Job, reason string) {
	// Re-read current state so a concurrent CancelJob (or successful complete)
	// is never overwritten to failed, and so we do not checkpoint after cancel.
	current, err := o.db.GetJob(job.ID)
	if err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to re-read job before fail: %v", err))
		return
	}
	if current != nil && (current.State == db.StateCancelled || current.State == db.StateDone) {
		o.log(job.ID, "info", fmt.Sprintf("Skipping failJob; job already terminal (%s): %s", current.State, reason))
		return
	}

	failure := o.takeValidationFailure(job.ID)

	// Preserve output on the remote even when quality checks or the model fail.
	// This is explicitly an incomplete checkpoint, never successful delivery.
	if job.WorktreePath != "" {
		published, err := o.checkpointFailedWork(job)
		var fb *fallbackCheckpointError
		if errors.As(err, &fb) {
			reason += fmt.Sprintf("; push to %s failed (%v); incomplete work preserved on fallback ref %s", job.BranchName, fb.cause, fb.ref)
		} else if err != nil {
			reason += fmt.Sprintf("; recovery incomplete (worktree preserved at %s): %v", job.WorktreePath, err)
		} else if published {
			reason += "; incomplete work committed and pushed to " + job.BranchName
		}
	}

	// Re-check after checkpoint: CancelJob may have won the race while we were
	// pushing recovery commits.
	current, err = o.db.GetJob(job.ID)
	if err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to re-read job after checkpoint: %v", err))
		return
	}
	if current != nil && (current.State == db.StateCancelled || current.State == db.StateDone) {
		o.log(job.ID, "info", fmt.Sprintf("Skipping fail overwrite; job already terminal (%s)", current.State))
		return
	}

	now := time.Now()
	job.State = db.StateFailed
	previousSignature := o.previousFailureSignature(job)
	job.FailureSignature, job.FailureSummary = "", ""
	if failure != nil {
		job.FailureSignature = failure.Signature
		job.FailureSummary = formatFailureSummary(failure)
		if previousSignature == failure.Signature {
			job.State = db.StateStuck
			reason = fmt.Sprintf("Stuck: same validation failure (signature %s) as the previous attempt on this issue; rerunning without new guidance will not help. %s", failure.Signature, reason)
		}
	}
	job.BlockerReason = reason
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to mark job %s: %v", job.State, err))
		return
	}

	if job.State == db.StateStuck {
		o.log(job.ID, "error", fmt.Sprintf("Job stuck: %s", reason))
		o.commentStuck(job)
		return
	}
	o.log(job.ID, "error", fmt.Sprintf("Job failed: %s", reason))
}

// previousFailureSignature returns the validation failure signature of the
// previous attempt on the same Linear issue: this job's own last failure (a
// review re-run of the same job) or, otherwise, the most recent other job.
func (o *Orchestrator) previousFailureSignature(job *db.Job) string {
	if job.FailureSignature != "" {
		return job.FailureSignature
	}
	prev, err := o.db.GetPreviousJob(job.LinearIssueID, job.ID)
	if err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to look up previous job for repeat-failure detection: %v", err))
		return ""
	}
	if prev == nil || !prev.State.IsFailure() {
		return ""
	}
	return prev.FailureSignature
}

// priorFailureSummary returns the deduped validation errors from the previous
// failed attempt that a continue should fix first (UTA-97), or "".
func (o *Orchestrator) priorFailureSummary(job *db.Job) string {
	opts := o.continueOptions(job)
	if !opts.Active() && !opts.ContinuePR {
		return ""
	}
	prev, err := o.db.GetPreviousJob(job.LinearIssueID, job.ID)
	if err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to look up previous job errors: %v", err))
		return ""
	}
	if prev == nil || !prev.State.IsFailure() {
		return ""
	}
	return prev.FailureSummary
}

// mandatoryFixStep renders prior validation errors as the first step of a
// continue/review prompt.
func mandatoryFixStep(summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return fmt.Sprintf(`Step 1 (mandatory): fix these errors from the previous attempt before doing anything else. The previous devbox job failed local validation with exactly these (deduplicated) errors; fix every one of them and rerun format + typecheck + unit tests until they pass:
---
%s
---

`, strings.TrimSpace(summary))
}

const maxFailureSummaryBytes = 16 * 1024

func formatFailureSummary(failure *validation.Failure) string {
	summary := fmt.Sprintf("%s: %v\n%s", failure.Command, failure.Err, failure.Summary())
	if len(summary) > maxFailureSummaryBytes {
		summary = summary[:maxFailureSummaryBytes] + "\n... summary truncated ..."
	}
	return summary
}

// commentStuck posts the deduped repeat failure to the Linear issue.
func (o *Orchestrator) commentStuck(job *db.Job) {
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil || issue == nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to fetch Linear issue for stuck comment: %v", err))
		return
	}
	summary := job.FailureSummary
	if len(summary) > 6000 {
		summary = summary[:6000] + "\n... truncated ..."
	}
	comment := fmt.Sprintf("⚠️ Devbox job stuck\n\nJob %s failed local validation with the same errors as the previous attempt (signature `%s`). Another `devbox assign --continue` will likely repeat this failure without new guidance.\n\n```\n%s\n```\n\n*Automated by devboxd*", job.ID, job.FailureSignature, summary)
	if err := o.linear.AddComment(issue.ID, comment); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to add Linear stuck comment: %v", err))
	}
}

// checkpointFailedWork stops the agent, commits any local changes, and pushes
// only when the branch has real commits ahead of the base. Returns published=true
// only when a real commit/diff was pushed. An identical branch is not recovery.
func (o *Orchestrator) checkpointFailedWork(job *db.Job) (published bool, err error) {
	if err := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); err != nil {
		return false, fmt.Errorf("cannot stop agent safely: %w", err)
	}
	manager := git.NewManager(job.RepoPath, o.baseBranch(job))
	if err := o.alignAssignedBranchIfContinue(job, manager); err != nil {
		return false, err
	}
	if err := manager.AssertBranch(job.WorktreePath, job.BranchName); err != nil {
		return false, err
	}

	hasChanges, err := manager.HasUncommittedChanges(job.WorktreePath)
	if err != nil {
		return false, err
	}
	if hasChanges {
		if err := manager.CommitAll(job.WorktreePath, fmt.Sprintf("[%s] Checkpoint incomplete automated work", job.LinearIssueID)); err != nil {
			return false, err
		}
	}

	// UTA-104: failure checkpoints must honor the same ALLOWLIST gate as the
	// success push path. Otherwise out-of-scope junk (wrong paths, meta format)
	// is published to the assigned branch when validation fails first.
	if err := o.enforceAllowlistPushGate(job, manager); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Skipping failure checkpoint publish: %v", err))
		o.maybeRemoveWorktreeAfterTerminal(job, manager, "allowlist blocked checkpoint")
		return false, nil
	}

	hasCommits, err := manager.HasCommitsAheadOfBase(job.WorktreePath)
	if err != nil {
		return false, err
	}
	if !hasCommits {
		o.log(job.ID, "info", "No incomplete work to checkpoint (branch identical to base)")
		o.maybeRemoveWorktreeAfterTerminal(job, manager, "empty checkpoint")
		return false, nil
	}

	if o.alreadyPublished(job, manager) {
		o.log(job.ID, "info", "No new work to checkpoint (remote branch already at HEAD); skipping push")
		o.maybeRemoveWorktreeAfterTerminal(job, manager, "empty checkpoint")
		return false, nil
	}

	if err := o.pushWithRetry(job, manager); err != nil {
		// UTA-98: never leave work local-only. Publish HEAD to a fallback ref.
		fallback, fbErr := o.publishFallbackCheckpoint(job, manager, err)
		if fbErr != nil {
			return false, fmt.Errorf("%v; fallback checkpoint also failed: %v", err, fbErr)
		}
		return false, &fallbackCheckpointError{ref: fallback, cause: err}
	}
	o.log(job.ID, "warn", "Incomplete work checkpoint committed and verified on remote; job remains failed")
	o.maybeRemoveWorktreeAfterTerminal(job, manager, "checkpoint published")
	return true, nil
}

func (o *Orchestrator) maybeRemoveWorktreeAfterTerminal(job *db.Job, manager *git.Manager, reason string) {
	if o.shouldPreserveWorktree(job) {
		o.log(job.ID, "info", fmt.Sprintf("Preserving worktree at %s after %s (PR/continue reuse)", job.WorktreePath, reason))
		return
	}
	if err := manager.RemoveWorktree(job.WorktreePath); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Worktree cleanup deferred after %s: %v", reason, err))
	}
}

// buildCodingPrompt builds the prompt for OpenCode with optional operator
// context. priorErrors (deduped validation errors from the previous failed
// attempt on a continue) become mandatory step 1.
func (o *Orchestrator) buildCodingPrompt(issue *linear.Issue, operatorContext, priorErrors string) string {
	var sb strings.Builder

	sb.WriteString(mandatoryFixStep(priorErrors))
	sb.WriteString(fmt.Sprintf("Build the following Linear issue:\n\n"))
	sb.WriteString(fmt.Sprintf("Identifier: %s\n", issue.Identifier))
	sb.WriteString(fmt.Sprintf("Title: %s\n", issue.Title))
	sb.WriteString(fmt.Sprintf("URL: %s\n", issue.URL))
	sb.WriteString(fmt.Sprintf("Priority: %d\n", issue.Priority))
	sb.WriteString(fmt.Sprintf("Team: %s (%s)\n", issue.Team.Name, issue.Team.Key))
	sb.WriteString(fmt.Sprintf("State: %s\n", issue.State.Name))

	if issue.Project != nil {
		sb.WriteString(fmt.Sprintf("Project: %s\n", issue.Project.Name))
	}

	if len(issue.Labels) > 0 {
		labels := make([]string, len(issue.Labels))
		for i, label := range issue.Labels {
			labels[i] = label.Name
		}
		sb.WriteString(fmt.Sprintf("Labels: %s\n", strings.Join(labels, ", ")))
	}

	sb.WriteString(fmt.Sprintf("\nDescription:\n%s\n\n", issue.Description))

	if operatorContext != "" {
		sb.WriteString("## Additional Operator Context\n\n")
		sb.WriteString(operatorContext)
		sb.WriteString("\n\n")
		if opts := ParseContinuePRContext(operatorContext); opts.Active() || opts.ContinuePR {
			sb.WriteString("## Continue existing PR (iterate in place)\n\n")
			sb.WriteString("This worktree is (or will be) checked out on the existing PR head branch. Do NOT create a new branch from main. Leave commits on the current branch so the existing PR updates.\n\n")
			if opts.PushRef != "" {
				sb.WriteString(fmt.Sprintf("Target PR head / push_ref: %q. Prefer staying on that branch; host delivery uses the same ref (dual-push is a no-op when assigned == push_ref).\n\n", opts.PushRef))
			}
		}
	}

	sb.WriteString("Instructions:\n")
	steps := []string{
		"Review the .opencode instructions in this repository",
		"Search Notion for related PRDs, specs, or context using the issue title and team",
		"Plan first: write an acceptance-criteria checklist (numbered) from the issue's acceptance criteria, or derive one from the description if none are listed, before editing code",
		"Implement in small steps, one checklist item at a time. After each step, run the repository's formatter, typecheck, and unit tests, and fix every failure before starting the next step",
		"Self-review before finishing: re-read your full diff against every acceptance-criteria checklist item, mark each item done or not done, and keep working until all are done and format + typecheck + unit tests pass",
		"Run the repository's CI-equivalent checks and resolve every failure (the host pre-push validation gate reruns them before delivery)",
		"Leave all completed changes in the assigned worktree for the host delivery gate",
	}
	first := 1
	if strings.TrimSpace(priorErrors) != "" {
		sb.WriteString("1. Complete Step 1 (mandatory) above: fix the previous attempt's errors first\n")
		first = 2
	}
	for i, step := range steps {
		sb.WriteString(fmt.Sprintf("%d. %s\n", first+i, step))
	}
	sb.WriteString(agentQualityInstructions)

	return sb.String()
}

// log adds a log entry
func (o *Orchestrator) log(jobID, level, message string) {
	// Add to database
	if err := o.db.AddLog(jobID, level, message); err != nil {
		// Log to stderr if DB logging fails (will be captured by TUI if active)
		fmt.Printf("[%s] %s: %s (failed to write to DB: %v)\n", jobID, level, message, err)
	}

	// Also add to TUI log buffer if it exists (for live display)
	// Format: [jobID] message
	formattedMsg := fmt.Sprintf("[%s] %s", jobID, message)
	log.Printf("[%s] %s", level, formattedMsg)
}

// ResumeInFlightJobs resumes all in-flight jobs after devboxd restart
func (o *Orchestrator) ResumeInFlightJobs() error {
	// Query DB for all jobs in busy/in-flight states
	jobs, err := o.db.ListJobs(0) // Get all jobs
	if err != nil {
		return fmt.Errorf("failed to list jobs: %w", err)
	}

	inFlightCount := 0
	for _, job := range jobs {
		// Skip terminal states and jobs already running
		if job.State.IsTerminal() {
			continue
		}
		if !job.State.IsBusy() {
			// Also skip non-busy states like queued or blocked
			continue
		}

		// Check if already running (shouldn't happen but defensive)
		if o.isJobActive(job.ID) {
			log.Printf("[orchestrator] Job %s already active, skipping resume", job.ID)
			continue
		}

		inFlightCount++
		log.Printf("[orchestrator] Resuming job %s in state %s", job.ID, job.State)
		o.log(job.ID, "info", fmt.Sprintf("Resuming job from state %s after devboxd restart", job.State))

		// Resume job asynchronously based on its state
		go o.resumeJobFromState(job)
	}

	if inFlightCount > 0 {
		log.Printf("[orchestrator] Resumed %d in-flight job(s)", inFlightCount)
	} else {
		log.Printf("[orchestrator] No in-flight jobs to resume")
	}

	return nil
}

// resumeJobFromState resumes a job from its current state
func (o *Orchestrator) resumeJobFromState(job *db.Job) {
	// Mark job as active
	o.markJobActive(job.ID)
	defer o.markJobInactive(job.ID)

	// Validate that we have the necessary information to resume
	if job.OpenCodeSessionID == "" && (job.State == db.StateCoding || (job.State == db.StateReviewing && job.ReviewFeedback == "")) {
		o.failJob(job, fmt.Sprintf("Cannot resume %s: missing OpenCode session ID", job.State))
		return
	}

	if job.WorktreePath == "" && job.State != db.StateFetching && job.State != db.StatePreparing {
		o.failJob(job, fmt.Sprintf("Cannot resume %s: missing worktree path", job.State))
		return
	}

	// Resume from the appropriate state. Coding must still send a review
	// prompt, and preparation must start a new coding session.
	runPhase := func(phase string, fn func(*db.Job) error) bool {
		if err := o.checkCancelled(job.ID); err != nil {
			return o.handlePipelineErr(job, phase, err)
		}
		if err := fn(job); err != nil {
			return o.handlePipelineErr(job, phase, err)
		}
		return false
	}

	switch job.State {
	case db.StateFetching:
		// Resume from the beginning
		if runPhase("fetch Linear issue", o.fetchLinearIssue) {
			return
		}
		fallthrough

	case db.StatePreparing:
		// Continue with worktree preparation
		if runPhase("prepare worktree", o.prepareWorktree) {
			return
		}
		if runPhase("execute coding", o.executeCoding) {
			return
		}
		if runPhase("review code", o.reviewCode) {
			return
		}

	case db.StateCoding:
		// Resume waiting for OpenCode coding session
		if runPhase("resume coding", o.resumeCoding) {
			return
		}
		if runPhase("review code", o.reviewCode) {
			return
		}

	case db.StateReviewing:
		// An accepted review is saved before the HTTP response. If the server
		// stopped before sending it, start it now using the persisted feedback.
		if job.ReviewFeedback != "" && job.ReviewingWaitStartedAt == nil {
			_ = o.runReview(job) // runReview persists its own failure.
			return
		}
		// Resume waiting for OpenCode review session
		if runPhase("resume reviewing", o.resumeReviewing) {
			return
		}

	case db.StatePushing:
		// A repair may still be running in OpenCode after devboxd restarts.
		if err := o.checkCancelled(job.ID); err != nil {
			_ = o.handlePipelineErr(job, "resume pushing", err)
			return
		}
		if err := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); err != nil {
			o.failJob(job, fmt.Sprintf("Cannot safely resume delivery: %v", err))
			return
		}

	default:
		o.failJob(job, fmt.Sprintf("Cannot resume from unknown state: %s", job.State))
		return
	}
	if runPhase("push branch", o.pushBranch) {
		return
	}
	if job.PRURL != "" {
		if err := o.checkCancelled(job.ID); err != nil {
			_ = o.handlePipelineErr(job, "finish review", err)
			return
		}
		job.State = db.StatePROpen
		job.BlockerReason = ""
		job.FailureSignature, job.FailureSummary = "", ""
		job.CompletedAt = nil
		if err := o.db.UpdateJob(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to finish review: %v", err))
		}
		return
	}
	if runPhase("create PR", o.createPullRequest) {
		return
	}
	if err := o.checkCancelled(job.ID); err != nil {
		_ = o.handlePipelineErr(job, "complete", err)
		return
	}
	o.completeJob(job)
}

// resumeCoding resumes waiting for an existing OpenCode coding session
func (o *Orchestrator) resumeCoding(job *db.Job) error {
	o.log(job.ID, "info", "Resuming OpenCode coding session wait")

	// Start event stream to capture OpenCode logs
	if err := o.startEventStream(job, job.OpenCodeSessionID); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to start event stream: %v", err))
		// Continue without streaming - not fatal
	}
	defer o.stopEventStream(job.ID)

	// Wait for OpenCode to complete the coding task with healing support
	logFunc := func(msg string) {
		o.log(job.ID, "info", msg)
	}

	sessionID, err := o.waitForSessionWithHealing(job, "coding", logFunc)
	if err != nil {
		o.log(job.ID, "error", fmt.Sprintf("OpenCode session did not complete: %v", err))
		return fmt.Errorf("OpenCode coding session timed out or failed: %w", err)
	}

	// Update job with final session ID (in case it was healed)
	if sessionID != job.OpenCodeSessionID {
		job.OpenCodeSessionID = sessionID
		if err := o.db.UpdateJob(job); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to update final session ID: %v", err))
		}
	}

	o.log(job.ID, "info", "OpenCode coding session completed")
	return nil
}

// resumeReviewing resumes waiting for an existing OpenCode review session
func (o *Orchestrator) resumeReviewing(job *db.Job) error {
	o.log(job.ID, "info", "Resuming OpenCode review session wait")

	// Start event stream to capture OpenCode logs
	if err := o.startEventStream(job, job.OpenCodeSessionID); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to start event stream: %v", err))
		// Continue without streaming - not fatal
	}
	defer o.stopEventStream(job.ID)

	// Wait for review to complete with healing support
	logFunc := func(msg string) {
		o.log(job.ID, "info", msg)
	}

	sessionID, err := o.waitForSessionWithHealing(job, "reviewing", logFunc)
	if err != nil {
		return o.recoverFromReviewWaitFailure(job, err)
	}

	// Update job with final session ID (in case it was healed)
	if sessionID != job.OpenCodeSessionID {
		job.OpenCodeSessionID = sessionID
		if err := o.db.UpdateJob(job); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to update final session ID: %v", err))
		}
	}

	o.log(job.ID, "info", "Code review completed")
	return nil
}

// CancelJob cancels a running job
func (o *Orchestrator) CancelJob(jobID string) error {
	job, err := o.db.GetJob(jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job not found")
	}

	if job.State.IsTerminal() {
		return fmt.Errorf("job is already in terminal state: %s", job.State)
	}

	// Persist cancelled first so any in-flight ProcessJob observes it before
	// treating a StopSession-induced idle wait as successful coding/review.
	now := time.Now()
	job.State = db.StateCancelled
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(jobID, "info", "Job cancelled")

	// Stop the active OpenCode session BEFORE worktree cleanup so the agent
	// cannot keep writing after cancel.
	if job.OpenCodeSessionID != "" {
		if err := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); err != nil {
			o.log(jobID, "warn", fmt.Sprintf("Failed to stop OpenCode session on cancel: %v", err))
		} else {
			o.log(jobID, "info", "Stopped OpenCode session on cancel")
		}
	}

	// Clean up worktree
	if job.WorktreePath != "" {
		gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
		if err := gitMgr.RemoveWorktree(job.WorktreePath); err != nil {
			o.log(jobID, "warn", fmt.Sprintf("Failed to remove worktree: %v", err))
		}
	}

	return nil
}

// ReplyToJob sends a reply to a blocked job
func (o *Orchestrator) ReplyToJob(jobID, message string) error {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	if o.maintenance {
		return fmt.Errorf("server is draining for upgrade; retry after restart")
	}
	job, err := o.db.GetJob(jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job not found")
	}

	if job.State != db.StateBlocked {
		return fmt.Errorf("job is not in blocked state (current: %s)", job.State)
	}

	o.log(jobID, "info", fmt.Sprintf("Received reply: %s", message))

	// Send reply to OpenCode
	if job.OpenCodeSessionID != "" {
		if err := o.opencode.SendMessage(job.OpenCodeSessionID, message, job.WorktreePath); err != nil {
			return fmt.Errorf("failed to send message to OpenCode: %w", err)
		}
	}

	// Resume job (transition back to coding state)
	job.State = db.StateCoding
	job.BlockerReason = ""
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(jobID, "info", "Job resumed")
	return nil
}

// ReviewJob sends review feedback to an existing job and updates the PR
func (o *Orchestrator) ReviewJob(jobIDOrLinearID, feedback string) error {
	job, err := o.acceptReview(jobIDOrLinearID, feedback)
	if err != nil {
		return err
	}
	return o.runReview(job)
}

// StartReview persists acceptance synchronously so errors are returned before
// HTTP 202, and a restart can resume even before the worker sends its prompt.
func (o *Orchestrator) StartReview(jobIDOrLinearID, feedback string) (*db.Job, error) {
	job, err := o.acceptReview(jobIDOrLinearID, feedback)
	if err != nil {
		return nil, err
	}
	accepted := *job // The response must not race with the worker's updates.
	go func() { _ = o.runReview(job) }()
	return &accepted, nil
}

func (o *Orchestrator) acceptReview(jobIDOrLinearID, feedback string) (*db.Job, error) {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	if o.maintenance {
		return nil, fmt.Errorf("server is draining for upgrade; retry after restart")
	}
	if strings.TrimSpace(feedback) == "" {
		return nil, fmt.Errorf("feedback is required")
	}
	// Try to find job by ID first, then by Linear issue ID
	job, err := o.db.GetJob(jobIDOrLinearID)
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}
	if job == nil {
		// Try finding by Linear issue ID
		job, err = o.db.GetJobByLinearIssueID(jobIDOrLinearID)
		if err != nil {
			return nil, fmt.Errorf("failed to get job by Linear ID: %w", err)
		}
		if job == nil {
			return nil, fmt.Errorf("job not found: %s", jobIDOrLinearID)
		}
	}

	// Check that job has a PR
	if job.PRURL == "" {
		return nil, fmt.Errorf("job does not have a pull request yet (state: %s)", job.State)
	}

	// Check that we have necessary information
	if job.BranchName == "" {
		return nil, fmt.Errorf("job has no branch name")
	}
	if job.RepoPath == "" {
		return nil, fmt.Errorf("job has no repo path")
	}
	current, err := o.db.GetCurrentJob()
	if err != nil {
		return nil, err
	}
	if current != nil || o.isJobActive(job.ID) {
		return nil, fmt.Errorf("server busy; cannot start concurrent review")
	}
	job.ReviewFeedback = feedback
	job.State = db.StateReviewing
	job.BlockerReason = ""
	job.CompletedAt = nil
	job.ReviewingWaitStartedAt = nil
	if err := o.db.UpdateJob(job); err != nil {
		return nil, fmt.Errorf("failed to accept review: %w", err)
	}
	o.markJobActive(job.ID)
	return job, nil
}

func (o *Orchestrator) runReview(job *db.Job) (resultErr error) {
	defer o.markJobInactive(job.ID)
	defer func() {
		if resultErr != nil {
			o.failJob(job, fmt.Sprintf("Review failed: %v", resultErr))
		}
	}()

	// Check if worktree exists on disk; recreate if it was cleaned up
	gitMgr := git.NewManager(job.RepoPath, o.baseBranch(job))
	if _, err := os.Stat(job.WorktreePath); os.IsNotExist(err) {
		o.log(job.ID, "info", "Worktree missing, recreating from existing branch")

		// Extract identifier from Linear issue ID for worktree path
		issue, err := o.linear.GetIssue(job.LinearIssueID)
		if err != nil {
			return fmt.Errorf("failed to get Linear issue: %w", err)
		}

		worktree, err := gitMgr.RecreateWorktree(issue.Identifier, job.BranchName)
		if err != nil {
			return fmt.Errorf("failed to recreate worktree: %w", err)
		}

		job.WorktreePath = worktree.Path
		if err := o.db.UpdateJob(job); err != nil {
			return fmt.Errorf("failed to update job with worktree path: %w", err)
		}

		o.log(job.ID, "info", fmt.Sprintf("Recreated worktree at %s", worktree.Path))
	} else if job.WorktreePath == "" {
		return fmt.Errorf("job has no worktree path and cannot recreate")
	}

	o.log(job.ID, "info", "Received review feedback")

	// Build review prompt. If this job previously failed validation, its
	// deduped errors come first as a mandatory step (UTA-97).
	reviewPrompt := mandatoryFixStep(job.FailureSummary) + fmt.Sprintf(`Review feedback on your pull request:

%s

Please address the feedback:
1. Review and understand each comment
2. Make the necessary changes to address the feedback
3. Run all relevant local CI-equivalent checks
4. Leave all completed changes in the assigned worktree for the host delivery gate

After making changes, report the checks you ran.
%s`, job.ReviewFeedback, agentQualityInstructions)

	// Create or reuse OpenCode session
	sessionID := job.OpenCodeSessionID
	if sessionID == "" {
		// Create new session if none exists
		issue, err := o.linear.GetIssue(job.LinearIssueID)
		if err != nil {
			return fmt.Errorf("failed to get Linear issue: %w", err)
		}
		sessionName := fmt.Sprintf("%s: Review iteration", issue.Identifier)
		session, err := o.opencode.CreateSession(sessionName, job.WorktreePath)
		if err != nil {
			return fmt.Errorf("failed to create OpenCode session: %w", err)
		}
		sessionID = session.ID
		job.OpenCodeSessionID = sessionID
		if err := o.db.UpdateJob(job); err != nil {
			return fmt.Errorf("failed to update job with session ID: %w", err)
		}
	}

	// Send review feedback to OpenCode
	started := time.Now()
	job.ReviewingWaitStartedAt = &started
	if err := o.db.UpdateJob(job); err != nil {
		return fmt.Errorf("failed to persist review start: %w", err)
	}
	if err := o.opencode.SendMessage(sessionID, reviewPrompt, job.WorktreePath); err != nil {
		return fmt.Errorf("failed to send review feedback to OpenCode: %w", err)
	}

	o.log(job.ID, "info", "Review feedback sent to OpenCode, waiting for completion")

	// Start event stream to capture OpenCode logs
	if err := o.startEventStream(job, sessionID); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to start event stream: %v", err))
		// Continue without streaming - not fatal
	}
	defer o.stopEventStream(job.ID)

	// Wait for OpenCode session to become idle
	logFunc := func(msg string) {
		o.log(job.ID, "info", msg)
	}

	if _, err := o.waitForSessionWithHealing(job, "reviewing", logFunc); err != nil {
		if err := o.recoverFromReviewWaitFailure(job, err); err != nil {
			return err
		}
	}

	// All review iterations use the same host gate as initial delivery, even
	// when the agent made no commit or timed out after editing files.
	if err := o.pushBranch(job); err != nil {
		return fmt.Errorf("failed to deliver review: %w", err)
	}

	o.log(job.ID, "info", fmt.Sprintf("Successfully pushed updates to branch %s (PR: %s)", job.BranchName, job.PRURL))

	// Update job state back to pr_open (still has active PR)
	job.State = db.StatePROpen
	job.BlockerReason = ""
	job.FailureSignature, job.FailureSummary = "", ""
	job.CompletedAt = nil
	if err := o.db.UpdateJob(job); err != nil {
		return fmt.Errorf("failed to update job state: %w", err)
	}

	// Add comment to Linear about the review iteration
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err == nil {
		comment := fmt.Sprintf("✅ Review Feedback Addressed\n\nOpenCode has addressed the review feedback and pushed updates to the PR.\n\nPR: %s\n\n*Automated by devboxd*", job.PRURL)
		if err := o.linear.AddComment(issue.ID, comment); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to add Linear comment: %v", err))
		}
	}

	o.log(job.ID, "info", "Review iteration completed successfully")
	return nil
}

// waitForSessionWithHealing waits for an OpenCode session to complete, with automatic healing for dead sessions.
// phase should be "coding" or "reviewing" to determine which prompt to send on recreation.
// Returns the final session ID (which may differ from job.OpenCodeSessionID if healing occurred).
func (o *Orchestrator) waitForSessionWithHealing(job *db.Job, phase string, logFunc func(string)) (string, error) {
	sessionID := job.OpenCodeSessionID

	// Determine wait timeout - use remaining time if resuming, else full budget
	waitBudget := o.cfg.OpenCode.Timeout
	if phase == "reviewing" {
		if o.cfg.OpenCode.ReviewTimeout > 0 {
			waitBudget = o.cfg.OpenCode.ReviewTimeout
		} else if waitBudget <= 0 || waitBudget > maxReviewWait {
			// Older programmatic configs do not populate ReviewTimeout. Keep
			// their shorter test budgets, otherwise use the safe default.
			waitBudget = maxReviewWait
		}
	}
	timeout := waitBudget
	var waitStartedAt *time.Time

	if phase == "coding" {
		waitStartedAt = job.CodingWaitStartedAt
	} else if phase == "reviewing" {
		waitStartedAt = job.ReviewingWaitStartedAt
	}

	// If wait was already in progress, calculate remaining time
	if waitStartedAt != nil {
		elapsed := time.Since(*waitStartedAt)
		remaining := waitBudget - elapsed

		if remaining <= 0 {
			// Already exhausted - fail immediately
			return sessionID, fmt.Errorf("wait timeout already exhausted: elapsed %v (started at %v)",
				elapsed, waitStartedAt.Format(time.RFC3339))
		}

		timeout = remaining
		o.log(job.ID, "info", fmt.Sprintf("Resuming %s wait: %v elapsed, %v remaining (budget: %v)",
			phase, elapsed.Round(time.Second), timeout.Round(time.Second), waitBudget))
	} else {
		// First time waiting - persist start time
		now := time.Now()
		if phase == "coding" {
			job.CodingWaitStartedAt = &now
		} else if phase == "reviewing" {
			job.ReviewingWaitStartedAt = &now
		}
		if err := o.db.UpdateJob(job); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to persist wait start time: %v", err))
			// Continue anyway - not fatal
		}
		o.log(job.ID, "info", fmt.Sprintf("Starting %s wait with %v timeout", phase, timeout))
	}

	for {
		if err := o.checkCancelled(job.ID); err != nil {
			return sessionID, err
		}

		// Try to wait for the session with remaining timeout
		err := o.opencode.WaitForSessionIdle(sessionID, timeout, job.WorktreePath, logFunc)

		// CancelJob marks cancelled then StopSession; a stopped session may look
		// idle. Prefer the cancelled terminal state over continuing the pipeline.
		if cancelErr := o.checkCancelled(job.ID); cancelErr != nil {
			return sessionID, cancelErr
		}

		if err == nil {
			// Success - session completed, clear wait start time
			if phase == "coding" {
				job.CodingWaitStartedAt = nil
			} else if phase == "reviewing" {
				job.ReviewingWaitStartedAt = nil
			}
			if updateErr := o.db.UpdateJob(job); updateErr != nil {
				o.log(job.ID, "warn", fmt.Sprintf("Failed to clear wait start time: %v", updateErr))
			}
			return sessionID, nil
		}

		// Check if this looks like a dead/missing session error
		errMsg := err.Error()
		isDeadSession := strings.Contains(errMsg, "never appeared in active map") ||
			strings.Contains(errMsg, "session") && strings.Contains(errMsg, "not found") ||
			strings.Contains(errMsg, "404")

		if !isDeadSession {
			// Not a dead session error - could be timeout or other issue
			// Return the error as-is (keep wait start time for potential future resume)
			return sessionID, err
		}

		// Dead session detected - attempt to heal
		o.log(job.ID, "warn", fmt.Sprintf("Detected dead/missing OpenCode session %s", sessionID))

		// Check healing attempt limit
		attemptKey := fmt.Sprintf("%s-%s", job.ID, phase)
		attempts := o.healingAttempts[attemptKey]

		if attempts >= maxHealingAttempts {
			o.log(job.ID, "error", fmt.Sprintf("Max healing attempts (%d) reached for phase %s", maxHealingAttempts, phase))
			return sessionID, fmt.Errorf("session healing failed after %d attempts: %w", maxHealingAttempts, err)
		}

		// Increment healing attempts
		o.healingAttempts[attemptKey]++
		o.log(job.ID, "info", fmt.Sprintf("Attempting session healing (attempt %d/%d)", o.healingAttempts[attemptKey], maxHealingAttempts))

		// Heal the session
		newSessionID, healErr := o.healSession(job, phase)
		if healErr != nil {
			o.log(job.ID, "error", fmt.Sprintf("Session healing failed: %v", healErr))
			return sessionID, fmt.Errorf("failed to heal session: %w", healErr)
		}

		// Update to new session ID and retry wait
		sessionID = newSessionID
		o.log(job.ID, "info", fmt.Sprintf("Session healed successfully, new session ID: %s", sessionID))

		// Recalculate remaining timeout before retry
		if waitStartedAt != nil {
			elapsed := time.Since(*waitStartedAt)
			timeout = waitBudget - elapsed
			if timeout <= 0 {
				return sessionID, fmt.Errorf("wait timeout exhausted during healing: elapsed %v", elapsed)
			}
		}

		// Continue loop to wait on new session
	}
}

// healSession creates a new OpenCode session and resends the appropriate prompt.
// phase should be "coding" or "reviewing" to determine which prompt to send.
// Returns the new session ID.
func (o *Orchestrator) healSession(job *db.Job, phase string) (string, error) {
	// Fetch issue details for prompt
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil {
		return "", fmt.Errorf("failed to fetch Linear issue: %w", err)
	}

	// Create new session
	sessionName := fmt.Sprintf("%s: %s (healed)", issue.Identifier, issue.Title)
	session, err := o.opencode.CreateSession(sessionName, job.WorktreePath)
	if err != nil {
		return "", fmt.Errorf("failed to create new session: %w", err)
	}

	o.log(job.ID, "info", fmt.Sprintf("Created new OpenCode session: %s", session.ID))

	// Build and send appropriate prompt based on phase
	var prompt string
	if phase == "coding" {
		// Get job for operator context
		currentJob, err := o.db.GetJob(job.ID)
		if err != nil {
			return "", fmt.Errorf("failed to get job: %w", err)
		}
		prompt = o.buildCodingPrompt(issue, currentJob.OperatorContext, o.priorFailureSummary(currentJob))
	} else if phase == "reviewing" {
		prompt = `Please review the changes you just made:
1. Check for code quality issues
2. Verify tests are passing
3. Ensure documentation is updated
4. Confirm the implementation matches requirements
5. **TUI changes (internal/tui/)**: Verify the dashboard fits entirely in one terminal screen with no overflow or clipped header/footer. Left column height (jobs + errors + integrations) must equal right column height (server logs + job logs).

If you find issues, fix them now.` + agentQualityInstructions
		if job.ReviewFeedback != "" {
			prompt += "\nAddress the persisted review feedback:\n" + job.ReviewFeedback
		}
	} else {
		return "", fmt.Errorf("unknown phase: %s", phase)
	}

	// Send prompt to new session
	if err := o.opencode.SendMessage(session.ID, prompt, job.WorktreePath); err != nil {
		return "", fmt.Errorf("failed to send prompt to new session: %w", err)
	}

	o.log(job.ID, "info", fmt.Sprintf("Re-sent %s prompt to new session", phase))

	// Update job with new session ID
	job.OpenCodeSessionID = session.ID
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to update job with new session ID: %v", err))
		// Continue anyway - session is created and prompt is sent
	}

	return session.ID, nil
}

// startEventStream starts streaming OpenCode session events and logging them
func (o *Orchestrator) startEventStream(job *db.Job, sessionID string) error {
	// Only support v2
	if o.cfg.OpenCode.Version != "v2" {
		return nil // Silently skip for non-v2
	}

	o.log(job.ID, "info", "Starting OpenCode event stream")

	// Create event handler that logs interesting events
	handler := func(event opencode.SessionEvent) error {
		// Map events to log entries
		switch event.Type {
		case "session.status":
			// Log status changes
			if status, ok := event.Properties["status"].(string); ok {
				o.log(job.ID, "info", fmt.Sprintf("OpenCode session status: %s", status))
			}

		case "session.next.text.delta":
			// Log streaming text deltas (coalesce to reduce noise)
			if text, ok := event.Data["text"].(string); ok && len(text) > 0 {
				// Only log substantial chunks to avoid flooding
				if len(text) > 20 {
					preview := text
					if len(preview) > 60 {
						preview = preview[:60] + "..."
					}
					o.log(job.ID, "info", fmt.Sprintf("OpenCode: %s", preview))
				}
			}

		case "session.next.text.ended":
			o.log(job.ID, "info", "OpenCode: text generation completed")

		case "session.next.tool.started":
			// Log tool calls
			if toolName, ok := event.Data["tool"].(string); ok {
				o.log(job.ID, "info", fmt.Sprintf("OpenCode: calling tool '%s'", toolName))
			} else if toolMap, ok := event.Data["tool"].(map[string]interface{}); ok {
				if name, ok := toolMap["name"].(string); ok {
					o.log(job.ID, "info", fmt.Sprintf("OpenCode: calling tool '%s'", name))
				}
			}

		case "session.next.tool.ended":
			if toolName, ok := event.Data["tool"].(string); ok {
				o.log(job.ID, "info", fmt.Sprintf("OpenCode: tool '%s' completed", toolName))
			} else if toolMap, ok := event.Data["tool"].(map[string]interface{}); ok {
				if name, ok := toolMap["name"].(string); ok {
					o.log(job.ID, "info", fmt.Sprintf("OpenCode: tool '%s' completed", name))
				}
			}

		case "session.error":
			// Log errors
			if errMsg, ok := event.Data["error"].(string); ok {
				o.log(job.ID, "error", fmt.Sprintf("OpenCode error: %s", errMsg))
			} else if errMap, ok := event.Data["error"].(map[string]interface{}); ok {
				if msg, ok := errMap["message"].(string); ok {
					o.log(job.ID, "error", fmt.Sprintf("OpenCode error: %s", msg))
				}
			}

		case "stream.error":
			// Stream connection error
			if errMsg, ok := event.Data["error"].(string); ok {
				o.log(job.ID, "warn", fmt.Sprintf("Event stream error: %s", errMsg))
			}
			return fmt.Errorf("stream error") // Stop on stream error

		case "message.updated":
			// Log message updates (concise)
			o.log(job.ID, "info", "OpenCode: message updated")

		default:
			// Skip other events silently to reduce noise
		}

		return nil
	}

	// Start streaming
	stopFunc, err := o.opencode.StreamSessionEvents(sessionID, handler)
	if err != nil {
		return fmt.Errorf("failed to start event stream: %w", err)
	}

	// Store stop function
	o.streamMu.Lock()
	o.streamStopFuncs[job.ID] = stopFunc
	o.streamMu.Unlock()

	return nil
}

// stopEventStream stops the event stream for a job
func (o *Orchestrator) stopEventStream(jobID string) {
	o.streamMu.Lock()
	defer o.streamMu.Unlock()

	if stopFunc, exists := o.streamStopFuncs[jobID]; exists {
		stopFunc()
		delete(o.streamStopFuncs, jobID)
	}
}

// prStatus is the lifecycle state of a GitHub PR as seen by the reconciler.
type prStatus string

const (
	prStatusOpen     prStatus = "open"
	prStatusMerged   prStatus = "merged"
	prStatusClosed   prStatus = "closed"
	prStatusNotFound prStatus = "not_found"
)

// StartReconciler polls GitHub periodically for jobs stuck in pr_open and
// advances them to a terminal state (UTA-68). It blocks forever; callers
// should run it in a goroutine. If the reconciler is disabled in config,
// it logs and returns immediately.
func (o *Orchestrator) StartReconciler() {
	if !o.cfg.ReconcilerEnabled() {
		log.Printf("[reconciler] disabled, not starting")
		return
	}
	interval := o.cfg.ReconcilerInterval()
	log.Printf("[reconciler] starting (interval=%s)", interval)

	// Reconcile once at startup so a restart picks up merged/closed PRs
	// without waiting a full interval, then tick.
	o.reconcileJobs()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		o.reconcileJobs()
	}
}

// reconcileJobs checks every pr_open job against GitHub PR state and moves
// merged PRs to done and closed/unmerged PRs to cancelled. Jobs in other
// states and jobs without a pr_url are never mutated.
func (o *Orchestrator) reconcileJobs() {
	if !o.cfg.ReconcilerEnabled() {
		return
	}

	jobs, err := o.db.ListJobs(0)
	if err != nil {
		log.Printf("[reconciler] failed to list jobs: %v", err)
		return
	}

	for _, j := range jobs {
		if j.State != db.StatePROpen {
			continue
		}
		if strings.TrimSpace(j.PRURL) == "" {
			log.Printf("[reconciler] job %s in pr_open has no pr_url, skipping", j.ID)
			continue
		}

		status, prNumber, err := o.getPRStatus(j.PRURL)
		if err != nil {
			log.Printf("[reconciler] job %s: failed to check PR %s: %v", j.ID, j.PRURL, err)
			continue
		}

		switch status {
		case prStatusMerged:
			now := time.Now()
			j.State = db.StateDone
			j.CompletedAt = &now
			if err := o.db.UpdateJob(j); err != nil {
				log.Printf("[reconciler] job %s: failed to mark done: %v", j.ID, err)
				continue
			}
			o.log(j.ID, "info", fmt.Sprintf("PR %s merged → done", prRef(prNumber, j.PRURL)))
			log.Printf("[reconciler] job %s PR %s merged → done", j.ID, prRef(prNumber, j.PRURL))
			o.cleanupWorktreeBestEffort(j)
		case prStatusClosed, prStatusNotFound:
			reason := "PR closed without merge"
			jobMsg := fmt.Sprintf("PR %s closed without merge → cancelled", prRef(prNumber, j.PRURL))
			if status == prStatusNotFound {
				// A PR that no longer exists on GitHub (deleted repo, force-deleted
				// ref, or bogus URL) will never merge, so treat it as closed
				// rather than leaving the job stuck in pr_open forever.
				reason = "PR not found on GitHub (treated as closed without merge)"
				jobMsg = fmt.Sprintf("PR %s not found on GitHub → cancelled", prRef(prNumber, j.PRURL))
			}
			now := time.Now()
			j.State = db.StateCancelled
			j.BlockerReason = reason
			j.CompletedAt = &now
			if err := o.db.UpdateJob(j); err != nil {
				log.Printf("[reconciler] job %s: failed to mark cancelled: %v", j.ID, err)
				continue
			}
			o.log(j.ID, "info", jobMsg)
			log.Printf("[reconciler] job %s PR %s closed → cancelled", j.ID, prRef(prNumber, j.PRURL))
			o.cleanupWorktreeBestEffort(j)
		case prStatusOpen:
			// Still waiting on external review — leave unchanged.
		default:
			log.Printf("[reconciler] job %s: unknown PR status %q for %s, skipping", j.ID, status, j.PRURL)
		}
	}
}

// getPRStatus returns the current lifecycle state of a GitHub PR. Tests can
// stub it via the prStatusFn field; otherwise it shells out to `gh pr view`.
func (o *Orchestrator) getPRStatus(prURL string) (prStatus, int, error) {
	if o.prStatusFn != nil {
		return o.prStatusFn(prURL)
	}
	return defaultGetPRStatus(prURL)
}

// defaultGetPRStatus queries GitHub for a PR's state via the gh CLI.
func defaultGetPRStatus(prURL string) (prStatus, int, error) {
	cmd := exec.Command("gh", "pr", "view", prURL, "--json", "number,state")
	output, err := cmd.CombinedOutput()
	if err != nil {
		lowered := strings.ToLower(string(output))
		if strings.Contains(lowered, "not found") ||
			strings.Contains(lowered, "no pull requests") ||
			strings.Contains(lowered, "could not resolve") ||
			strings.Contains(lowered, "404") {
			return prStatusNotFound, prNumberFromURL(prURL), nil
		}
		return "", 0, fmt.Errorf("gh pr view failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	var pr struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(output, &pr); err != nil {
		return "", 0, fmt.Errorf("failed to parse gh pr view output: %w", err)
	}

	number := pr.Number
	if number == 0 {
		number = prNumberFromURL(prURL)
	}

	// gh reports state as OPEN, CLOSED, or MERGED.
	switch strings.ToUpper(strings.TrimSpace(pr.State)) {
	case "MERGED":
		return prStatusMerged, number, nil
	case "CLOSED":
		return prStatusClosed, number, nil
	case "OPEN":
		return prStatusOpen, number, nil
	default:
		return "", number, fmt.Errorf("unknown PR state %q", pr.State)
	}
}

// prNumberFromURL extracts the PR number from a GitHub PR URL
// (e.g. https://github.com/owner/repo/pull/123 -> 123), or 0 if unparseable.
func prNumberFromURL(prURL string) int {
	idx := strings.LastIndex(prURL, "/pull/")
	if idx == -1 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(prURL[idx+len("/pull/"):]))
	if err != nil {
		return 0
	}
	return n
}

// prRef formats a PR reference for log lines, preferring #N when known.
func prRef(number int, prURL string) string {
	if number > 0 {
		return fmt.Sprintf("#%d", number)
	}
	return prURL
}

// cleanupWorktreeBestEffort removes a reconciled job's worktree without
// failing the state transition if cleanup fails.
func (o *Orchestrator) cleanupWorktreeBestEffort(j *db.Job) {
	if j.WorktreePath == "" {
		return
	}
	if _, err := os.Stat(j.WorktreePath); os.IsNotExist(err) {
		return
	}
	gitMgr := git.NewManager(j.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
	if err := gitMgr.RemoveWorktree(j.WorktreePath); err != nil {
		o.log(j.ID, "warn", fmt.Sprintf("Failed to remove worktree: %v", err))
	}
}

// fallbackCheckpointError means the assigned branch could not be updated, but
// the incomplete work was published and verified on a separate fallback ref.
type fallbackCheckpointError struct {
	ref   string
	cause error
}

func (e *fallbackCheckpointError) Error() string {
	return fmt.Sprintf("push failed (%v); work preserved on fallback ref %s", e.cause, e.ref)
}

// fallbackCheckpointRef names the rescue ref for a job: devbox/<issue>-checkpoint-<jobid8>.
func fallbackCheckpointRef(job *db.Job) string {
	short := job.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("devbox/%s-checkpoint-%s", strings.ToLower(job.LinearIssueID), short)
}

// publishFallbackCheckpoint pushes HEAD to the fallback ref, verifies it on the
// remote, and posts a Linear comment naming the ref (and conflicting files).
func (o *Orchestrator) publishFallbackCheckpoint(job *db.Job, manager *git.Manager, pushErr error) (string, error) {
	ref := fallbackCheckpointRef(job)
	if err := manager.PushHeadToRef(job.WorktreePath, ref); err != nil {
		return "", err
	}
	o.log(job.ID, "warn", fmt.Sprintf("Push to %s failed; incomplete work published to fallback ref %s", job.BranchName, ref))

	detail := ""
	if ce, ok := git.IsConflict(pushErr); ok && len(ce.Files) > 0 {
		detail = "\n\nConflicting files vs `origin/" + ce.Branch + "`:\n- `" + strings.Join(ce.Files, "`\n- `") + "`"
	}
	issue, err := o.linear.GetIssue(job.LinearIssueID)
	if err != nil || issue == nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to fetch Linear issue for fallback checkpoint comment: %v", err))
		return ref, nil
	}
	comment := fmt.Sprintf("⚠️ Devbox could not update `%s`\n\nJob %s failed and its incomplete work could not be pushed to the assigned branch, so it was preserved on `%s`.%s\n\nIntegrate that ref manually (merge or rebase) before the next continue. Nothing was force-pushed.\n\n*Automated by devboxd*", job.BranchName, job.ID, ref, detail)
	if err := o.linear.AddComment(issue.ID, comment); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Failed to add Linear fallback checkpoint comment: %v", err))
	}
	return ref, nil
}
