package job

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/git"
	"github.com/nicoalimin/devbox/internal/linear"
	"github.com/nicoalimin/devbox/internal/opencode"
)

// Orchestrator manages job lifecycle
type Orchestrator struct {
	cfg             *config.Config
	db              *db.DB
	linear          *linear.Client
	opencode        *opencode.Client
	activeJobs      map[string]bool              // Track actively running jobs to prevent double-resume
	healingAttempts map[string]int               // Track session healing attempts per job (jobID -> count)
	streamStopFuncs map[string]func()            // Stop functions for active event streams (jobID -> stopFunc)
	streamMu        sync.Mutex                   // Mutex to protect streamStopFuncs map
}

const (
	maxHealingAttempts = 2 // Max number of session recreate attempts per phase
)

// NewOrchestrator creates a new job orchestrator
func NewOrchestrator(cfg *config.Config, database *db.DB) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		db:              database,
		linear:          linear.NewClient(cfg.Linear.APIKey),
		opencode:        opencode.NewClient(cfg.OpenCode.BaseURL, cfg.OpenCode.Username, cfg.OpenCode.Password, cfg.OpenCode.Version),
		activeJobs:      make(map[string]bool),
		healingAttempts: make(map[string]int),
		streamStopFuncs: make(map[string]func()),
	}
}

// markJobActive marks a job as actively running
func (o *Orchestrator) markJobActive(jobID string) {
	o.activeJobs[jobID] = true
}

// markJobInactive marks a job as no longer running
func (o *Orchestrator) markJobInactive(jobID string) {
	delete(o.activeJobs, jobID)
}

// isJobActive checks if a job is currently running
func (o *Orchestrator) isJobActive(jobID string) bool {
	return o.activeJobs[jobID]
}

