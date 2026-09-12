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

# Run the server
devboxd --config devboxd.yaml
```

The server will:
- Listen on `0.0.0.0:8080` (configurable)
- Create `devboxd.db` for persistent state
- Wait for job assignments via the HTTP API

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

linear:
  # api_key: "lin_api_your_key_here"     # or set LINEAR_API_KEY env var
  assignee_id: ""                         # Optional: only process issues assigned to this user
  workspace_id: ""                        # Optional: workspace ID

opencode:
  base_url: "http://localhost:3000"
  timeout: "30m"                          # Mark blocked after this timeout

github:
  default_base_branch: "main"

# Map Linear issue attributes to repository paths
repos:
  - match:
      team: "ENG"
    repo:
      path: "/home/dev/my-backend"
      base_branch: "main"
  
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
```

**Environment Variable Overrides:**
- `DEVBOXD_AUTH_TOKEN` - Server authentication token
- `LINEAR_API_KEY` - Linear API key
- `LINEAR_ASSIGNEE_ID` - Linear user ID filter
- `OPENCODE_BASE_URL` - OpenCode server URL

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

1. **Session Creation**: `POST /session` with job name
2. **Task Prompt**: `POST /session/:id/message` with issue details
3. **Review Prompt**: Another message for code review

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
ExecStart=/usr/local/bin/devboxd --config /etc/devboxd/config.yaml --db /var/lib/devboxd/devboxd.db
Restart=always
RestartSec=10

# Environment variables
Environment="LINEAR_API_KEY=lin_api_..."
Environment="DEVBOXD_AUTH_TOKEN=..."

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable devboxd
sudo systemctl start devboxd
sudo systemctl status devboxd
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

## Troubleshooting

### Server Won't Start

**Error**: `failed to load config`
- Check that `devboxd.yaml` exists and is valid YAML
- Ensure required fields are set (auth_token, linear.api_key, repos)

**Error**: `failed to open database`
- Check file permissions on the database path
- Ensure the parent directory exists

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
- Verify OpenCode is running: `curl http://localhost:3000/global/health`
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
