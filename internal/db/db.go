package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// JobState represents the state of a job
type JobState string

const (
	StateQueued    JobState = "queued"
	StateFetching  JobState = "fetching"
	StatePreparing JobState = "preparing"
	StateCoding    JobState = "coding"
	StateReviewing JobState = "reviewing"
	StatePushing   JobState = "pushing"
	StatePROpen    JobState = "pr_open"
	StateDone      JobState = "done"
	StateBlocked   JobState = "blocked"
	StateFailed    JobState = "failed"
	StateCancelled JobState = "cancelled"
	// StateStuck is a failed job whose validation failure signature matches
	// the previous job on the same Linear issue (UTA-97): rerunning the same
	// path will not help without operator input.
	StateStuck JobState = "stuck"
)

// IsTerminal returns true if the state is terminal (won't transition further)
func (s JobState) IsTerminal() bool {
	return s == StateDone || s == StateFailed || s == StateCancelled || s == StateStuck
}

// IsFailure reports whether the state is a failed terminal state.
func (s JobState) IsFailure() bool {
	return s == StateFailed || s == StateStuck
}

// IsBusy returns true if the state indicates active processing
func (s JobState) IsBusy() bool {
	return s == StateFetching || s == StatePreparing || s == StateCoding ||
		s == StateReviewing || s == StatePushing
}

