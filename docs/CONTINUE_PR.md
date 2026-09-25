# Continue existing PR — reuse worktree/branch (UTA-96)

When a Linear issue already has an open PR / prior Devbox job, **iterate in place** on the same worktree, branch, and PR. Do **not** mint a new numbered branch from main (`devbox/uta-N-K`) for each continue.

## Operator CLI

```bash
# Preferred: reuse path
devbox assign UTA-N --continue --context-file notes.md

# Or include continue markers in operator context (server also auto-detects
# when a prior job for the issue already has a PR / branch):
devbox assign UTA-N --note "continue_pr: true" --note "push_ref: devbox/uta-n-5"

# After a job finished done/failed with PR still open — same branch/worktree:
devbox review UTA-N --comments-file feedback.md
```

`--continue` injects `continue_pr: true`. The server resolves the branch from `push_ref` or the prior job’s `branch_name` / `pr_url` / `worktree_path`.

## Required / useful `operator_context` keys

```
continue_pr: true
push_ref: devbox/uta-82-32
```

| Key | Role |
|-----|------|
| **`push_ref`** | Existing PR head branch to keep updating. When set, reuse checks out this ref (assigned branch == `push_ref`). |
| **`continue_pr: true`** | Marks continue intent. Alone is enough when a prior job for the issue already recorded `branch_name` / `pr_url`. |

Keys are case-insensitive; `:` or `=` separators; one key per line.

## Host behavior

### Reuse path (UTA-96)

When `push_ref` / `continue_pr` is present **or** a prior job for the issue has an open PR / worktree:

1. Prefer **reattach** to the prior job’s `worktree_path` if it still exists on disk (ensure checkout on the reuse ref).
2. Else reuse any git worktree already registered on that branch.
3. Else `git worktree add` checked out **on** the existing ref (not `origin/main` + new `devbox/uta-N-K`).
4. Persist `worktree_path`, `branch_name` (= reuse ref), and `pr_url` on the job.
5. After `done`/`failed` with PR still open, **preserve** the worktree so `assign --continue` / `review` can reattach. Reconciler removes it when the PR is merged or closed.

Assigned branch equals `push_ref` on the reuse path, so AssertBranch / dual-push complexity collapses (dual-push is a no-op when refs match).

### Legacy align + dual-push (UTA-88)

If a continue job still lands on a *different* assigned branch (older hosts / unusual contexts), the host:

1. `git checkout -B <assigned-branch>` before AssertBranch / format / checkpoint (keeps commits).
2. Pushes the assigned branch, then dual-pushes `HEAD:refs/heads/<push_ref>` so the existing PR tip advances.
3. Prefers the open PR for `head=push_ref` instead of opening a second PR.

Fresh assigns (no continue markers and no prior PR/worktree) still create a new numbered branch from main.

## Review after finished job

`devbox review <job_id|LINEAR_ISSUE_ID>` accepts a Linear id after the prior job is `done`/`failed`, as long as `pr_url` / `branch_name` remain. Missing worktrees are recreated from the branch (or reattached when preserved).

## Validation failures, repeat detection, and `stuck` (UTA-97)

- Validation failures report deduplicated TypeScript errors (first 30 distinct, with total/distinct counts and example locations) instead of a 16KB byte tail. Non-TypeScript output keeps the head plus a short tail. The full raw output is saved under `$TMPDIR/devbox-validation/<signature>.log`, and the job log records that path.
- Each validation failure stores a `failure_signature`: a hash of the sorted, deduped error code + file + message entries, with line numbers, timestamps, and absolute worktree/temp paths removed. The deduped report is stored in `failure_summary`.
- If a job fails with the same signature as the previous job on the same Linear issue (or as its own last failure, for a `review` re-run), it ends in the terminal state **`stuck`** instead of `failed`, and devboxd posts a Linear comment with the deduped errors. `devbox jobs`, `devbox job <id>`, and the TUI show the `stuck` state.
- On `assign --continue` (or a reuse continue) after a failed/stuck job, and on `review` of a job whose last run failed validation, the prompt starts with `Step 1 (mandatory): fix these errors …`, followed by the prior deduped errors.
- Formatter output goes into the agent-work commit (`[ISSUE] <title>`). There are no standalone `chore: format` commits, and a format-only diff is neither committed nor pushed (including by failure checkpoints).
