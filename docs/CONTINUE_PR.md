# Continue existing PR (operator_context)

TokoBoss / CoS continue jobs must update an **existing** PR tip (for example `#34` on `devbox/uta-82-32`) while the host still assigns a **new** worktree branch from main (e.g. `devbox/uta-82-46`).

Without continue hints, OpenCode may `git reset --hard origin/<existing-pr-branch>`, then host delivery fails:

> worktree must be on assigned branch "devbox/uta-82-46" (found "devbox/uta-82-32")

## Required `operator_context` keys

Include these keys in free-form operator context (case-insensitive; `:` or `=`):

```
continue_pr: true
push_ref: devbox/uta-82-32
```

- **`push_ref`**: existing PR head branch to keep updated. **Required** to activate continue delivery.
- **`continue_pr: true`**: optional marker; alone does not activate align/dual-push.

CoS **must** include `push_ref` when asking OpenCode to reset onto an existing PR tip.

## Host behavior (UTA-88)

1. Before `AssertBranch` / format / checkpoint: `git checkout -B <assigned-branch>` (keeps commits; renames local branch).
2. Push assigned branch as usual, then dual-push `HEAD:refs/heads/<push_ref>` so the existing PR tip advances.
3. Prefer resolving the open PR for `head=push_ref` instead of opening a second PR from the assigned branch alone. If none exists, fall back to normal `gh pr create`.

Non-continue jobs keep strict assigned-branch checks unchanged.
