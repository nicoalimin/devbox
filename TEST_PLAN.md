# Devbox Test Plan

This document outlines the testing strategy for the devbox client-server system.

## Pre-Deployment Testing

### Unit Tests (Automated)

All unit tests are located in `*_test.go` files and can be run with:

```bash
make test
```

**Coverage:**
- ✅ Job state transitions
- ✅ Terminal/busy state detection
- ✅ Current job detection (single-flight)
- ✅ Blocked jobs listing
- ✅ Log persistence and retrieval
- ✅ Single-flight enforcement
- ✅ Job cancellation
- ✅ Reply to blocked jobs

### Build Tests

```bash
# Build both binaries
make build

# Verify binaries exist
ls -lh bin/

# Test CLI help
./bin/devbox --help
./bin/devboxd --help

# Cross-compile (optional)
make build-all
```

## Post-Deployment Testing

### Phase 1: Server Setup Validation

**Prerequisites:**
- Coding host with Go, git, gh CLI, OpenCode running

**Steps:**

1. **Install server**:
   ```bash
   sudo cp bin/devboxd /usr/local/bin/
   cp devboxd.yaml.example devboxd.yaml
   # Edit devboxd.yaml with real credentials
   ```

2. **Configure server**:
   - Set `DEVBOXD_AUTH_TOKEN`
   - Set `LINEAR_API_KEY`
   - Configure `repos` mapping (test repo paths)
   - Verify `opencode.base_url` matches running OpenCode

3. **Start server**:
   ```bash
   devboxd --config devboxd.yaml --db devboxd-test.db
   ```

4. **Verify health**:
   ```bash
   curl http://localhost:8080/health
   # Expected: {"healthy":true,"version":"0.1.0"}
   ```

5. **Verify auth**:
   ```bash
   # Without auth (should fail)
   curl http://localhost:8080/v1/status
   # Expected: 401 Unauthorized

   # With auth (should succeed)
   curl -H "Authorization: Bearer your-token" http://localhost:8080/v1/status
   # Expected: {"version":"0.1.0","busy":false}
   ```

### Phase 2: Client Setup Validation

**Prerequisites:**
- Grok Bot host (or test machine)
- Network connectivity to server

**Steps:**

1. **Install client**:
   ```bash
   sudo cp bin/devbox /usr/local/bin/
   ```

2. **Configure client**:
   ```bash
   export DEVBOX_SERVER_URL="http://coding-host:8080"
   export DEVBOX_TOKEN="your-token"
   ```

3. **Test health**:
   ```bash
   devbox health
   # Expected: ✓ Server is healthy (version 0.1.0)
   ```

4. **Test status**:
   ```bash
   devbox status
   # Expected: Version: 0.1.0
   #           Status: IDLE
   ```

5. **Test JSON output**:
   ```bash
   devbox status --json
   # Expected: {"version":"0.1.0","busy":false}
   ```

### Phase 3: End-to-End Job Flow

**Prerequisites:**
- Server and client configured
- Test Linear issue (e.g., TEST-1)
- Git repository configured in devboxd.yaml

**Test Case 1: Successful Job Completion**

1. **Assign job**:
   ```bash
   devbox assign TEST-1
   # Note the job ID
   ```

2. **Monitor status**:
   ```bash
   devbox status
   # Expected: Status: BUSY, Current Job: <id>
   ```

3. **Check job details**:
   ```bash
   devbox job <job-id>
   # Expected: State transitions (fetching -> preparing -> coding -> ...)
   ```

4. **View logs**:
   ```bash
   devbox logs <job-id>
   # Expected: Chronological log entries
   ```

5. **Verify artifacts**:
   - Git worktree created under `.devbox-worktrees/`
   - Branch exists: `git branch | grep devbox/TEST-1`
   - OpenCode session started (check OpenCode logs)

6. **Wait for completion**:
   ```bash
   devbox job <job-id>
   # Expected: State: done, PR URL populated
   ```

7. **Verify PR**:
   - PR opened on GitHub
   - Linear issue commented with PR link

8. **Verify cleanup**:
   - Worktree removed after completion

**Test Case 2: Single-Flight Enforcement**

