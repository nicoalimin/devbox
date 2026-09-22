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
}

type issueClient interface {
	GetIssue(string) (*linear.Issue, error)
	AddComment(string, string) error
}

const (
	maxHealingAttempts = 2 // Max number of session recreate attempts per phase
	maxReviewWait      = 15 * time.Minute
	commitRecoveryWait = 10 * time.Minute
	maxRepairAttempts  = 3
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

// prepareWorktree creates a git worktree for the job
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

	// Create worktree
	gitMgr := git.NewManager(repo.Path, repo.BaseBranch)
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
	prompt := o.buildCodingPrompt(issue, currentJob.OperatorContext)

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

// pushBranch pushes the branch to the remote
func (o *Orchestrator) pushBranch(job *db.Job) error {
	job.State = db.StatePushing
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", "Preparing validated commits before push")

	gitMgr := git.NewManager(job.RepoPath, o.baseBranch(job))
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

	o.log(job.ID, "info", "Commits verified, pushing branch to remote")

	if err := o.pushWithRetry(job, gitMgr); err != nil {
		return err
	}

	o.log(job.ID, "info", "Branch pushed successfully")
	return nil
}

// validateWithOpenCodeRepair formats locally and retries the full gate after
// each bounded repair, including interrupted repairs that may have succeeded.
func (o *Orchestrator) validateWithOpenCodeRepair(job *db.Job, gitMgr *git.Manager) error {
	runValidation := func() error {
		// A repair agent can run arbitrary Git commands. Reassert the assigned
		// branch before every host-owned format/commit pass.
		if err := gitMgr.AssertBranch(job.WorktreePath, job.BranchName); err != nil {
			return err
		}
		o.log(job.ID, "info", "Running deterministic host formatting before validation")
		if err := validation.Format(job.WorktreePath, o.formatCommands(job), func(msg string) {
			o.log(job.ID, "info", msg)
		}); err != nil {
			return fmt.Errorf("host formatting failed: %w", err)
		}

		hasChanges, err := gitMgr.HasUncommittedChanges(job.WorktreePath)
		if err != nil {
			return fmt.Errorf("failed to inspect formatted worktree: %w", err)
		}
		if hasChanges {
			o.log(job.ID, "info", "Host formatting or agent output changed the worktree; creating chore: format commit")
			if err := gitMgr.CommitAll(job.WorktreePath, "chore: format"); err != nil {
				return fmt.Errorf("failed to commit host-formatted worktree: %w", err)
			}
			o.log(job.ID, "info", "Created chore: format commit")
		}

		o.log(job.ID, "info", "Running local CI-equivalent validation before push")
		return validation.Run(job.WorktreePath, o.validationCommands(job), func(msg string) {
			o.log(job.ID, "info", msg)
		})
	}

	for attempt := 0; ; attempt++ {
		if err := o.checkCancelled(job.ID); err != nil {
			return err
		}
		validationErr := runValidation()
		if validationErr == nil {
			return nil
		}
		if attempt == maxRepairAttempts || job.OpenCodeSessionID == "" {
			return fmt.Errorf("local validation failed after %d repair attempts: %w", attempt, validationErr)
		}
		o.log(job.ID, "warn", fmt.Sprintf("Repair attempt %d/%d: %v", attempt+1, maxRepairAttempts, validationErr))
		err := o.opencode.SendMessage(job.OpenCodeSessionID, buildValidationRepairPrompt(validationErr), job.WorktreePath)
		if err == nil {
			wait := commitRecoveryWait
			if o.cfg.OpenCode.Timeout > 0 && o.cfg.OpenCode.Timeout < wait {
				wait = o.cfg.OpenCode.Timeout
			}
			err = o.opencode.WaitForSessionIdle(job.OpenCodeSessionID, wait, job.WorktreePath, func(msg string) { o.log(job.ID, "info", msg) })
		}
		if err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Repair did not finish cleanly; stopping session and rechecking actual files: %v", err))
			if stopErr := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); stopErr != nil {
				return fmt.Errorf("repair failed (%v); cannot safely take over worktree: %w", err, stopErr)
			}
		}
	}
}

func buildValidationRepairPrompt(validationErr error) string {
	return fmt.Sprintf(`Devbox's local pre-push validation failed. Resolve the failure completely in the current worktree.

Validation output:
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
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = manager.PushBranch(job.WorktreePath, job.BranchName); err == nil {
			return nil
		}
		o.log(job.ID, "warn", fmt.Sprintf("Push attempt %d/3 failed: %v", attempt, err))
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return err
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

	prURL, err := git.CreatePR(job.WorktreePath, prTitle, prBody, baseBranch)
	if err != nil {
		return err
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
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to mark job complete: %v", err))
		return
	}

	o.log(job.ID, "info", "Job completed successfully")

	// Clean up worktree
	if job.WorktreePath != "" {
		gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
		if err := gitMgr.RemoveWorktree(job.WorktreePath); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to remove worktree: %v", err))
		}
	}
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

	// Preserve output on the remote even when quality checks or the model fail.
	// This is explicitly an incomplete checkpoint, never successful delivery.
	if job.WorktreePath != "" {
		published, err := o.checkpointFailedWork(job)
		if err != nil {
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
	job.BlockerReason = reason
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to mark job failed: %v", err))
		return
	}

	o.log(job.ID, "error", fmt.Sprintf("Job failed: %s", reason))
}

// checkpointFailedWork stops the agent, commits any local changes, and pushes
// only when the branch has real commits ahead of the base. Returns published=true
// only when a real commit/diff was pushed. An identical branch is not recovery.
func (o *Orchestrator) checkpointFailedWork(job *db.Job) (published bool, err error) {
	if err := o.opencode.StopSession(job.OpenCodeSessionID, job.WorktreePath); err != nil {
		return false, fmt.Errorf("cannot stop agent safely: %w", err)
	}
	manager := git.NewManager(job.RepoPath, o.baseBranch(job))
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

	hasCommits, err := manager.HasCommitsAheadOfBase(job.WorktreePath)
	if err != nil {
		return false, err
	}
	if !hasCommits {
		o.log(job.ID, "info", "No incomplete work to checkpoint (branch identical to base)")
		if err := manager.RemoveWorktree(job.WorktreePath); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Worktree cleanup deferred after empty checkpoint: %v", err))
		}
		return false, nil
	}

	if err := o.pushWithRetry(job, manager); err != nil {
		return false, err
	}
	o.log(job.ID, "warn", "Incomplete work checkpoint committed and verified on remote; job remains failed")
	if err := manager.RemoveWorktree(job.WorktreePath); err != nil {
		o.log(job.ID, "warn", fmt.Sprintf("Checkpoint published; worktree cleanup deferred: %v", err))
	}
	return true, nil
}

// buildCodingPrompt builds the prompt for OpenCode with optional operator context
func (o *Orchestrator) buildCodingPrompt(issue *linear.Issue, operatorContext string) string {
	var sb strings.Builder

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
	}

	sb.WriteString("Instructions:\n")
	sb.WriteString("1. Review the .opencode instructions in this repository\n")
	sb.WriteString("2. Search Notion for related PRDs, specs, or context using the issue title and team\n")
	sb.WriteString("3. Implement the required changes\n")
	sb.WriteString("4. Run the repository's CI-equivalent checks and resolve every failure\n")
	sb.WriteString("5. Leave all completed changes in the assigned worktree for the host delivery gate\n")
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

	// Build review prompt
	reviewPrompt := fmt.Sprintf(`Review feedback on your pull request:

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
		prompt = o.buildCodingPrompt(issue, currentJob.OperatorContext)
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
