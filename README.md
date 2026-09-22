# Devbox Agent Worktree Cleanup Fix

## Problem
The devbox agent was encountering errors when trying to clean up worktrees during job completion, particularly in distributed environments where multiple agent sessions might be running concurrently on the same workspace. This caused jobs to appear as "complete" even though the cleanup failed, leading to inconsistency.

## Root Cause
Multiple cleanup operations were happening at different stages of job execution:
1. In `completeJob` function when a job successfully completes
2. In `checkpointFailedWork` when a job fails but still needs checkpointing
3. In `CancelJob` when a job is cancelled

The cleanup process would log warnings but not fail the overall job operation, allowing jobs to be marked as complete or successful even when worktree cleanup failed.

## Solution
Improved error handling and diagnostics for worktree cleanup operations:

1. **Enhanced Error Propagation**: Worktree cleanup failures now cause the job completion process to fail rather than just logging a warning
2. **State Consistency**: Jobs will no longer be marked as successful if cleanup fails
3. **Better Diagnostics**: More explicit error messages to identify cleanup issues
4. **Idempotent Cleanup**: Improved checking to handle cases where worktrees may already be removed

## Changes Made
### internal/job/orchestrator.go
- `completeJob`: Changed from warning to failing when worktree removal fails
- `checkpointFailedWork`: Enhanced error handling for both empty and non-empty checkpoint cleanup scenarios  
- `CancelJob`: Improved error propagation for cleanup operations

### internal/git/git.go
- `RemoveWorktree`: Added pre-check for worktree existence to prevent errors on already-removed worktrees

## Impact
This fix ensures that when worktree cleanup fails:
1. The job is not marked as complete/successful
2. Proper error messages are logged for debugging
3. System state remains consistent
4. Admins receive clear indication of cleanup failures for manual intervention if needed