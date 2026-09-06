# Linear n8n OpenCode Adapter

A thin, single-flight adapter that connects Linear issue tracking to a local OpenCode coding agent instance via n8n workflow automation.

## Overview

This adapter enables automatic assignment of Linear issues to a local OpenCode coding agent. It operates sequentially—**one ticket at a time**—ensuring OpenCode never receives overlapping work.

**Flow:**
1. n8n polls Linear periodically (default: every 5 minutes)
2. Checks if OpenCode is healthy and idle
3. If busy → skip cycle
4. If idle → fetch assigned Linear issues (filtered by assignee + states)
5. Pick the **first** issue (by priority)
6. Create OpenCode session and send task
7. Optionally transition Linear issue to "In Progress"

## Prerequisites

- **Node.js** 18+ (for n8n)
- **OpenCode** running locally with HTTP API exposed (default: `http://localhost:3000`)
- **Linear workspace** with API access
- Same machine for both n8n and OpenCode (communicates via localhost)

## Installation

### 1. Install n8n

Choose one of the following methods:

#### Option A: Using npm (recommended for development)

```bash
npm install -g n8n
```

#### Option B: Using Docker

```bash
docker run -d \
  --name n8n \
  --network host \
  -v ~/.n8n:/home/node/.n8n \
  n8nio/n8n
```

**Note:** Using `--network host` allows n8n to access OpenCode on `localhost`.

#### Option C: Using npx (no global install)

```bash
npx n8n
```

### 2. Configure Environment Variables

Copy the example environment file and configure it:

```bash
cp .env.example .env
```

Edit `.env` with your values:

