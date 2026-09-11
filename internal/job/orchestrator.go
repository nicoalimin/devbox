package job

import (
	"fmt"
	"strings"
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
	cfg       *config.Config
	db        *db.DB
	linear    *linear.Client
	opencode  *opencode.Client
}

// NewOrchestrator creates a new job orchestrator
func NewOrchestrator(cfg *config.Config, database *db.DB) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		db:       database,
		linear:   linear.NewClient(cfg.Linear.APIKey),
		opencode: opencode.NewClient(cfg.OpenCode.BaseURL, cfg.OpenCode.Username, cfg.OpenCode.Password),
	}
}

// CreateJob creates a new job for a Linear issue
func (o *Orchestrator) CreateJob(linearIssueID string) (*db.Job, error) {
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
		ID:            uuid.New().String(),
		LinearIssueID: linearIssueID,
		State:         db.StateFetching,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	if err := o.db.CreateJob(job); err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	o.log(job.ID, "info", fmt.Sprintf("Created job for Linear issue %s", linearIssueID))

	// Start processing asynchronously
	go o.ProcessJob(job.ID)

	return job, nil
}

// ProcessJob processes a job through its lifecycle
func (o *Orchestrator) ProcessJob(jobID string) {
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
	session, err := o.opencode.CreateSession(sessionName)
	if err != nil {
		return err
	}

	job.OpenCodeSessionID = session.ID
	if err := o.db.UpdateJob(job); err != nil {
		return err
	}

	// Build prompt with context
	prompt := o.buildCodingPrompt(issue)

	// Send task to OpenCode
	if err := o.opencode.SendMessage(session.ID, prompt); err != nil {
		return err
	}

	o.log(job.ID, "info", "Sent task to OpenCode")

	// Note: In a real implementation, we'd monitor OpenCode session status
	// For now, we assume it completes. The "blocked" state would be set
	// if OpenCode indicates it needs clarification.

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

If you find issues, fix them now. If everything looks good, confirm the changes are ready.`

	if err := o.opencode.SendMessage(job.OpenCodeSessionID, reviewPrompt); err != nil {
		return err
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

	o.log(job.ID, "info", "Pushing branch to remote")

	gitMgr := git.NewManager(job.RepoPath, o.cfg.GitHub.DefaultBaseBranch)
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

	o.log(job.ID, "info", fmt.Sprintf("Pull request created: %s", prURL))

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

// buildCodingPrompt builds the prompt for OpenCode
func (o *Orchestrator) buildCodingPrompt(issue *linear.Issue) string {
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
	if err := o.db.AddLog(jobID, level, message); err != nil {
		// Log to stderr if DB logging fails
		fmt.Printf("[%s] %s: %s (failed to write to DB: %v)\n", jobID, level, message, err)
	}
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
		if err := o.opencode.SendMessage(job.OpenCodeSessionID, message); err != nil {
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