// CreateJob creates a new job for a Linear issue with optional operator context
func (o *Orchestrator) CreateJob(linearIssueID string, operatorContext string) (*db.Job, error) {
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

	// Fetch Linear issue
	if err := o.fetchLinearIssue(job); err != nil {
		o.failJob(job, fmt.Sprintf("Failed to fetch Linear issue: %v", err))
		return
	}

	// Prepare worktree
	if err := o.prepareWorktree(job); err != nil {
		o.failJob(job, fmt.Sprintf("Failed to prepare worktree: %v", err))
		return
	}

	// Execute coding task
	if err := o.executeCoding(job); err != nil {
		o.failJob(job, fmt.Sprintf("Failed to execute coding: %v", err))
		return
	}

	// Review code
	if err := o.reviewCode(job); err != nil {
		o.failJob(job, fmt.Sprintf("Failed to review code: %v", err))
		return
	}

	// Push branch
	if err := o.pushBranch(job); err != nil {
		o.failJob(job, fmt.Sprintf("Failed to push branch: %v", err))
		return
	}

	// Create PR
	if err := o.createPullRequest(job); err != nil {
		o.failJob(job, fmt.Sprintf("Failed to create PR: %v", err))
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

If you find issues, fix them now. If everything looks good, confirm the changes are ready.`

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
		return fmt.Errorf("OpenCode review timed out or failed: %w", err)
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

// pushBranch pushes the branch to the remote
func (o *Orchestrator) pushBranch(job *db.Job) error {
	job.State = db.StatePushing
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(job.ID, "info", "Verifying commits before push")

	gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
	
	// Check if there are commits ahead of base
	hasCommits, err := gitMgr.HasCommitsAheadOfBase(job.WorktreePath)
	if err != nil {
		return fmt.Errorf("failed to check for commits: %w", err)
	}
	
	if !hasCommits {
		return fmt.Errorf("no commits found on branch %s compared to base branch - OpenCode may have completed without making changes", job.BranchName)
	}

	o.log(job.ID, "info", "Commits verified, pushing branch to remote")

	if err := gitMgr.PushBranch(job.WorktreePath, job.BranchName); err != nil {
		return err
	}

	o.log(job.ID, "info", "Branch pushed successfully")
	return nil
}

// createPullRequest creates a PR using gh CLI
func (o *Orchestrator) createPullRequest(job *db.Job) error {
	job.State = db.StatePROpen
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

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
	repo := o.cfg.FindRepo(issue.Team.Key, "", nil)
	baseBranch := o.cfg.GitHub.DefaultBaseBranch
	if repo != nil && repo.BaseBranch != "" {
		baseBranch = repo.BaseBranch
	}

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
	now := time.Now()
	job.State = db.StateFailed
	job.BlockerReason = reason
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to mark job failed: %v", err))
		return
	}

	o.log(job.ID, "error", fmt.Sprintf("Job failed: %s", reason))

	// Clean up worktree if it exists
	if job.WorktreePath != "" {
		gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
		if err := gitMgr.RemoveWorktree(job.WorktreePath); err != nil {
			o.log(job.ID, "warn", fmt.Sprintf("Failed to remove worktree: %v", err))
		}
	}
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
	sb.WriteString("4. Run tests and ensure code quality\n")
	sb.WriteString("5. Commit your changes with clear messages\n")

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
	if job.OpenCodeSessionID == "" && (job.State == db.StateCoding || job.State == db.StateReviewing) {
		o.failJob(job, fmt.Sprintf("Cannot resume %s: missing OpenCode session ID", job.State))
		return
	}

	if job.WorktreePath == "" && job.State != db.StateFetching {
		o.failJob(job, fmt.Sprintf("Cannot resume %s: missing worktree path", job.State))
		return
	}

	// Resume from the appropriate state
	switch job.State {
	case db.StateFetching:
		// Resume from the beginning
		if err := o.fetchLinearIssue(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to fetch Linear issue: %v", err))
			return
		}
		fallthrough

	case db.StatePreparing:
		// Continue with worktree preparation
		if err := o.prepareWorktree(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to prepare worktree: %v", err))
			return
		}
		fallthrough

	case db.StateCoding:
		// Resume waiting for OpenCode coding session
		if err := o.resumeCoding(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to resume coding: %v", err))
			return
		}
		fallthrough

	case db.StateReviewing:
		// Resume waiting for OpenCode review session
		if err := o.resumeReviewing(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to resume reviewing: %v", err))
			return
		}
		fallthrough

	case db.StatePushing:
		// Resume pushing branch
		if err := o.pushBranch(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to push branch: %v", err))
			return
		}

		// Create PR
		if err := o.createPullRequest(job); err != nil {
			o.failJob(job, fmt.Sprintf("Failed to create PR: %v", err))
			return
		}

		// Mark complete
		o.completeJob(job)

	default:
		o.failJob(job, fmt.Sprintf("Cannot resume from unknown state: %s", job.State))
	}
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
		return fmt.Errorf("OpenCode review timed out or failed: %w", err)
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

	now := time.Now()
	job.State = db.StateCancelled
	job.CompletedAt = &now
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	o.log(jobID, "info", "Job cancelled")

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
	// Try to find job by ID first, then by Linear issue ID
	job, err := o.db.GetJob(jobIDOrLinearID)
	if err != nil {
		return fmt.Errorf("failed to get job: %w", err)
	}
	if job == nil {
		// Try finding by Linear issue ID
		job, err = o.db.GetJobByLinearIssueID(jobIDOrLinearID)
		if err != nil {
			return fmt.Errorf("failed to get job by Linear ID: %w", err)
		}
		if job == nil {
			return fmt.Errorf("job not found: %s", jobIDOrLinearID)
		}
	}

	// Check that job has a PR
	if job.PRURL == "" {
		return fmt.Errorf("job does not have a pull request yet (state: %s)", job.State)
	}

	// Check that we have necessary information
	if job.BranchName == "" {
		return fmt.Errorf("job has no branch name")
	}
	if job.RepoPath == "" {
		return fmt.Errorf("job has no repo path")
	}

	// Check if worktree exists on disk; recreate if it was cleaned up
	gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
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

	// Store review feedback
	job.ReviewFeedback = feedback
	job.State = db.StateReviewing
	if err := o.db.UpdateJob(job); err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}

	// Build review prompt
	reviewPrompt := fmt.Sprintf(`Review feedback on your pull request:

%s

Please address the feedback:
1. Review and understand each comment
2. Make the necessary changes to address the feedback
3. Run tests to ensure everything still works
4. Commit your changes with clear messages describing what you fixed

After making changes, confirm they are ready to push.`, feedback)

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
	
	if err := o.opencode.WaitForSessionIdle(sessionID, o.cfg.OpenCode.Timeout, job.WorktreePath, logFunc); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("OpenCode session did not complete: %v", err))
		return fmt.Errorf("OpenCode session timeout or error: %w", err)
	}

	o.log(job.ID, "info", "OpenCode session completed, checking for new commits")

	// Verify that new commits exist (reuse gitMgr from earlier)
	hasCommits, err := gitMgr.HasCommitsAheadOfBase(job.WorktreePath)
	if err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to check for commits: %v", err))
		return fmt.Errorf("failed to check for commits: %w", err)
	}

	if !hasCommits {
		o.log(job.ID, "warn", "No new commits found after review - OpenCode may not have made changes")
		// Don't fail, just update state back to pr_open
		job.State = db.StatePROpen
		if err := o.db.UpdateJob(job); err != nil {
			return fmt.Errorf("failed to update job state: %w", err)
		}
		return fmt.Errorf("no new commits found after review iteration")
	}

	o.log(job.ID, "info", "New commits detected, pushing to existing branch")

	// Push to the same branch (updates the existing PR)
	if err := gitMgr.PushBranch(job.WorktreePath, job.BranchName); err != nil {
		o.log(job.ID, "error", fmt.Sprintf("Failed to push branch: %v", err))
		return fmt.Errorf("failed to push branch: %w", err)
	}

	o.log(job.ID, "info", fmt.Sprintf("Successfully pushed updates to branch %s (PR: %s)", job.BranchName, job.PRURL))

	// Update job state back to pr_open (still has active PR)
	job.State = db.StatePROpen
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
	timeout := o.cfg.OpenCode.Timeout
	var waitStartedAt *time.Time
	
	if phase == "coding" {
		waitStartedAt = job.CodingWaitStartedAt
	} else if phase == "reviewing" {
		waitStartedAt = job.ReviewingWaitStartedAt
	}
	
	// If wait was already in progress, calculate remaining time
	if waitStartedAt != nil {
		elapsed := time.Since(*waitStartedAt)
		remaining := o.cfg.OpenCode.Timeout - elapsed
		
		if remaining <= 0 {
			// Already exhausted - fail immediately
			return sessionID, fmt.Errorf("wait timeout already exhausted: elapsed %v (started at %v)", 
				elapsed, waitStartedAt.Format(time.RFC3339))
		}
		
		timeout = remaining
		o.log(job.ID, "info", fmt.Sprintf("Resuming %s wait: %v elapsed, %v remaining (budget: %v)", 
			phase, elapsed.Round(time.Second), timeout.Round(time.Second), o.cfg.OpenCode.Timeout))
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
		// Try to wait for the session with remaining timeout
		err := o.opencode.WaitForSessionIdle(sessionID, timeout, job.WorktreePath, logFunc)
		
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
			timeout = o.cfg.OpenCode.Timeout - elapsed
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

If you find issues, fix them now. If everything looks good, confirm the changes are ready.`
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