**Required:**
- `LINEAR_API_KEY`: Your Linear API key ([get it here](https://linear.app/settings/api))
- `LINEAR_ASSIGNEE_ID`: User ID of the devbox agent in Linear
- `OPENCODE_BASE_URL`: OpenCode instance URL (default: `http://localhost:3000`)

**Optional:**
- `LINEAR_STATES`: Issue states to poll (default: `["Todo", "Triage", "Backlog"]`)
- `LINEAR_AUTO_TRANSITION`: Auto-update issue to "In Progress" (default: `false`)
- `LINEAR_IN_PROGRESS_STATE_ID`: State ID for "In Progress" (required if auto-transition enabled)

#### Finding Your Linear Assignee ID

1. Go to [Linear API Settings](https://linear.app/settings/api)
2. Open the GraphQL explorer
3. Run this query:
   ```graphql
   query {
     viewer {
       id
       name
       email
     }
   }
   ```
4. Copy the `id` value to `LINEAR_ASSIGNEE_ID`

#### Finding Linear State IDs (Optional)

If you want to customize which states to monitor or enable auto-transition:

1. Go to [Linear API Settings](https://linear.app/settings/api)
2. Run this query:
   ```graphql
   query {
     workflowStates {
       nodes {
         id
         name
         type
       }
     }
   }
   ```
3. Find the relevant state IDs

### 3. Start OpenCode

Ensure OpenCode is running with the HTTP API exposed:

```bash
opencode serve
```

By default, OpenCode serves on `http://localhost:3000`. Verify it's running:

```bash
curl http://localhost:3000/global/health
# Should return: {"healthy":true}
```

### 4. Start n8n

Load your environment variables and start n8n:

```bash
# If using npm/npx
export $(cat .env | xargs)
n8n start

# If using Docker, update the docker run command:
docker run -d \
  --name n8n \
  --network host \
  --env-file .env \
  -v ~/.n8n:/home/node/.n8n \
  n8nio/n8n
```

n8n will start on `http://localhost:5678` by default.

### 5. Import the Workflow

1. Open n8n UI: `http://localhost:5678`
2. Click **"Workflows"** in the sidebar
3. Click **"Import from File"**
4. Select `n8n-workflow.json` from this repository
5. The workflow "Linear to OpenCode Adapter" will be imported

### 6. Activate the Workflow

1. Open the imported workflow
2. Click **"Active"** toggle in the top-right
3. The workflow will now run on its schedule

## How It Works

### Sequential Single-Flight Execution

The adapter enforces **strict sequential processing**:

1. **Health Check**: Before every cycle, verify OpenCode is healthy via `GET /global/health`
2. **Busy Check**: Query `GET /session/status` to check for active sessions
3. **Skip if Busy**: If any session is active/running, exit immediately (no-op)
4. **Poll Linear**: Only when idle, query Linear for assigned issues
5. **Pick One**: Take the first issue (ordered by priority)
6. **Create Session**: POST to OpenCode to create a new session
7. **Send Task**: POST the issue details as a message to the session
8. **Optional Update**: If `LINEAR_AUTO_TRANSITION=true`, update Linear status

**This guarantees OpenCode never works on multiple tickets simultaneously.**

### Linear Query Behavior

The workflow queries Linear with these filters:
- **Assignee**: Matches `LINEAR_ASSIGNEE_ID`
- **States**: Matches states in `LINEAR_STATES` (default: Todo, Triage, Backlog)
- **Order**: Priority (highest first)
- **Limit**: 10 issues (only the first is processed)

### OpenCode Integration

The workflow creates an OpenCode session and sends a structured message:

```
Build the following Linear issue:

Identifier: ENG-123
Title: Fix user authentication bug
URL: https://linear.app/company/issue/ENG-123
Priority: High (2)
Team: Engineering (ENG)
State: Todo
Project: Q4 Roadmap
Labels: bug, security

Description:
[Full issue description from Linear]

Instructions:
1. Review the .opencode instructions in this repository
2. Search Notion for related PRDs, specs, or context using the issue title and team
3. Implement the required changes
4. Update Linear status as you progress
5. Leave a Linear comment with the PR link when complete
```

## Configuration

### Adjusting Poll Interval

The default poll interval is **5 minutes**. To change:

1. Open the workflow in n8n UI
2. Click the **"Schedule Trigger"** node
3. Modify the interval (e.g., 10 minutes, 15 minutes)
4. Save the workflow

### Customizing Linear States

Edit `LINEAR_STATES` in `.env`:

```bash
# Example: Include "In Progress" to pick up partially-started tickets
LINEAR_STATES=["Todo", "Triage", "Backlog", "In Progress"]
```

### Enabling Auto-Transition

To automatically move issues to "In Progress" when assigned to OpenCode:

1. Find your "In Progress" state ID (see "Finding Linear State IDs" above)
2. Update `.env`:
   ```bash
   LINEAR_AUTO_TRANSITION=true
   LINEAR_IN_PROGRESS_STATE_ID=your-state-id
   ```
3. Restart n8n

**Note:** It's recommended to leave this disabled and let the OpenCode agent manage status updates (see `.opencode` instructions).

## Testing

### 1. Verify OpenCode is Running

```bash
curl http://localhost:3000/global/health
```

Expected: `{"healthy":true}`

### 2. Create a Test Issue in Linear

1. Go to your Linear workspace
2. Create a new issue
3. Assign it to the user configured in `LINEAR_ASSIGNEE_ID`
4. Set the state to one of the states in `LINEAR_STATES` (e.g., "Todo")

### 3. Manually Trigger the Workflow

In n8n UI:
1. Open the workflow
2. Click **"Execute Workflow"** button
3. Watch the execution path

### 4. Verify Task Reached OpenCode

Check OpenCode logs or UI to confirm the session was created and the task was received.

### 5. Monitor Workflow Executions

In n8n UI:
1. Click **"Executions"** in the sidebar
2. View success/failure history
3. Click an execution to see detailed logs

## Troubleshooting

### OpenCode Not Responding

**Problem:** Workflow shows "Unhealthy" or connection errors

**Solutions:**
- Verify OpenCode is running: `ps aux | grep opencode`
- Check OpenCode logs for errors
- Ensure correct port in `OPENCODE_BASE_URL`
- Test health endpoint: `curl http://localhost:3000/global/health`

### Linear Authentication Failed

**Problem:** "Invalid API key" or 401 errors

**Solutions:**
- Verify `LINEAR_API_KEY` format: should start with `lin_api_`
- Regenerate API key in [Linear Settings](https://linear.app/settings/api)
- Ensure API key has required permissions
- Check for extra whitespace in `.env`

### No Issues Being Picked Up

**Problem:** Workflow runs but never picks issues

**Solutions:**
- Verify `LINEAR_ASSIGNEE_ID` matches the assignee in Linear
- Check `LINEAR_STATES` includes the state of your test issue
- Query Linear API directly to verify issues exist:
  ```bash
  curl -X POST https://api.linear.app/graphql \
    -H "Authorization: $LINEAR_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{"query":"{ viewer { id assignedIssues(first:5) { nodes { identifier title state { name } } } } }"}'
  ```
- Review n8n execution logs for the "Query Linear Issues" node

### OpenCode Always Busy

**Problem:** Workflow never starts new work

**Solutions:**
- Check OpenCode session status: `curl http://localhost:3000/session/status`
- Review the "Parse Session Status" node logic in n8n
- Manually abort stuck sessions in OpenCode
- Verify the busy detection logic matches your OpenCode version

### Environment Variables Not Loading

**Problem:** Workflow shows "undefined" for env vars

**Solutions:**
- Ensure `.env` file is in the same directory
- Restart n8n after changing `.env`
- For Docker: verify `--env-file` path is correct
- For npm: ensure `export $(cat .env | xargs)` was run in the same shell

### Network Issues (n8n ↔ OpenCode)

**Problem:** n8n cannot reach OpenCode on localhost

**Solutions:**
- If using Docker for n8n: ensure `--network host` is set
- Try `OPENCODE_BASE_URL=http://host.docker.internal:3000` (macOS/Windows Docker)
- For Linux Docker: use `OPENCODE_BASE_URL=http://172.17.0.1:3000`
- Test connectivity from within n8n container:
  ```bash
  docker exec -it n8n curl http://localhost:3000/global/health
  ```

## OpenCode Agent Instructions

This adapter includes `.opencode` instructions that guide the coding agent's behavior. These instructions tell OpenCode to:

1. **Search Notion proactively** for related PRDs, specs, and context before starting work
2. **Update Linear status** as work progresses (In Progress → In Review → Done)
3. **Leave Linear comments** with PR links and summaries when work completes
4. **Ask for clarification** rather than guessing product requirements

See `.opencode/AGENTS.md` for full details.

## Architecture

```
┌─────────────────┐
│  Linear Workspace│
│  (Issues)       │
└────────┬────────┘
         │ GraphQL API
         │ (poll every 5min)
         ▼
┌─────────────────┐
│   n8n Workflow  │
│  ┌──────────┐   │
│  │ 1. Health│   │
│  │ 2. Busy? │   │
│  │ 3. Query │   │
│  │ 4. Pick  │   │
│  │ 5. POST  │   │
│  └──────────┘   │
└────────┬────────┘
         │ HTTP API
         │ localhost:3000
         ▼
┌─────────────────┐
│  OpenCode Agent │
│  (Coding)       │
│  ┌──────────┐   │
│  │ Session  │   │
│  │ Execute  │   │
│  │ Code     │   │
│  └──────────┘   │
└────────┬────────┘
         │
         ├──► GitHub (PRs)
         ├──► Notion (context)
         └──► Linear (updates)
```

## Design Principles

1. **Thin adapter**: n8n does minimal work—poll, check, POST
2. **Single-flight**: Never start ticket B while ticket A is running
3. **Stateless**: No database; OpenCode session status is the source of truth
4. **Localhost-first**: OpenCode and n8n share the host (no network latency)
5. **Configurable**: All critical values in environment variables
6. **Sequential priority**: Always process highest-priority issue first

## Advanced Usage

### Custom OpenCode API

If your OpenCode instance has a different API structure:

1. Edit the workflow in n8n UI
2. Modify the "Check OpenCode Sessions" and "Create OpenCode Session" nodes
3. Adjust the busy-check logic in "Parse Session Status"
4. Update the message format in "Prepare Task Message"

### Multi-Team Support

To support multiple teams with different assignees:

1. Duplicate the workflow in n8n
2. Create separate `.env` files or use n8n's credential system
3. Adjust `LINEAR_ASSIGNEE_ID` per team
4. Optionally filter by team ID in the Linear query

### Webhook Alternative

For real-time triggering instead of polling:

1. Configure Linear webhooks (requires public endpoint)
2. Use n8n's webhook trigger instead of schedule trigger
3. Add filtering logic to ignore non-assigned issues
4. Maintain busy-check logic to enforce single-flight

## Security Considerations

- **API Keys**: Store `LINEAR_API_KEY` securely; never commit to git
- **Localhost**: OpenCode API should only be accessible locally (not exposed to internet)
- **n8n Auth**: Enable authentication in n8n for production use
- **Secrets**: Use n8n's credential system for sensitive values in production

## Contributing

To improve this adapter:

1. Fork the repository
2. Make changes to `n8n-workflow.json` or documentation
3. Test with a live OpenCode + Linear setup
4. Submit a pull request with clear description

## License

MIT

## Support

For issues or questions:
- Check the Troubleshooting section above
- Review n8n execution logs
- Verify OpenCode API documentation
- Open an issue in this repository
