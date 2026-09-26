# Path allowlist push gate (UTA-103)

When a Linear description or `devbox assign --context` contains an `ALLOWLIST:` block, the host fails the job **before push** if `git diff --name-only origin/<base>...HEAD` contains any path outside that allowlist.

## Syntax

```
ALLOWLIST:
packages/database/src/repositories/
packages/database/src/__tests__/
```

- One path prefix per line (directory or file).
- Prefix match: `packages/database/src/repositories/` allows any file under that directory.
- Blank line or markdown heading ends the block.
- If no `ALLOWLIST:` block is present, the gate is inactive (backward compatible).

## Operator tips

Put the block in the Linear ticket body **and** repeat it in `--context` for assign.