// Job represents a coding job
type Job struct {
	ID                     string     `json:"id"`
	LinearIssueID          string     `json:"linear_issue_id"`
	LinearURL              string     `json:"linear_url"`
	State                  JobState   `json:"state"`
	RepoPath               string     `json:"repo_path"`
	BranchName             string     `json:"branch_name"`
	WorktreePath           string     `json:"worktree_path"`
	PRURL                  string     `json:"pr_url"`
	BlockerReason          string     `json:"blocker_reason"`
	OpenCodeSessionID      string     `json:"opencode_session_id"`
	OperatorContext        string     `json:"operator_context"`
	ReviewFeedback         string     `json:"review_feedback"`
	CodingWaitStartedAt    *time.Time `json:"coding_wait_started_at"`
	ReviewingWaitStartedAt *time.Time `json:"reviewing_wait_started_at"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	CompletedAt            *time.Time `json:"completed_at"`
	// FailureSignature is a stable hash of the deduped validation failure
	// (empty when the job did not fail validation).
	FailureSignature string `json:"failure_signature,omitempty"`
	// FailureSummary is the deduped validation error report behind
	// FailureSignature, injected into the next continue/review prompt.
	FailureSummary string `json:"failure_summary,omitempty"`
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

// Open opens a database connection and runs migrations
func Open(path string) (*DB, error) {
	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := createDirIfNotExist(dir); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

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

	// Run migrations
	if err := runMigrations(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return &DB{conn: conn}, nil
}

// runMigrations runs database migrations using golang-migrate
func runMigrations(conn *sql.DB) error {
	// Create the migrations source from embedded filesystem
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to get migrations subdirectory: %w", err)
	}

	sourceDriver, err := iofs.New(migrations, ".")
	if err != nil {
		return fmt.Errorf("failed to create migrations source: %w", err)
	}

	// Create the database driver
	dbDriver, err := sqlite3.WithInstance(conn, &sqlite3.Config{})
	if err != nil {
		return fmt.Errorf("failed to create database driver: %w", err)
	}

	// Create the migrator
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "sqlite3", dbDriver)
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	// Run migrations to latest version
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to apply migrations: %w", err)
	}

	return nil
}

// createDirIfNotExist creates a directory if it doesn't exist
func createDirIfNotExist(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.MkdirAll(dir, 0755)
	}
	return nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.conn.Close()
}

// Backup includes committed WAL contents in a standalone SQLite snapshot.
func (db *DB) Backup(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := db.conn.Exec("VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	return os.Chmod(path, 0600)
}

// jobColumns lists job columns in the order scanJob reads them.
const jobColumns = `id, linear_issue_id, linear_url, state, repo_path, branch_name,
			worktree_path, pr_url, blocker_reason, opencode_session_id,
			operator_context, review_feedback,
			coding_wait_started_at, reviewing_wait_started_at,
			failure_signature, failure_summary,
			created_at, updated_at, completed_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (*Job, error) {
	var job Job
	if err := row.Scan(
		&job.ID, &job.LinearIssueID, &job.LinearURL, &job.State, &job.RepoPath,
		&job.BranchName, &job.WorktreePath, &job.PRURL, &job.BlockerReason,
		&job.OpenCodeSessionID, &job.OperatorContext, &job.ReviewFeedback,
		&job.CodingWaitStartedAt, &job.ReviewingWaitStartedAt,
		&job.FailureSignature, &job.FailureSummary,
		&job.CreatedAt, &job.UpdatedAt, &job.CompletedAt,
	); err != nil {
		return nil, err
	}
	return &job, nil
}

func (db *DB) queryJob(query string, args ...any) (*Job, error) {
	job, err := scanJob(db.conn.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return job, err
}

func (db *DB) queryJobs(query string, args ...any) ([]*Job, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// CreateJob creates a new job
func (db *DB) CreateJob(job *Job) error {
	_, err := db.conn.Exec(`
		INSERT INTO jobs (`+jobColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, job.ID, job.LinearIssueID, job.LinearURL, job.State, job.RepoPath,
		job.BranchName, job.WorktreePath, job.PRURL, job.BlockerReason,
		job.OpenCodeSessionID, job.OperatorContext, job.ReviewFeedback,
		job.CodingWaitStartedAt, job.ReviewingWaitStartedAt,
		job.FailureSignature, job.FailureSummary,
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
			failure_signature = ?, failure_summary = ?,
			updated_at = ?, completed_at = ?
		WHERE id = ?
	`, job.State, job.RepoPath, job.BranchName, job.WorktreePath,
		job.PRURL, job.BlockerReason, job.OpenCodeSessionID,
		job.OperatorContext, job.ReviewFeedback,
		job.CodingWaitStartedAt, job.ReviewingWaitStartedAt,
		job.FailureSignature, job.FailureSummary,
		job.UpdatedAt, job.CompletedAt, job.ID)
	return err
}

// GetJob retrieves a job by ID
func (db *DB) GetJob(id string) (*Job, error) {
	return db.queryJob(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
}

// ListJobs lists jobs with optional limit
func (db *DB) ListJobs(limit int) ([]*Job, error) {
	query := "SELECT " + jobColumns + " FROM jobs ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	return db.queryJobs(query)
}

// GetCurrentJob returns the currently busy job (if any)
func (db *DB) GetCurrentJob() (*Job, error) {
	return db.queryJob(`
		SELECT ` + jobColumns + `
		FROM jobs
		WHERE state IN ('fetching', 'preparing', 'coding', 'reviewing', 'pushing')
		ORDER BY created_at DESC
		LIMIT 1
	`)
}

// GetJobByLinearIssueID retrieves a job by Linear issue ID (most recent first)
func (db *DB) GetJobByLinearIssueID(linearIssueID string) (*Job, error) {
	return db.queryJob(`
		SELECT `+jobColumns+`
		FROM jobs
		WHERE linear_issue_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, linearIssueID)
}

// GetPreviousJob returns the most recent job for a Linear issue other than
// excludeJobID that reached a resting state (failed, stuck, done, blocked,
// pr_open). Cancelled and in-flight jobs are skipped so a cancel between two
// identical failures does not reset repeat detection (UTA-97/UTA-98).
func (db *DB) GetPreviousJob(linearIssueID, excludeJobID string) (*Job, error) {
	return db.queryJob(`
		SELECT `+jobColumns+`
		FROM jobs
		WHERE linear_issue_id = ? AND id != ?
			AND state IN ('failed', 'stuck', 'done', 'blocked', 'pr_open')
		ORDER BY created_at DESC
		LIMIT 1
	`, linearIssueID, excludeJobID)
}

// GetPriorReusableJob returns the most recent prior job for a Linear issue that
// still has a branch (and preferably a PR or worktree) so a continue/reuse assign
// can reattach instead of creating a new numbered branch from main.
// excludeJobID skips the newly created successor job.
func (db *DB) GetPriorReusableJob(linearIssueID, excludeJobID string) (*Job, error) {
	jobs, err := db.queryJobs(`
		SELECT `+jobColumns+`
		FROM jobs
		WHERE linear_issue_id = ?
			AND id != ?
			AND branch_name != ''
			AND (pr_url != '' OR worktree_path != '')
		ORDER BY created_at DESC
		LIMIT 5
	`, linearIssueID, excludeJobID)
	if err != nil {
		return nil, err
	}
	// Prefer a job that already has a PR URL.
	for _, job := range jobs {
		if job.PRURL != "" {
			return job, nil
		}
	}
	if len(jobs) > 0 {
		return jobs[0], nil
	}
	return nil, nil
}

// GetBlockedJobs returns jobs in blocked state
func (db *DB) GetBlockedJobs() ([]*Job, error) {
	return db.queryJobs(`
		SELECT ` + jobColumns + `
		FROM jobs
		WHERE state = 'blocked'
		ORDER BY created_at DESC
	`)
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