1. **Start first job**:
   ```bash
   devbox assign TEST-1
   ```

2. **Try to start second job while first is running**:
   ```bash
   devbox assign TEST-2
   # Expected: Error: server busy with job <id>
   ```

3. **Verify status**:
   ```bash
   devbox status
   # Expected: Status: BUSY
   ```

4. **Wait for first job to complete**:
   ```bash
   # Monitor until done
   devbox job <first-job-id>
   ```

5. **Now assign second job**:
   ```bash
   devbox assign TEST-2
   # Expected: Success
   ```

**Test Case 3: Blocked Job & Reply**

*Note: This test requires OpenCode to actually request clarification, which may be difficult to trigger automatically. Consider this a manual test.*

1. **Create a vague Linear issue** that OpenCode will need clarification on

2. **Assign the issue**:
   ```bash
   devbox assign VAGUE-1
   ```

3. **Wait for OpenCode to block**:
   ```bash
   # Monitor until state becomes "blocked"
   devbox job <job-id>
   ```

4. **List blockers**:
   ```bash
   devbox blockers
   # Expected: Show VAGUE-1 with blocker reason
   ```

5. **Reply with clarification**:
   ```bash
   devbox reply <job-id> "Use the blue color from the design system"
   ```

6. **Verify job resumed**:
   ```bash
   devbox job <job-id>
   # Expected: State changed from blocked to coding
   ```

**Test Case 4: Job Cancellation**

1. **Start a job**:
   ```bash
   devbox assign TEST-1
   ```

2. **Cancel while running**:
   ```bash
   devbox cancel <job-id>
   ```

3. **Verify cancellation**:
   ```bash
   devbox job <job-id>
   # Expected: State: cancelled, CompletedAt set
   ```

4. **Verify cleanup**:
   - Worktree removed
   - Server status now IDLE

**Test Case 5: Job List & History**

1. **Create multiple jobs** (assign, let complete)

2. **List all jobs**:
   ```bash
   devbox jobs --limit 10
   # Expected: Table with job IDs, states, timestamps
   ```

3. **Get specific job details**:
   ```bash
   devbox job <job-id>
   # Expected: Full job details
   ```

4. **Check logs for old job**:
   ```bash
   devbox logs <job-id> --tail 20
   # Expected: Last 20 log entries
   ```

### Phase 4: Failure & Recovery Testing

**Test Case 6: Server Restart**

1. **Start a job**:
   ```bash
   devbox assign TEST-1
   ```

2. **While job is running, restart server**:
   ```bash
   # Kill and restart devboxd
   pkill devboxd
   devboxd --config devboxd.yaml --db devboxd-test.db
   ```

3. **Verify state persisted**:
   ```bash
   devbox job <job-id>
   # Expected: Job still exists with its state
   ```

4. **Check logs persisted**:
   ```bash
   devbox logs <job-id>
   # Expected: Logs from before restart still present
   ```

**Test Case 7: Network Failure**

1. **Client loses connection to server**:
   ```bash
   # Stop server or block network
   devbox status
   # Expected: Error: request failed: connection refused
   ```

2. **Client retries after server back online**:
   ```bash
   devbox status
   # Expected: Success
   ```

**Test Case 8: Git Failures**

1. **Configure repo path incorrectly** in devboxd.yaml

2. **Assign job**:
   ```bash
   devbox assign TEST-1
   ```

3. **Verify failure logged**:
   ```bash
   devbox job <job-id>
   # Expected: State: failed, Reason: repository path error
   ```

4. **Check logs**:
   ```bash
   devbox logs <job-id>
   # Expected: Clear error message about git
   ```

**Test Case 9: Linear API Failure**

1. **Use invalid Linear API key** in devboxd.yaml

2. **Assign job**:
   ```bash
   devbox assign TEST-1
   ```

3. **Verify failure**:
   ```bash
   devbox job <job-id>
   # Expected: State: failed, Reason: Linear API error
   ```

**Test Case 10: OpenCode Unavailable**

1. **Stop OpenCode** (or configure wrong base_url)

2. **Assign job**:
   ```bash
   devbox assign TEST-1
   ```

