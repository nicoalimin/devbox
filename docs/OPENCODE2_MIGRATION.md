# OpenCode2 API Integration Guide

## Overview

Devboxd now supports **OpenCode2** (v0.0.0-beta-19135 and later) in addition to the classic OpenCode server. This document explains the changes and how to configure your setup.

## What Changed

OpenCode2 introduced breaking changes to the HTTP API:

### API Endpoints

| Operation | Classic OpenCode | OpenCode2 |
|-----------|------------------|-----------|
| Create Session | `POST /session?directory=<path>` | `POST /api/session` with `location: {directory}` in body |
| Send Message | `POST /session/{id}/message?directory=<path>` | `POST /api/session/{id}/prompt` with `location: {directory}` in body |
| Event Stream | N/A | `GET /event` or `GET /global/event` (SSE) |
| Response Format | `{id: "string", status: "string"}` | `{data: {id: {value: "string"}}}` |

### Key Differences

1. **URL Prefix**: OpenCode2 uses `/api/` prefix for all session operations
2. **Directory Parameter**: Moved from query parameter to request body under `location.directory`
3. **Endpoint Names**: `/message` → `/prompt` for sending prompts to sessions
4. **Response Structure**: Wrapped in `{data: ...}` envelope with nested ID objects
5. **Event Streaming**: OpenCode2 provides SSE event streams at `/event` (directory-scoped) or `/global/event` for real-time session updates

## Configuration

Add `version` field to your `config.yaml`:

```yaml
opencode:
  base_url: http://localhost:3000
  version: v2  # Use "v2" for OpenCode2, "classic" for legacy OpenCode
  username: opencode  # Optional basic auth
  password: secret    # Optional basic auth
```

**Default**: If `version` is not specified, it defaults to `v2` (OpenCode2).

### For OpenCode2 Beta (v0.0.0-beta-19135+)

```yaml
opencode:
  base_url: http://localhost:3000
  version: v2
```

### For Classic OpenCode

```yaml
opencode:
  base_url: http://localhost:4096
  version: classic
```

## Testing Your Setup

### Test OpenCode2 Connection

Create a session manually to verify the server responds correctly:

```bash
# Without auth
curl -X POST http://localhost:3000/api/session \
  -H "Content-Type: application/json" \
  -d '{"title": "Test Session", "location": {"directory": "/tmp/test"}}'

# With basic auth
curl -X POST http://localhost:3000/api/session \
  -u opencode:yourpassword \
  -H "Content-Type: application/json" \
  -d '{"title": "Test Session", "location": {"directory": "/tmp/test"}}'
```

**Expected Response** (OpenCode2):
```json
{
  "data": {
    "id": {"value": "session-abc123"},
    ...
  }
}
```

### Test Classic OpenCode Connection

```bash
curl -X POST "http://localhost:4096/session?directory=/tmp/test" \
  -H "Content-Type: application/json" \
  -d '{"title": "Test Session"}'
```

**Expected Response** (Classic):
```json
{
  "id": "session-abc123",
  "status": "active"
}
```

## Error Diagnostics Improvements

When a request fails, devboxd now provides detailed error information:

**Before** (unhelpful):
```
Failed to execute coding: API returned status 405: 
```

**After** (actionable):
```
POST http://localhost:3000/api/session returned 405 Method Not Allowed (Allow: GET, PUT): 
{"error": "endpoint requires GET or PUT"}
```

The error now includes:
- HTTP method used
- Full URL attempted
- Status code and message
- `Allow` header (showing accepted methods)
- Response body with server error details

## Troubleshooting

### 405 Method Not Allowed

**Symptom**: `POST http://localhost:3000/session returned 405 Method Not Allowed`

**Cause**: You're running OpenCode2 but config is set to `version: classic` (or you're hitting the wrong endpoint).

**Fix**: 
1. Verify OpenCode2 is running: `curl http://localhost:3000/api/session`
2. Update config: `version: v2`
3. Restart devboxd

### 404 Not Found

**Symptom**: `POST http://localhost:4096/api/session returned 404 Not Found`

**Cause**: You're running classic OpenCode but config is set to `version: v2`.

**Fix**:
1. Verify classic OpenCode is running: `curl http://localhost:4096/session/status`
2. Update config: `version: classic`
3. Restart devboxd

### Empty Response Body

**Symptom**: Error message ends with `405: ` (nothing after colon)

**Cause**: Server returned no error body (uncommon in OpenCode2).

**Fix**: Check the `Allow` header in the error message to see which HTTP methods are accepted, or check OpenCode server logs.

### Connection Refused

**Symptom**: `request failed: dial tcp 127.0.0.1:3000: connection refused`

**Cause**: OpenCode server is not running or wrong port.

**Fix**:
1. Start OpenCode2: `opencode serve` (default port 3000)
2. Verify: `curl http://localhost:3000/global/health`
3. Update `base_url` in config if using non-default port

## Version Compatibility Matrix

| devboxd | OpenCode Classic | OpenCode2 Beta |
|---------|------------------|----------------|
| v0.1.0+ | ✅ `version: classic` | ❌ |
| v0.2.0+ | ✅ `version: classic` | ✅ `version: v2` |

## Example Configurations

### Local Development with OpenCode2

```yaml
server:
  listen: "0.0.0.0:8080"
  auth_token: "dev-token-123"

linear:
  api_key: "lin_api_..."
  assignee_id: "user-abc123"

opencode:
  base_url: http://localhost:3000
  version: v2

github:
  default_base_branch: main

repos:
  - match:
      team: ENG
    repo:
      path: /home/user/projects/myapp
      base_branch: main
```

### Production with Classic OpenCode + Auth

```yaml
opencode:
  base_url: https://opencode.company.com
  version: classic
  username: devboxd
  password: ${OPENCODE_PASSWORD}  # Set via env var
```

## Migration Checklist

If you're upgrading an existing setup to use OpenCode2:

- [ ] Verify OpenCode2 version: `opencode --version` (should show beta-19135 or later)
- [ ] Update config: add `version: v2` under `opencode:`
- [ ] Test session creation with curl (see above)
- [ ] Restart devboxd
- [ ] Trigger a test job: `devbox assign TEST-123`
- [ ] Verify in logs: should see "Starting OpenCode coding session" without 405 errors
- [ ] Check that worktree directory is correctly used

## Need Help?

If you encounter issues:

1. Check the error message for HTTP method, URL, and response body
2. Verify OpenCode version: `opencode --version`
3. Test the endpoint manually with curl
4. Check OpenCode server logs for details
5. Open an issue with the full error output (including method, URL, and response)
