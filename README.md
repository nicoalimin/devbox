# Devbox: OpenCode Orchestration System

Devbox is a client-server system that orchestrates [OpenCode](https://github.com/nicoalimin/opencode) coding agents through Grok Bot (or any other orchestrator). It provides a clean separation between orchestration logic and code execution, enabling Grok Bot to manage Linear issues by delegating implementation work to a dedicated coding host.

## Architecture

```
┌─────────────┐
│  Grok Bot   │ (orchestrator - decides when to assign, checks status, unblocks)
└──────┬──────┘
       │ shell invokes
       v
┌─────────────┐
│   devbox    │ (CLIENT binary - thin CLI wrapper)
│  (CLI)      │
└──────┬──────┘
       │ authenticated HTTPS
       v
┌─────────────┐
│  devboxd    │ (SERVER daemon on coding host)
│  (daemon)   │
└──────┬──────┘
       │
       ├──> Linear GraphQL API (fetch/update issues)
       ├──> Local git (worktree management)
       ├──> OpenCode HTTP API (coding sessions)
       └──> GitHub (gh CLI for PRs)
```

**Key Features:**
- **Single-flight execution**: Only one job runs at a time (configurable queueing)
- **Git worktree isolation**: Each job gets its own isolated worktree
- **Persistent state**: SQLite database survives restarts
- **Rich logging**: Track every step of job execution
- **Blocker support**: Jobs can pause for human clarification

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed technical design.

## Quick Start

### Prerequisites

**On the coding host** (where code changes will be made):
- Go 1.21+ (for building)
- Git with configured credentials
- [GitHub CLI (`gh`)](https://cli.github.com/) authenticated
- [OpenCode](https://github.com/nicoalimin/opencode) running locally
- Linear API key

**On the Grok Bot host** (or wherever you want to invoke commands):
- The `devbox` client binary

### 1. Build the Binaries

```bash
git clone https://github.com/nicoalimin/devbox
cd devbox
make build
```

This creates:
- `bin/devboxd` - Server daemon (install on coding host)
- `bin/devbox` - Client CLI (install on Grok Bot host)

### 2. Install the Server (Coding Host)

```bash
# Copy binary
sudo cp bin/devboxd /usr/local/bin/

# Create configuration
cp devboxd.yaml.example devboxd.yaml
# Edit devboxd.yaml with your settings (see Configuration section)

# Run the server (with interactive TUI)
devboxd --config devboxd.yaml
```

The server will:
- Listen on `0.0.0.0:8080` (configurable)
- Create a persistent SQLite database (default: `~/.local/share/devbox/jobs.db`)
- Display an interactive terminal UI dashboard by default
- Wait for job assignments via the HTTP API

**Full-Screen Interactive TUI**

When you run `devboxd --config devboxd.yaml`, it launches an immersive full-screen interface:

```
┌─────────────────────────────────────────────────────────────────┐
│ devboxd v0.1.0 │ BUSY │ Job: ENG-123 (coding) │ 2m15s          │ ← Status Bar
├──────────────────────┬──────────────────────────────────────────┤
│ JOBS (12)            │ LIVE SERVER LOGS                         │
│ ┌──────────────────┐ │ ┌────────────────────────────────────┐  │
│ │ ▸ ENG-123 coding │ │ │ [15:04:03] [INFO] Server started   │  │
│ │   ENG-124 done   │ │ │ [15:04:04] [INFO] Job assigned     │  │ ← Server Logs
│ │   ENG-125 failed │ │ │ [15:04:05] [INFO] HTTP access log  │  │   (daemon logs)
│ │   ...            │ │ └────────────────────────────────────┘  │
│ └──────────────────┘ │                                          │
│                      │ JOB LOGS                                 │
│ ERRORS (2)           │ ┌────────────────────────────────────┐  │
│ ┌──────────────────┐ │ │ [15:04:03] [INFO] Starting job     │  │
│ │ • ENG-120        │ │ │ [15:04:04] [INFO] Fetching Linear  │  │ ← Job Logs
│ │   needs input    │ │ │ [15:04:05] [INFO] Creating branch  │  │   (selected job)
│ └──────────────────┘ │ │ ...                                │  │
│                      │ └────────────────────────────────────┘  │
│ INTEGRATIONS         │                                          │
│ ✓ Linear ✓ GitHub   │                                          │
│ ✓ OpenCode          │                                          │
├──────────────────────┴──────────────────────────────────────────┤
│ Focus: Jobs | Tab: switch | ↑↓/jk: nav | r: refresh | q: quit  │ ← Help Bar
└─────────────────────────────────────────────────────────────────┘
```

**Layout:**
- **Header**: Real-time status (BUSY/IDLE), current job, elapsed time
- **Sidebar** (left):
  - **Jobs**: Recent job list with states (done/busy/failed/blocked)
  - **Errors**: Blocked jobs requiring attention
  - **Integrations**: Health of Linear, GitHub, OpenCode, configured repos
- **Main Pane** (right): Split log view
  - **Live Server Logs**: Daemon orchestration logs (HTTP access, job lifecycle)
  - **Job Logs**: Logs for selected job (or current active job)
- **Footer**: Keybindings help

**TUI Keybindings:**
- `Tab` / `Shift+Tab`: Switch focus between panes (Jobs → Server Logs → Job Logs → Integrations)
- `↑` `↓` or `j` `k`: Navigate within focused pane (in Jobs pane, updates job logs for selected job)
- `g` / `G`: Jump to top/bottom
- `Enter`: Switch to Job Logs pane (when in Jobs pane)
- `r`: Force refresh data
- `q` or `Ctrl+C`: Quit and restore terminal

**Features:**
- **Full-screen immersive**: Uses alternate screen buffer (like vim/htop)
- **Vim-like navigation**: j/k, arrow keys, g/G for movement
- **Focus highlighting**: Active pane has colored border
- **Auto-refresh**: Updates every 2 seconds
- **Scrollable logs**: Navigate through job history with viewport
- **Color-coded states**: Green (done), red (failed), yellow (blocked), blue (active)
- **Responsive**: Handles terminal resize gracefully

**TUI Review Requirements**

When making changes to the TUI (files in `internal/tui/`), verify the following before submitting:

- [ ] **Full-screen fit**: The entire dashboard (header + both columns + footer) fits within **one terminal screen** with no overflow or clipping
  - Test with various terminal sizes (minimum: 80x24, recommended: 120x30+)
  - Both columns (left sidebar and right logs) must have the **same total height**
  - Resize the terminal to confirm no content is pushed above the visible area
- [ ] **Layout balance**: Left sidebar height (jobs + errors + integrations) equals right column height (server logs + job logs)
- [ ] **Headless mode**: `--no-tui` flag still works correctly for non-interactive environments
- [ ] **Navigation**: Independent scrolling in server logs and job logs panes still functions
- [ ] **Job selection**: Changing selected job in jobs pane updates job logs pane correctly

These requirements ensure a professional, immersive TUI experience that fits standard terminal environments.

**Headless Mode**

To run without the TUI (e.g., in Docker, systemd, or CI):

```bash
# Option 1: --no-tui flag
devboxd --config devboxd.yaml --no-tui

# Option 2: Environment variable
DEVBOX_NO_TUI=1 devboxd --config devboxd.yaml

```

The server disables the TUI only when:
- `--no-tui` flag is provided
- `DEVBOX_NO_TUI` environment variable is set

Redirecting output does not implicitly select headless mode; services and scripts
must use one of the explicit options above.

### 3. Install the Client (Grok Bot Host)

```bash
# Copy binary
sudo cp bin/devbox /usr/local/bin/

# Set environment variables
export DEVBOX_SERVER_URL="https://your-coding-host:8080"
export DEVBOX_TOKEN="your-secret-token"

# Test connectivity
devbox health
```

### 4. Assign Your First Job

```bash
# Assign a Linear issue to devboxd
devbox assign ENG-123

# Check status
devbox status

# View job details
devbox jobs
```

The server will:
1. Fetch the Linear issue details
2. Create a git worktree and branch
3. Send the issue to OpenCode for implementation
4. Run a code review pass
5. Push the branch and create a PR
6. Update Linear with the PR link

## Configuration

### Server Configuration (`devboxd.yaml`)

```yaml
server:
  listen: "0.0.0.0:8080"
  # auth_token: "your-secret-token"  # or set DEVBOXD_AUTH_TOKEN env var

database:
  # path: ""  # Path to SQLite database file
              # Default: ~/.local/share/devbox/jobs.db (or $XDG_DATA_HOME/devbox/jobs.db)
              # Can also set DEVBOXD_DB_PATH env var or use --db CLI flag

linear:
  # api_key: "lin_api_your_key_here"     # or set LINEAR_API_KEY env var
  assignee_id: ""                         # Optional: only process issues assigned to this user
  workspace_id: ""                        # Optional: workspace ID

opencode:
  base_url: "http://127.0.0.1:3000"
  timeout: "30m"                          # Coding session timeout
  review_timeout: "15m"                   # Interrupt a stuck review, then validate/deliver safely

  # HTTP Basic Authentication (OpenCode 2.0.3+)
  # When OpenCode server is started with OPENCODE_SERVER_PASSWORD set,
  # all API requests require HTTP Basic Auth.
  username: "opencode"                    # or set DEVBOXD_OPENCODE_USERNAME / OPENCODE_SERVER_USERNAME
  password: "your-password-here"          # or set DEVBOXD_OPENCODE_PASSWORD / OPENCODE_SERVER_PASSWORD

github:
  default_base_branch: "main"

# Map Linear issue attributes to repository paths
repos:
  - match:
      team: "ENG"
    repo:
      path: "/home/dev/my-backend"
      base_branch: "main"
      # Omit to auto-detect common Go and Node CI checks. Set [] to disable.
      validation_commands:
        - "go test ./..."
        - "go vet ./..."
  
  - match:
      project: "Frontend Redesign"
    repo:
      path: "/home/dev/my-frontend"
      base_branch: "develop"
  
  - match:
      label: "infrastructure"
    repo:
      path: "/home/dev/infra"
      base_branch: "main"

queue:
  enabled: false  # Default: reject when busy
  max_depth: 1    # Only if enabled

reconciler:
  enabled: true   # Periodic GitHub check for stuck pr_open jobs (merged -> done, closed -> cancelled)
  interval: "2m"  # Poll interval (~1-5m recommended)
```

Before pushing and opening a pull request, Devbox runs the configured
`validation_commands`. When the field is omitted, it auto-detects common Go
checks (`go test ./...`, `go vet ./...`) and Node package-manager checks
(frozen install plus available `format:check`, `lint`, `typecheck`, `test`, and
`build` scripts). Use `validation_commands: []` only when validation should be
explicitly disabled for that repository.

The same host delivery gate runs for every review iteration. Devbox runs
write-mode formatting before checks, asks OpenCode to repair failures up to
three times, and reruns the checks even when a repair times out (after stopping
the session and confirming it is idle). It commits all tracked and non-ignored
untracked changes, pushes the assigned branch without force, and verifies that
the actual remote SHA matches local HEAD and the worktree is clean. Pushes are
retried up to three times. The configured repository base branch is used.

Formatting and Node checks are detected in nested Git-visible packages such as
`web/` as well as the root. Formatting uses `format:write`, `format:fix`, or
`format`; a simple `prettier --check ...` script can supply the write command
when none is declared. Use `format_commands` for other tools or layouts (commands
run from the worktree root), or `format_commands: []` to disable formatting.
For example:

```yaml
format_commands:
  - "cd web && pnpm install --frozen-lockfile && pnpm exec prettier --write ."
validation_commands:
  - "cd web && pnpm run format:check && pnpm run lint && pnpm run test"
```

If coding or review still fails, Devbox attempts to stop OpenCode, commit an
**incomplete checkpoint**, and push it before marking the job failed. A failed
check never becomes a successful ticket merely because its branch was pushed.
Cleanup occurs only after checkpoint publication is verified; if interruption,
commit hooks, credentials, or remote rejection prevent recovery, the worktree is
preserved and `blocker_reason` records the failure and recovery path. Cleanup
never force-removes a dirty worktree.

Review requests are saved before HTTP `202 Accepted` is returned. Duplicate or
concurrent reviews are rejected, background errors are persisted, and accepted
reviews resume after restart. The response includes the canonical `jobId`.
`devbox review TEST-123 --comments "Address feedback" --wait` waits for verified
delivery and exits with an error on failure. Without `--wait`, the CLI reports
`accepted`, not completion. `--wait-timeout` limits the client's wait only; the
server continues processing.

**Environment Variable Overrides:**
- `DEVBOXD_AUTH_TOKEN` - Server authentication token
- `DEVBOXD_DB_PATH` - Database file path
- `LINEAR_API_KEY` - Linear API key
- `LINEAR_ASSIGNEE_ID` - Linear user ID filter
- `OPENCODE_BASE_URL` - OpenCode server URL
- `DEVBOXD_OPENCODE_USERNAME` or `OPENCODE_SERVER_USERNAME` - OpenCode HTTP Basic Auth username
- `DEVBOXD_OPENCODE_PASSWORD` or `OPENCODE_SERVER_PASSWORD` - OpenCode HTTP Basic Auth password

### Client Configuration

The client is configured via environment variables:

```bash
export DEVBOX_SERVER_URL="https://your-coding-host:8080"
export DEVBOX_TOKEN="your-secret-token"
```

Or pass as flags:
```bash
devbox --server https://your-coding-host:8080 --token your-secret-token status
```

## Usage

### CLI Commands

#### Health Check
```bash
devbox health
# Output: ✓ Server is healthy (version 0.1.0)
```

#### Get Status
```bash
devbox status
# Shows if server is busy and current job
```

#### Assign a Job
```bash
devbox assign ENG-123
# Creates a job for Linear issue ENG-123
```

#### List Jobs
```bash
devbox jobs --limit 20
# Lists recent jobs with their states
```

#### Get Job Details
```bash
devbox job <job-id>
# Shows full details for a specific job
```

#### List Blockers
```bash
devbox blockers
# Shows jobs waiting for human input
```

#### Reply to Blocked Job
```bash
devbox reply <job-id> "Use the blue button color from the design system"
# Sends clarification to resume a blocked job
```

#### Cancel a Job
```bash
devbox cancel <job-id>
# Cancels a running or blocked job
```

#### View Logs
```bash
devbox logs <job-id> --tail 50
# Shows last 50 log entries for a job
```

### JSON Output

Add `--json` to any command for machine-readable output:

```bash
devbox status --json
devbox jobs --json
devbox job <job-id> --json
```

## Job Lifecycle

### Job States

- `queued` - Job accepted but waiting (if queueing enabled)
- `fetching` - Fetching Linear issue details
- `preparing` - Setting up git worktree
- `coding` - OpenCode is implementing
- `reviewing` - OpenCode is code reviewing
- `pushing` - Pushing branch to remote
- `pr_open` - Opening pull request
- `done` - Successfully completed
- `blocked` - Needs human input/clarification
- `failed` - Encountered unrecoverable error
- `cancelled` - User/system cancelled

### State Transitions

```
queued -> fetching -> preparing -> coding -> reviewing -> pushing -> pr_open -> done
   |         |            |           |          |           |          |
   |         v            v           v          v           v          v
   +-------> failed    failed      blocked    failed     failed    failed
             
blocked -> (reply received) -> coding/reviewing (resume)
```

## Integration with Grok Bot

Grok Bot (or any orchestrator) interacts with devboxd through the `devbox` CLI:

### Example Grok Bot Workflow

1. **Grok receives user request**: "Please implement ENG-123"
2. **Grok checks server status**: `devbox status --json`
3. **If idle, assign the job**: `devbox assign ENG-123`
4. **Periodically check progress**: `devbox job <job-id> --json`
5. **If blocked**: `devbox blockers --json` → Ask user for clarification → `devbox reply <job-id> "<answer>"`
6. **When done**: Report PR URL to user

### Example Grok Bot Script

```bash
#!/bin/bash
# Simple script Grok Bot can invoke

LINEAR_ISSUE="$1"

# Check if server is busy
STATUS=$(devbox status --json)
if echo "$STATUS" | jq -e '.busy == true' > /dev/null; then
  echo "Server is busy with another job"
  exit 1
fi

# Assign the job
JOB=$(devbox assign "$LINEAR_ISSUE" --json)
JOB_ID=$(echo "$JOB" | jq -r '.id')

echo "Job $JOB_ID created for $LINEAR_ISSUE"
echo "Monitor with: devbox job $JOB_ID"
```

## OpenCode Integration

Devboxd integrates with [OpenCode](https://github.com/nicoalimin/opencode) via its HTTP API. When a job is assigned:

1. **Session Creation**: `POST /api/session` with job name
2. **Task Prompt**: `POST /api/session/:id/prompt` with issue details
3. **Review Prompt**: Another message for code review

### OpenCode Telemetry

Devboxd automatically logs detailed telemetry for OpenCode operations to help diagnose issues:

**Session Events:**
- Session create (ID, status, response code)
- Prompt send (phase coding/review, message size, hash, response code)
- Event stream lifecycle (connect, disconnect, heartbeat every 30s when quiet)
- Mapped OpenCode events (session status changes, tool calls, text generation, errors)
- Stale-active warnings (when `/api/session/active` shows active but no progress for 60s)

All telemetry is automatically logged to:
- **Server logs** (OpenCode HTTP operations, event stream status)
- **Job logs** (OpenCode session activity, progress updates)

View these logs in the TUI or via `devbox logs <job-id>`.

### Enabling OpenCode Debug Logs

For deeper debugging of OpenCode itself (not just devboxd's integration), enable OpenCode's debug logging:

**Method 1: Environment Variable (recommended for `opencode serve`)**
```bash
export OPENCODE_LOG_LEVEL=DEBUG
opencode serve
```

**Method 2: CLI Flag**
```bash
opencode serve --log-level DEBUG --print-logs
```

**Method 3: Config File (`~/.config/opencode/opencode.json`)**
```json
{
  "logLevel": "DEBUG"
}
```

**OpenCode Log Files:**
- **macOS/Linux**: `~/.local/share/opencode/log/`
- **Windows**: `%USERPROFILE%\.local\share\opencode\log`

Logs are timestamped (e.g., `2026-01-15T123456.log`) and the most recent 10 are kept.

**Advanced: Streaming OpenCode Logs to Devbox**

If you want OpenCode's debug logs to appear in devbox job logs:

1. Start OpenCode with `--print-logs` to mirror logs to stderr:
   ```bash
   OPENCODE_LOG_LEVEL=DEBUG opencode serve --print-logs 2>&1 | tee opencode.log
   ```

2. Tail the log file from within devbox workflows (future enhancement)

For session-specific history (prompts, responses, token usage):
```bash
opencode export <session-id>  # Export full session data
opencode stats                 # View token usage and costs
```

### OpenCode 2.0.3+ Authentication

OpenCode 2.0.3 and later require **HTTP Basic Authentication** when `OPENCODE_SERVER_PASSWORD` is set on the OpenCode server. Devboxd automatically sends credentials with every request.

**Configuration options:**

1. **Config file** (`devboxd.yaml`):
```yaml
opencode:
  base_url: "http://127.0.0.1:3000"
  username: "opencode"           # Default username
  password: "your-password-here" # Match OPENCODE_SERVER_PASSWORD
```

2. **Environment variables** (higher priority):
```bash
# Option 1: Use DEVBOXD_ prefix (recommended)
export DEVBOXD_OPENCODE_USERNAME="opencode"
export DEVBOXD_OPENCODE_PASSWORD="your-password-here"

# Option 2: Use OPENCODE_SERVER_ variables (matches OpenCode conventions)
export OPENCODE_SERVER_USERNAME="opencode"
export OPENCODE_SERVER_PASSWORD="your-password-here"
```

**Without authentication**, all OpenCode API requests will fail with `401 Unauthorized`.

### OpenCode Prompt Format

The server sends a structured prompt to OpenCode:

```
Build the following Linear issue:

Identifier: ENG-123
Title: Add CSV export for inventory
URL: https://linear.app/company/issue/ENG-123
Team: Engineering (ENG)
Priority: High

Description:
Users need to export inventory data as CSV...

Instructions:
1. Review the .opencode instructions in this repository
2. Search Notion for related PRDs, specs, or context
3. Implement the required changes
4. Run tests and ensure code quality
5. Commit your changes with clear messages
```

### Existing .opencode Instructions

The repository includes comprehensive OpenCode agent instructions in `.opencode/AGENTS.md`:
- How to search Notion for product context
- When to ask for clarification vs. implementing
- How to update Linear status during work
- How to leave PR comments in Linear

These instructions are preserved and continue to guide OpenCode sessions started by devboxd.

## Network & Security

### Recommended Setup

For production use, we recommend:

1. **Private Network**: Use [Tailscale](https://tailscale.com/) or a VPN to connect Grok Bot host to coding host
2. **HTTPS**: Use a reverse proxy (nginx, Caddy) with TLS certificates
3. **Firewall**: Restrict devboxd port to only trusted IPs

### Simple Tailscale Setup

```bash
# On both hosts
curl -fsSL https://tailscale.com/install.sh | sh
tailscale up

# On coding host
# Note your Tailscale IP (e.g., 100.x.y.z)
devboxd --config devboxd.yaml

# On Grok Bot host
export DEVBOX_SERVER_URL="http://100.x.y.z:8080"
devbox health
```

### Token Generation

Generate a strong authentication token:

```bash
openssl rand -hex 32
```

Add to `devboxd.yaml`:
```yaml
server:
  auth_token: "your-generated-token-here"
```

Or set via environment:
```bash
export DEVBOXD_AUTH_TOKEN="your-generated-token-here"
```

## Systemd Service (Optional)

For production deployment, run devboxd as a systemd service:

```ini
# /etc/systemd/system/devboxd.service
[Unit]
Description=Devbox Daemon
After=network.target

[Service]
Type=simple
User=devbox
WorkingDirectory=/home/devbox
ExecStart=/usr/local/bin/devboxd --config /etc/devboxd/config.yaml --db /var/lib/devboxd/devboxd.db --no-tui
Restart=always
RestartSec=10

# Environment variables
Environment="LINEAR_API_KEY=lin_api_..."
Environment="DEVBOXD_AUTH_TOKEN=..."

[Install]
WantedBy=multi-user.target
```

**Note:** The `--no-tui` flag disables the interactive dashboard for headless operation.

Enable and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable devboxd
sudo systemctl start devboxd
sudo systemctl status devboxd

# View logs
sudo journalctl -u devboxd -f
```

## Development

### Building from Source

```bash
# Clone the repository
git clone https://github.com/nicoalimin/devbox
cd devbox

# Build binaries
make build

# Run tests
make test

# Format code
make fmt
```

### Project Structure

```
devbox/
├── cmd/
│   ├── devbox/          # Client CLI
│   └── devboxd/         # Server daemon
├── internal/
│   ├── api/             # HTTP API server
│   ├── config/          # Configuration management
│   ├── db/              # SQLite database layer
│   ├── git/             # Git worktree management
│   ├── job/             # Job orchestration
│   ├── linear/          # Linear API client
│   └── opencode/        # OpenCode API client
├── pkg/
│   └── client/          # API client library
├── .opencode/           # OpenCode agent instructions
├── ARCHITECTURE.md      # Detailed architecture
├── README.md            # This file
├── Makefile             # Build commands
└── devboxd.yaml.example # Example configuration
```

### Cross-Compilation

Build for multiple platforms:

```bash
make build-all
# Creates binaries for linux/amd64, darwin/amd64, darwin/arm64
```

## Database & Persistence

### Storage Location

Devboxd uses SQLite for persistent job storage. The database survives server restarts and contains:
- All job records (current, completed, failed, blocked)
- Job metadata (Linear issue, PR URL, branch, worktree path, OpenCode session)
- Operator context and review feedback
- Complete job logs

**Default location:**
- `.devbox/jobs.db` (relative to current working directory)

**⚠️ Breaking Change from v0.1.0:**
Previous versions used `~/.local/share/devbox/jobs.db` (XDG convention). The new default is `.devbox/jobs.db` in the repository directory.

**Migration Notes:**
- Old data in `~/.local/share/devbox/jobs.db` is **not automatically migrated**
- The database schema uses golang-migrate for version management
- If you want to keep using the old location, set `database.path` in your config

**Override options:**
1. **Config file**: Set `database.path` in `devboxd.yaml`
2. **Environment**: Set `DEVBOXD_DB_PATH`
3. **CLI flag**: Use `--db /path/to/database.db`

Priority: CLI flag > Environment variable > Config file > Default

### After Restart

When devboxd restarts:
- ✅ All jobs are preserved with full state
- ✅ `devbox jobs` shows historical jobs
- ✅ `devbox review <linear-id>` can resume PR reviews
- ✅ Blocked jobs remain in the queue
- ⚠️ Active OpenCode sessions are not automatically resumed (mark job as blocked)

### Backup & Migration

The database is a single SQLite file. To backup or migrate:

```bash
# Backup
cp .devbox/jobs.db ~/backups/jobs-$(date +%Y%m%d).db

# Restore
cp ~/backups/jobs-20240115.db .devbox/jobs.db

# Migrate to new location
mv .devbox/jobs.db /var/lib/devboxd/jobs.db
# Update devboxd.yaml:
#   database:
#     path: "/var/lib/devboxd/jobs.db"

# To use old XDG location (not recommended):
# Update devboxd.yaml:
#   database:
#     path: "~/.local/share/devbox/jobs.db"
```

## Troubleshooting

### Server Won't Start

**Error**: `failed to load config`
- Check that `devboxd.yaml` exists and is valid YAML
- Ensure required fields are set (auth_token, linear.api_key, repos)

**Error**: `failed to open database`
- Check file permissions on the database path
- The default database location is `.devbox/jobs.db` (relative to current working directory)
- You can override it with the `--db` flag or `database.path` in the config file
- The parent directory is automatically created, but ensure you have write permissions

### Client Can't Connect

**Error**: `request failed: connection refused`
- Verify devboxd is running: `systemctl status devboxd`
- Check server listen address in config
- Test with curl: `curl http://your-server:8080/health`

**Error**: `API error (status 401)`
- Verify your token matches the server's `auth_token`
- Check for typos in `DEVBOX_TOKEN` env var

### Job Fails in `preparing` State

**Error**: `no repository configured`
- Check your `repos` mapping in `devboxd.yaml`
- Ensure at least one repo matches the issue's team/project/label

**Error**: `failed to create worktree`
- Verify git is installed and configured
- Check that the repo path exists and is a valid git repository
- Ensure remote is configured: `git remote -v`

### Job Fails in `pushing` State

**Error**: `failed to push branch`
- Verify git credentials are configured
- Test manual push: `cd /path/to/repo && git push`
- Check for branch protection rules

### Job Fails in `pr_open` State

**Error**: `gh: command not found`
- Install GitHub CLI: https://cli.github.com/
- Authenticate: `gh auth login`

**Error**: `failed to create PR`
- Verify gh CLI is authenticated: `gh auth status`
- Check repository permissions

### OpenCode Issues

**Error**: `request failed` when contacting OpenCode
- Verify OpenCode is running: `curl http://127.0.0.1:3000/global/health`
- Check `opencode.base_url` in config matches your OpenCode server

## Migration from n8n

This repository previously used an n8n workflow for Linear→OpenCode integration. That workflow is preserved in `n8n-workflow.json` as **legacy optional** documentation.

### Key Differences

| Aspect | n8n Workflow (Old) | devbox (New) |
|--------|-------------------|--------------|
| Trigger | Polling (every 5 minutes) | Explicit assignment via CLI |
| Orchestrator | n8n scheduler | Grok Bot (or any orchestrator) |
| State | Stateless (in-flight only) | Persistent (SQLite) |
| Concurrency | Single-flight via busy check | Enforced single-flight + queueing |
| Blocking | Not supported | Blocked state + reply mechanism |
| Network | Direct OpenCode access | Client-server architecture |

### Why the Change?

The n8n approach worked but had limitations:
- **No visibility**: Jobs disappeared between polls
- **No recovery**: Restarts lost in-flight work
- **No blocking**: Couldn't pause for clarification
- **Tight coupling**: Grok Bot and OpenCode on same network

Devboxd addresses these with a proper client-server architecture.

## FAQ

**Q: Can I run multiple jobs concurrently?**  
A: Not yet. Currently enforced single-flight. Future enhancement planned for per-repo concurrency.

**Q: What happens if devboxd crashes?**  
A: Jobs are persisted in SQLite. On restart, you can check job status and manually retry if needed.

**Q: Can I use this without Grok Bot?**  
A: Yes! Any system can use the `devbox` CLI. It's just a thin HTTP API wrapper.

**Q: Does this work with OpenCode forks?**  
A: Yes, as long as the HTTP API endpoints match. See ARCHITECTURE.md for assumed API shape.

**Q: Can I use GitLab instead of GitHub?**  
A: Not yet. PR creation uses `gh` CLI. You'd need to modify `internal/git/git.go` to use `glab` or the GitLab API.

**Q: How do I update Linear status automatically?**  
A: The server can call Linear's GraphQL API to update issue state. Future enhancement to make this configurable.

## Self-upgrade (macOS and Linux)

After a reviewed harness PR is merged to `nicoalimin/devbox` main, an operator
or automation can run `devbox upgrade`. The authenticated RPC builds the
configured remote branch and restarts the server; it does not merge PRs itself.

Enable it in the host's `devboxd.yaml`:

```yaml
upgrade:
  enabled: true
  source_path: /Users/nicoalimin/code/devbox
  remote: origin
  branch: main
  build_timeout: 20m
  drain_timeout: 1h
```

The service account needs Git access to that trusted remote, Go (and the C
toolchain required by SQLite), and write access to the installed binary
directory and database directory. The configured checkout may contain other
branches or dirty files: upgrades fetch into an isolated temporary checkout and
never pull/reset the host checkout. Both `devboxd` and the sibling `devbox`
executable are installed atomically. Run from a compiled, installed binary,
not `go run`. No sudo or privilege escalation is performed by the upgrader.

The first deployment of this feature needs the normal bootstrap once:

```sh
go build -o bin/devboxd ./cmd/devboxd
go build -o bin/devbox ./cmd/devbox
# Stop the old daemon only when its jobs have finished, then start:
./bin/devboxd --config devboxd.yaml
```

Subsequent upgrades use the running server:

```sh
devbox upgrade                       # Wait through restart and verify health
devbox upgrade --wait=false          # Accept asynchronously
devbox upgrade --status              # Inspect durable progress/failure
devbox version                      # Client revision
devbox status                       # Server revision, instance, and drain state
```

`POST /v1/upgrade` returns HTTP 202 and an upgrade ID; `GET /v1/upgrade` returns
its persisted state. Both require the usual bearer token. Targets come only
from the host configuration, not request input. Progress is `building` →
`draining` → `installing` → `restarting` → `complete` (or `up_to_date`/`failed`).
Duplicate upgrades are rejected. While draining, new assignments, reviews,
and replies are rejected; existing workers finish normally. If the drain
deadline expires, installation is abandoned and admission resumes.

Before replacing binaries, Devbox saves rollback copies and a consistent
SQLite snapshot, including committed WAL data. It closes HTTP, restores the
TUI terminal, closes SQLite, and uses `exec` to preserve the daemon PID, working
directory, arguments, environment, and foreground terminal. The new process
runs normal database migrations and must pass a local HTTP health check before
the upgrade is marked complete and job admission resumes. Installation/exec
failures restore the binaries. Startup failures restore the binaries and
database snapshot; failed database/sidecar files are retained for diagnosis.

State, built binaries, and rollback copies live under `upgrades/` beside the
jobs database (normally `.devbox/upgrades/`). Source checkouts are removed after
building; rollback artifacts are retained. Abrupt process/host death still
requires the normal service supervisor or operator to start the daemon again;
startup recovers persisted upgrade state. An upgrade cannot make an unavailable
Git remote, full disk, or broken toolchain succeed: failures remain visible and
never count as completion. Keep ordinary database backups as well.

`/health`, `/v1/status`, and response headers expose the server revision and
instance ID. Polling CLI commands announce an instance change. `devbox upgrade`
verifies both the target SHA and a new healthy instance and prints the client
restart action. The sibling client on the server host is updated automatically;
restart long-running clients. Clients on other machines should build/install
the same revision, then reconnect. `--timeout` limits only the client wait;
it does not cancel the server's upgrade.

The opt-in integration test builds and restarts a real daemon against a local
temporary Git remote, checks the client binary revision, and verifies a stored
job survives. Run it from a commit containing the implementation:

```sh
DEVBOX_UPGRADE_INTEGRATION=1 go test ./cmd/devboxd -run TestSelfUpgradeProcess -v
```

## Contributing

Contributions welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Run `make test fmt`
6. Submit a pull request

## License

MIT License - see LICENSE file for details

## Links

- [ARCHITECTURE.md](ARCHITECTURE.md) - Detailed technical design
- [OpenCode](https://github.com/nicoalimin/opencode) - Coding agent
- [Linear API](https://developers.linear.app/docs/graphql/working-with-the-graphql-api) - Linear GraphQL docs
- [GitHub CLI](https://cli.github.com/) - gh CLI documentation

## Support

For issues, questions, or contributions:
- Open an issue on GitHub
- Check existing issues for solutions
- Read ARCHITECTURE.md for technical details
