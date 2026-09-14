package db

import (
	"database/sql"
	"embed"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaFS embed.FS

// JobState represents the state of a job
type JobState string

const (
	StateQueued   JobState = "queued"
	StateFetching JobState = "fetching"
	StatePreparing JobState = "preparing"
	StateCoding    JobState = "coding"
	StateReviewing JobState = "reviewing"
	StatePushing   JobState = "pushing"
	StatePROpen    JobState = "pr_open"
	StateDone      JobState = "done"
	StateBlocked   JobState = "blocked"
	StateFailed    JobState = "failed"
	StateCancelled JobState = "cancelled"
)

// IsTerminal returns true if the state is terminal (won't transition further)
func (s JobState) IsTerminal() bool {
	return s == StateDone || s == StateFailed || s == StateCancelled
}

// IsBusy returns true if the state indicates active processing
func (s JobState) IsBusy() bool {
	return s == StateFetching || s == StatePreparing || s == StateCoding ||
		s == StateReviewing || s == StatePushing
}

// Job represents a coding job
type Job struct {
	ID                      string     `json:"id"`
	LinearIssueID           string     `json:"linear_issue_id"`
	LinearURL               string     `json:"linear_url"`
	State                   JobState   `json:"state"`
	RepoPath                string     `json:"repo_path"`
	BranchName              string     `json:"branch_name"`
	WorktreePath            string     `json:"worktree_path"`
	PRURL                   string     `json:"pr_url"`
	BlockerReason           string     `json:"blocker_reason"`
	OpenCodeSessionID       string     `json:"opencode_session_id"`
	OperatorContext         string     `json:"operator_context"`
	ReviewFeedback          string     `json:"review_feedback"`
	CodingWaitStartedAt     *time.Time `json:"coding_wait_started_at"`
	ReviewingWaitStartedAt  *time.Time `json:"reviewing_wait_started_at"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	CompletedAt             *time.Time `json:"completed_at"`
}

// JobLog represents a log entry for a job
type JobLog struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

// DB wraps the database connection
type DB struct {
	conn *sql.DB
}

// Open opens a database connection and initializes the schema
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Enable foreign keys and WAL mode for better concurrency
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}
	if _, err := conn.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}

	// Initialize schema
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return nil, fmt.Errorf("failed to read schema: %w", err)
	}
	if _, err := conn.Exec(string(schema)); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Run migrations for existing databases
	if err := runMigrations(conn); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return &DB{conn: conn}, nil
}

// runMigrations applies schema migrations to existing databases
func runMigrations(conn *sql.DB) error {
	// Check if coding_wait_started_at column exists
	var codingColExists bool
	err := conn.QueryRow(`
		SELECT COUNT(*) > 0
		FROM pragma_table_info('jobs')
		WHERE name = 'coding_wait_started_at'
	`).Scan(&codingColExists)
	if err != nil {
		return fmt.Errorf("failed to check for coding_wait_started_at column: %w", err)
	}

	// Add coding_wait_started_at if missing
	if !codingColExists {
		if _, err := conn.Exec(`ALTER TABLE jobs ADD COLUMN coding_wait_started_at TIMESTAMP`); err != nil {
			return fmt.Errorf("failed to add coding_wait_started_at column: %w", err)
		}
	}

	// Check if reviewing_wait_started_at column exists
	var reviewingColExists bool
	err = conn.QueryRow(`
		SELECT COUNT(*) > 0
		FROM pragma_table_info('jobs')
		WHERE name = 'reviewing_wait_started_at'
	`).Scan(&reviewingColExists)
	if err != nil {
		return fmt.Errorf("failed to check for reviewing_wait_started_at column: %w", err)
	}

	// Add reviewing_wait_started_at if missing
	if !reviewingColExists {
		if _, err := conn.Exec(`ALTER TABLE jobs ADD COLUMN reviewing_wait_started_at TIMESTAMP`); err != nil {
			return fmt.Errorf("failed to add reviewing_wait_started_at column: %w", err)
		}
	}

	return nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.conn.Close()
}

// CreateJob creates a new job
func (db *DB) CreateJob(job *Job) error {
	_, err := db.conn.Exec(`
		INSERT INTO jobs (
			id, linear_issue_id, linear_url, state, repo_path, branch_name,
			worktree_path, pr_url, blocker_reason, opencode_session_id,
			operator_context, review_feedback,
			coding_wait_started_at, reviewing_wait_started_at,
			created_at, updated_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, job.ID, job.LinearIssueID, job.LinearURL, job.State, job.RepoPath,
		job.BranchName, job.WorktreePath, job.PRURL, job.BlockerReason,
		job.OpenCodeSessionID, job.OperatorContext, job.ReviewFeedback,
		job.CodingWaitStartedAt, job.ReviewingWaitStartedAt,
		job.CreatedAt, job.UpdatedAt, job.CompletedAt)
	return err
}

// UpdateJob updates an existing job
func (db *DB) UpdateJob(job *Job) error {
	job.UpdatedAt = time.Now()
	_, err := db.conn.Exec(`
		UPDATE jobs SET
			state = ?, repo_path = ?, branch_name = ?, worktree_path = ?,
			pr_url = ?, blocker_reason = ?, opencode_session_id = ?,
			operator_context = ?, review_feedback = ?,
			coding_wait_started_at = ?, reviewing_wait_started_at = ?,
			updated_at = ?, completed_at = ?
		WHERE id = ?
	`, job.State, job.RepoPath, job.BranchName, job.WorktreePath,
		job.PRURL, job.BlockerReason, job.OpenCodeSessionID,
		job.OperatorContext, job.ReviewFeedback,
		job.CodingWaitStartedAt, job.ReviewingWaitStartedAt,
		job.UpdatedAt, job.CompletedAt, job.ID)
	return err
}

// GetJob retrieves a job by ID
func (db *DB) GetJob(id string) (*Job, error) {
	var job Job
	err := db.conn.QueryRow(`
		SELECT id, linear_issue_id, linear_url, state, repo_path, branch_name,
			worktree_path, pr_url, blocker_reason, opencode_session_id,
			operator_context, review_feedback,
			coding_wait_started_at, reviewing_wait_started_at,
			created_at, updated_at, completed_at
		FROM jobs WHERE id = ?
	`, id).Scan(
		&job.ID, &job.LinearIssueID, &job.LinearURL, &job.State, &job.RepoPath,
		&job.BranchName, &job.WorktreePath, &job.PRURL, &job.BlockerReason,
		&job.OpenCodeSessionID, &job.OperatorContext, &job.ReviewFeedback,
		&job.CodingWaitStartedAt, &job.ReviewingWaitStartedAt,
		&job.CreatedAt, &job.UpdatedAt, &job.CompletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &job, err
}

// ListJobs lists jobs with optional limit
func (db *DB) ListJobs(limit int) ([]*Job, error) {
	query := "SELECT id, linear_issue_id, linear_url, state, repo_path, branch_name, worktree_path, pr_url, blocker_reason, opencode_session_id, operator_context, review_feedback, coding_wait_started_at, reviewing_wait_started_at, created_at, updated_at, completed_at FROM jobs ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(
			&job.ID, &job.LinearIssueID, &job.LinearURL, &job.State, &job.RepoPath,
			&job.BranchName, &job.WorktreePath, &job.PRURL, &job.BlockerReason,
			&job.OpenCodeSessionID, &job.OperatorContext, &job.ReviewFeedback,
			&job.CodingWaitStartedAt, &job.ReviewingWaitStartedAt,
			&job.CreatedAt, &job.UpdatedAt, &job.CompletedAt,
		); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, rows.Err()
}

// GetCurrentJob returns the currently busy job (if any)
func (db *DB) GetCurrentJob() (*Job, error) {
	var job Job
	err := db.conn.QueryRow(`
		SELECT id, linear_issue_id, linear_url, state, repo_path, branch_name,
			worktree_path, pr_url, blocker_reason, opencode_session_id,
			operator_context, review_feedback,
			coding_wait_started_at, reviewing_wait_started_at,
			created_at, updated_at, completed_at
		FROM jobs
		WHERE state IN ('fetching', 'preparing', 'coding', 'reviewing', 'pushing')
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(
		&job.ID, &job.LinearIssueID, &job.LinearURL, &job.State, &job.RepoPath,
		&job.BranchName, &job.WorktreePath, &job.PRURL, &job.BlockerReason,
		&job.OpenCodeSessionID, &job.OperatorContext, &job.ReviewFeedback,
		&job.CodingWaitStartedAt, &job.ReviewingWaitStartedAt,
		&job.CreatedAt, &job.UpdatedAt, &job.CompletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &job, err
}

// GetJobByLinearIssueID retrieves a job by Linear issue ID (most recent first)
func (db *DB) GetJobByLinearIssueID(linearIssueID string) (*Job, error) {
	var job Job
	err := db.conn.QueryRow(`
		SELECT id, linear_issue_id, linear_url, state, repo_path, branch_name,
			worktree_path, pr_url, blocker_reason, opencode_session_id,
			operator_context, review_feedback,
			coding_wait_started_at, reviewing_wait_started_at,
			created_at, updated_at, completed_at
		FROM jobs
		WHERE linear_issue_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, linearIssueID).Scan(
		&job.ID, &job.LinearIssueID, &job.LinearURL, &job.State, &job.RepoPath,
		&job.BranchName, &job.WorktreePath, &job.PRURL, &job.BlockerReason,
		&job.OpenCodeSessionID, &job.OperatorContext, &job.ReviewFeedback,
		&job.CodingWaitStartedAt, &job.ReviewingWaitStartedAt,
		&job.CreatedAt, &job.UpdatedAt, &job.CompletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &job, err
}

// GetBlockedJobs returns jobs in blocked state
func (db *DB) GetBlockedJobs() ([]*Job, error) {
	rows, err := db.conn.Query(`
		SELECT id, linear_issue_id, linear_url, state, repo_path, branch_name,
			worktree_path, pr_url, blocker_reason, opencode_session_id,
			operator_context, review_feedback,
			coding_wait_started_at, reviewing_wait_started_at,
			created_at, updated_at, completed_at
		FROM jobs
		WHERE state = 'blocked'
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(
			&job.ID, &job.LinearIssueID, &job.LinearURL, &job.State, &job.RepoPath,
			&job.BranchName, &job.WorktreePath, &job.PRURL, &job.BlockerReason,
			&job.OpenCodeSessionID, &job.OperatorContext, &job.ReviewFeedback,
			&job.CodingWaitStartedAt, &job.ReviewingWaitStartedAt,
			&job.CreatedAt, &job.UpdatedAt, &job.CompletedAt,
		); err != nil {
			return nil, err
		}
		jobs = append(jobs, &job)
	}
	return jobs, rows.Err()
}

// AddLog adds a log entry for a job
func (db *DB) AddLog(jobID, level, message string) error {
	_, err := db.conn.Exec(`
		INSERT INTO job_logs (job_id, timestamp, level, message)
		VALUES (?, ?, ?, ?)
	`, jobID, time.Now(), level, message)
	return err
}

// GetLogs retrieves logs for a job
func (db *DB) GetLogs(jobID string, tail int) ([]*JobLog, error) {
	query := "SELECT id, job_id, timestamp, level, message FROM job_logs WHERE job_id = ? ORDER BY timestamp DESC"
	if tail > 0 {
		query += fmt.Sprintf(" LIMIT %d", tail)
	}

	rows, err := db.conn.Query(query, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*JobLog
	for rows.Next() {
		var log JobLog
		if err := rows.Scan(&log.ID, &log.JobID, &log.Timestamp, &log.Level, &log.Message); err != nil {
			return nil, err
		}
		logs = append(logs, &log)
	}

	// Reverse to get chronological order
	for i := 0; i < len(logs)/2; i++ {
		j := len(logs) - 1 - i
		logs[i], logs[j] = logs[j], logs[i]
	}

	return logs, rows.Err()
}