3. **Verify failure**:
   ```bash
   devbox job <job-id>
   # Expected: State: failed, Reason: OpenCode connection error
   ```

### Phase 5: Performance & Load Testing

**Test Case 11: Rapid Assignment Attempts**

```bash
# Try to spam assignments
for i in {1..10}; do
  devbox assign TEST-$i &
done
wait
# Expected: Only one succeeds, rest get 409 Conflict
```

**Test Case 12: Large Job History**

1. **Create 100+ jobs** over time

2. **List jobs**:
   ```bash
   time devbox jobs --limit 100
   # Expected: < 1 second response time
   ```

3. **Query specific job**:
   ```bash
   time devbox job <old-job-id>
   # Expected: < 100ms response time
   ```

**Test Case 13: Log Volume**

1. **Job with 1000+ log entries** (simulate verbose OpenCode)

2. **Get all logs**:
   ```bash
   time devbox logs <job-id>
   # Expected: < 2 seconds
   ```

3. **Get tail**:
   ```bash
   time devbox logs <job-id> --tail 50
   # Expected: < 100ms
   ```

### Phase 6: Integration with Grok Bot

**Test Case 14: Grok Bot Script**

Create a test script that Grok Bot would invoke:

```bash
#!/bin/bash
# grok-bot-assign.sh

LINEAR_ISSUE="$1"

# Check server health
if ! devbox health &>/dev/null; then
  echo "ERROR: Server unhealthy"
  exit 1
fi

# Check if busy
STATUS=$(devbox status --json)
if echo "$STATUS" | jq -e '.busy == true' > /dev/null; then
  echo "ERROR: Server busy"
  exit 1
fi

# Assign job
JOB=$(devbox assign "$LINEAR_ISSUE" --json)
if [ $? -ne 0 ]; then
  echo "ERROR: Failed to assign job"
  exit 1
fi

JOB_ID=$(echo "$JOB" | jq -r '.id')
echo "SUCCESS: Job $JOB_ID created for $LINEAR_ISSUE"
echo "Monitor: devbox job $JOB_ID"
```

Test:
```bash
./grok-bot-assign.sh TEST-1
# Expected: Success message with job ID
```

**Test Case 15: Grok Bot Blocker Check**

```bash
#!/bin/bash
# grok-bot-check-blockers.sh

BLOCKERS=$(devbox blockers --json)
COUNT=$(echo "$BLOCKERS" | jq '.blockers | length')

if [ "$COUNT" -gt 0 ]; then
  echo "Found $COUNT blocked jobs:"
  echo "$BLOCKERS" | jq -r '.blockers[] | "\(.linear_issue_id): \(.blocker_reason)"'
fi
```

Test:
```bash
./grok-bot-check-blockers.sh
# Expected: List of blocked jobs (if any)
```

## Regression Testing

After any code changes:

1. **Run unit tests**: `make test`
2. **Rebuild**: `make build`
3. **Re-test Phase 3**: End-to-End Job Flow
4. **Re-test Phase 4**: Failure & Recovery

## Sign-Off Criteria

Before considering deployment complete:

- [ ] All unit tests pass
- [ ] Server starts without errors
- [ ] Client can connect to server
- [ ] At least one successful end-to-end job completion
- [ ] Single-flight enforcement verified
- [ ] Job state persists across server restart
- [ ] Logs are recorded and queryable
- [ ] PR is created on GitHub for completed job
- [ ] Linear issue is updated with PR link
- [ ] No credential leaks in logs or responses

## Known Limitations

Document any issues found during testing:

1. **OpenCode API assumptions**: If endpoints differ, document actual shape
2. **Network latency**: If Grok Bot ↔ coding host latency is high, consider increasing timeouts
3. **Git credentials**: Must be configured correctly on coding host
4. **GitHub CLI auth**: `gh` must be authenticated before first PR

## Support & Troubleshooting

If tests fail, refer to:
- README.md "Troubleshooting" section
- ARCHITECTURE.md for technical details
- Server logs: `devbox logs <job-id>`
- Database: `sqlite3 devboxd.db "SELECT * FROM jobs WHERE id='<job-id>'"`
