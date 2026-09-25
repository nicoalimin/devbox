DROP INDEX IF EXISTS idx_jobs_linear_issue_created;
ALTER TABLE jobs DROP COLUMN failure_summary;
ALTER TABLE jobs DROP COLUMN failure_signature;
