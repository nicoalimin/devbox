ALTER TABLE jobs ADD COLUMN failure_signature TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN failure_summary TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_jobs_linear_issue_created ON jobs(linear_issue_id, created_at DESC);
