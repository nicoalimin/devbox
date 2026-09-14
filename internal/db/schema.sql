CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    linear_issue_id TEXT NOT NULL,
    linear_url TEXT,
    state TEXT NOT NULL,
    repo_path TEXT,
    branch_name TEXT,
    worktree_path TEXT,
    pr_url TEXT,
    blocker_reason TEXT,
    opencode_session_id TEXT,
    operator_context TEXT,
    review_feedback TEXT,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at DESC);

CREATE TABLE IF NOT EXISTS job_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL,
    timestamp TIMESTAMP NOT NULL,
    level TEXT NOT NULL,
    message TEXT NOT NULL,
    FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_job_logs_job_id ON job_logs(job_id);
CREATE INDEX IF NOT EXISTS idx_job_logs_timestamp ON job_logs(timestamp DESC);
