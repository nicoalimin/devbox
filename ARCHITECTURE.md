# Devbox Architecture

## Overview

Devbox is a client-server system that orchestrates OpenCode coding agents through Grok Bot. The architecture consists of two binaries that work together to manage Linear issue assignments and automate code changes.

## System Diagram

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

## Components

### 1. devbox (Client Binary)

**Purpose**: Thin CLI wrapper for the server HTTP API, installed on Grok Bot's machine.

**Commands**:
- `devbox health` - Check if server is reachable
- `devbox status` - Get overall status + current job summary
- `devbox assign <LINEAR_ISSUE_ID>` - Create/start job for Linear ticket
- `devbox jobs [--limit N]` - List recent jobs
- `devbox job <job_id>` - Get detailed job status
- `devbox blockers` - List jobs in blocked state
- `devbox reply <job_id> <message>` - Send clarification to resume job
- `devbox cancel <job_id>` - Cancel a job
- `devbox logs <job_id> [--tail N]` - View job logs

**Configuration**:
- `DEVBOX_SERVER_URL` - Server endpoint (e.g., https://coding-host:8080)
- `DEVBOX_TOKEN` - Authentication bearer token

**Output**: JSON format with `--json` flag, human-friendly by default.

### 2. devboxd (Server Daemon)

**Purpose**: Long-running daemon on the bare-metal coding host that orchestrates the entire workflow.

**Workflow**:
1. Receive assignment via HTTP API
2. Fetch Linear issue details (GraphQL)
3. Resolve target git repository from config
4. Create isolated git worktree + branch
5. Drive OpenCode to implement the ticket
6. Run code review pass with OpenCode
7. Push branch to remote
8. Open PR via `gh` CLI
9. Update Linear with PR link
10. Report completion or blocked state

**API Endpoints** (all under `/v1`):
- `GET /health` - Health check
- `GET /v1/status` - System status (busy, currentJobId, version)
- `POST /v1/jobs` - Create new job (body: `{"linearIssueId": "ENG-123"}`)
- `GET /v1/jobs` - List all jobs
- `GET /v1/jobs/:id` - Get job details
- `GET /v1/blockers` - Get jobs in blocked state
- `POST /v1/jobs/:id/reply` - Send clarification (body: `{"message": "..."}`)
- `POST /v1/jobs/:id/cancel` - Cancel job
- `GET /v1/jobs/:id/logs` - Get job logs

**Authentication**: Bearer token via `Authorization` header.

### 3. Job State Machine

**States**:
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

**Transitions**:
```
queued -> fetching -> preparing -> coding -> reviewing -> pushing -> pr_open -> done
   |         |            |           |          |           |          |
   |         v            v           v          v           v          v
   +-------> failed    failed      blocked    failed     failed    failed
             
blocked -> (reply received) -> coding/reviewing (resume)
```

### 4. Persistence

**Job Storage**: SQLite database (`devboxd.db`)

**Schema**:
```sql
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,           -- UUID
    linear_issue_id TEXT NOT NULL, -- e.g., ENG-123
    linear_url TEXT,
    state TEXT NOT NULL,
    repo_path TEXT,
    branch_name TEXT,
    worktree_path TEXT,
    pr_url TEXT,
    blocker_reason TEXT,
    opencode_session_id TEXT,
    created_at TIMESTAMP,
    updated_at TIMESTAMP,
    completed_at TIMESTAMP
);

CREATE TABLE job_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL,
    timestamp TIMESTAMP,
    level TEXT,              -- info, warn, error
    message TEXT,
    FOREIGN KEY (job_id) REFERENCES jobs(id)
);
```

### 5. Configuration

**Server Config** (`devboxd.yaml` or env vars):
```yaml
server:
  listen: "0.0.0.0:8080"
  auth_token: "secret-token-here"  # or via DEVBOXD_AUTH_TOKEN env

linear:
  api_key: "lin_api_..."           # or LINEAR_API_KEY
  assignee_id: "user-uuid"         # optional filter
  workspace_id: "workspace-uuid"   # optional

opencode:
  base_url: "http://127.0.0.1:3000"
  # username/password if needed

github:
  # Uses gh CLI auth from host

repos:
  # Map Linear identifiers to repo paths
  # Key can be: team key, project name, label, or custom field
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

## Single-Flight Enforcement

Only **one running job** at a time:
- When a job is in `fetching`, `preparing`, `coding`, `reviewing`, `pushing`, or `pr_open` state, the server is "busy"
- New assign requests while busy:
  - If queue disabled (default): Return HTTP 409 Conflict with message "Server busy with job XYZ"
  - If queue enabled: Queue up to `max_depth`, then reject with 409

## OpenCode Integration

**Assumed OpenCode HTTP API** (based on n8n workflow + common patterns):

### Health Check
```
GET /global/health
Response: { "healthy": true }
```

### Session Management
```
GET /session/status
Response: { "session-id": { "status": "idle|active", "busy": false } }

POST /session
Body: { "name": "ENG-123: Add feature" }
Response: { "id": "session-uuid", "status": "active" }
```

### Messaging
```
POST /session/:id/message
Body: { "parts": [{ "type": "text", "text": "Build this feature..." }] }
Response: { "success": true }
```

**Note**: These endpoints are inferred from the n8n workflow. If OpenCode's actual API differs, the server implementation documents the assumptions and provides hooks for customization.

## Security Considerations

1. **Authentication**: Server requires bearer token on all `/v1/*` endpoints
2. **Network**: Recommend Tailscale or private network between Grok Bot and coding host
3. **Token Storage**: Client stores token in env var (not in filesystem)
4. **Linear API Key**: Server-side only, never exposed to client

## Monitoring & Observability

- **Health Endpoint**: `/health` for uptime checks
- **Status Endpoint**: `/v1/status` shows if busy and current job
- **Structured Logs**: All operations logged to job_logs table
- **Job Logs**: Queryable via API for debugging

## Error Handling

- **Transient Errors**: Retry with exponential backoff (Linear/GitHub API)
- **OpenCode Timeout**: Mark as blocked after configurable timeout
- **Git Failures**: Clean up worktree and mark failed
- **Network Failures**: Log and mark failed, allow retry

## Integration with .opencode Instructions

The existing `.opencode/AGENTS.md` instructions remain valid. When `devboxd` creates an OpenCode session, it:

1. Includes the Linear issue context in the prompt
2. Explicitly references `.opencode` instructions
3. Reminds OpenCode to:
   - Search Notion for context
   - Update Linear status
   - Leave PR comments in Linear

## Migration from n8n

The n8n workflow (`n8n-workflow.json`) can remain as **legacy optional** documentation. It demonstrates the original polling-based approach. The new client-server architecture is the **primary path** for Grok Bot orchestration.

Key differences:
- **Old**: n8n polls Linear every 5 minutes, pushes to OpenCode
- **New**: Grok Bot explicitly assigns via `devbox assign`, server pulls Linear on-demand

## Technology Stack

- **Language**: Go (single static binary, excellent concurrency, cross-platform)
- **Database**: SQLite (embedded, no separate service)
- **HTTP**: stdlib `net/http` or `chi` router
- **Testing**: Go testing stdlib + testify for assertions

## Build & Deploy

### Build
```bash
make build
# or
go build -o bin/devbox ./cmd/devbox
go build -o bin/devboxd ./cmd/devboxd
```

### Install Client (Grok Bot box)
```bash
cp bin/devbox /usr/local/bin/
export DEVBOX_SERVER_URL="https://coding-host:8080"
export DEVBOX_TOKEN="your-token"
```

### Install Server (Coding host)
```bash
cp bin/devboxd /usr/local/bin/
cp devboxd.yaml.example devboxd.yaml
# Edit devboxd.yaml with your config
devboxd --config devboxd.yaml
```

### Systemd Service (optional)
```ini
[Unit]
Description=Devbox Daemon
After=network.target

[Service]
Type=simple
User=devbox
ExecStart=/usr/local/bin/devboxd --config /etc/devboxd/config.yaml
Restart=always

[Install]
WantedBy=multi-user.target
```

## Future Enhancements

1. **Parallel Jobs**: Support multiple concurrent jobs (per-repo locks)
2. **Job Priority**: Queue management with priority ordering
3. **Webhooks**: Push notifications instead of polling
4. **Metrics**: Prometheus/StatsD integration
5. **Web UI**: Dashboard for job status visualization
6. **Multi-Host**: Distribute jobs across multiple coding hosts

## Success Criteria

- ✅ `devbox` and `devboxd` build from source
- ✅ Documented API and CLI usage
- ✅ Single-flight execution enforced
- ✅ Linear ID → worktree → OpenCode → review → PR path works
- ✅ Job state persistence across restarts
- ✅ Clear error messages and logging
- ✅ Integration with existing `.opencode` instructions
