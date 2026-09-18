package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStopSessionConfirmsIdleAfterInterrupt(t *testing.T) {
	checks := 0
	interrupted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session/test/interrupt" {
			interrupted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !interrupted {
			t.Error("checked before interrupt")
		}
		checks++
		if checks < 2 {
			fmt.Fprint(w, `{"data":{"test":{}}}`)
		} else {
			fmt.Fprint(w, `{"data":{}}`)
		}
	}))
	defer server.Close()
	if err := NewClient(server.URL, "", "", "v2").StopSession("test", ""); err != nil {
		t.Fatal(err)
	}
	if checks != 2 {
		t.Fatalf("did not wait for interruption to settle: %d checks", checks)
	}
}

func TestCreateSessionV2(t *testing.T) {
	tests := []struct {
		name          string
		title         string
		directory     string
		wantTitle     string
		wantDirectory string
	}{
		{
			name:          "create session with title and directory",
			title:         "ENG-123: Fix bug",
			directory:     "/tmp/worktree/ENG-123",
			wantTitle:     "ENG-123: Fix bug",
			wantDirectory: "/tmp/worktree/ENG-123",
		},
		{
			name:          "create session without directory",
			title:         "ENG-456: Add feature",
			directory:     "",
			wantTitle:     "ENG-456: Add feature",
			wantDirectory: "",
		},
		{
			name:          "create session with directory with spaces",
			title:         "Test",
			directory:     "/tmp/work tree/test",
			wantTitle:     "Test",
			wantDirectory: "/tmp/work tree/test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server for OpenCode2 API
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request method and path
				if r.Method != "POST" {
					t.Errorf("Expected POST method, got %s", r.Method)
				}
				if r.URL.Path != "/api/session" {
					t.Errorf("Expected path /api/session, got %s", r.URL.Path)
				}

				// Verify request body
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("Failed to decode request body: %v", err)
				}

				if title, ok := body["title"].(string); !ok || title != tt.wantTitle {
					t.Errorf("Expected title %q in body, got %v", tt.wantTitle, body["title"])
				}

				// Verify location with directory in OpenCode2 format
				if tt.wantDirectory != "" {
					location, ok := body["location"].(map[string]interface{})
					if !ok {
						t.Error("Expected location object in body")
					} else {
						if dir, ok := location["directory"].(string); !ok || dir != tt.wantDirectory {
							t.Errorf("Expected directory %q in location, got %v", tt.wantDirectory, location["directory"])
						}
					}
				}

				// Send OpenCode2 response format
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"id": map[string]interface{}{
							"value": "session-123",
						},
					},
				})
			}))
			defer server.Close()

			// Create client with v2 version
			client := NewClient(server.URL, "", "", "v2")
			session, err := client.CreateSession(tt.title, tt.directory)
			if err != nil {
				t.Fatalf("CreateSession failed: %v", err)
			}

			if session.ID != "session-123" {
				t.Errorf("Expected session ID 'session-123', got %q", session.ID)
			}
		})
	}
}

func TestCreateSessionV2IDFormats(t *testing.T) {
	tests := []struct {
		name         string
		responseBody string
		wantID       string
		wantError    bool
	}{
		{
			name:         "data.id as plain string (simple)",
			responseBody: `{"data": {"id": "ses_abc123"}}`,
			wantID:       "ses_abc123",
			wantError:    false,
		},
		{
			name:         "data.id as plain string (real OpenCode2 beta-19135 response)",
			responseBody: `{"data":{"id":"ses_f6569f945ffeSs3ilx8iMGAHto","projectID":"bb7b415881da8d795e39037bf3fd0477118b918f","cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1789299918592,"updated":1789299918592},"title":"probe","location":{"directory":"/Users/nicoalimin/code/tokoboss"}}}`,
			wantID:       "ses_f6569f945ffeSs3ilx8iMGAHto",
			wantError:    false,
		},
		{
			name:         "data.id with value field",
			responseBody: `{"data": {"id": {"value": "ses_xyz789"}}}`,
			wantID:       "ses_xyz789",
			wantError:    false,
		},
		{
			name:         "data.id with id field",
			responseBody: `{"data": {"id": {"id": "ses_nested123"}}}`,
			wantID:       "ses_nested123",
			wantError:    false,
		},
		{
			name:         "top-level id as string",
			responseBody: `{"id": "ses_toplevel"}`,
			wantID:       "ses_toplevel",
			wantError:    false,
		},
		{
			name:         "top-level id with value",
			responseBody: `{"id": {"value": "ses_topvalue"}}`,
			wantID:       "ses_topvalue",
			wantError:    false,
		},
		{
			name:         "empty id returns error with response",
			responseBody: `{"data": {"id": ""}}`,
			wantID:       "",
			wantError:    true,
		},
		{
			name:         "missing id returns error with response",
			responseBody: `{"data": {"status": "active"}}`,
			wantID:       "",
			wantError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			client := NewClient(server.URL, "", "", "v2")
			session, err := client.CreateSession("Test", "/tmp/test")

			if tt.wantError {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				// Verify error includes response body
				if !contains(err.Error(), "response:") {
					t.Errorf("Error should include response body, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				if session.ID != tt.wantID {
					t.Errorf("Expected session ID %q, got %q", tt.wantID, session.ID)
				}
			}
		})
	}
}

func TestSendMessageV2(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		message   string
	}{
		{
			name:      "send message",
			sessionID: "session-123",
			message:   "Fix the authentication bug",
		},
		{
			name:      "send message with newlines",
			sessionID: "session-456",
			message:   "Add tests\n\nInclude edge cases",
		},
		{
			name:      "send long coding prompt",
			sessionID: "ses_abc123",
			message:   "Build the Linear issue:\n\nTitle: Fix auth\nDescription: Update middleware",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server for OpenCode2 beta-19135 API
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request method and path
				if r.Method != "POST" {
					t.Errorf("Expected POST method, got %s", r.Method)
				}
				expectedPath := "/api/session/" + tt.sessionID + "/prompt"
				if r.URL.Path != expectedPath {
					t.Errorf("Expected path %s, got %s", expectedPath, r.URL.Path)
				}

				// Verify request body matches OpenCode2 beta-19135 flat structure
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("Failed to decode request body: %v", err)
				}

				// Verify text field at ROOT level (beta-19135 format)
				text, ok := body["text"].(string)
				if !ok {
					t.Fatalf("Expected 'text' field at root level, got %v", body)
				}
				if text != tt.message {
					t.Errorf("Expected text %q, got %q", tt.message, text)
				}

				// Verify OLD formats are NOT used
				if _, hasPrompt := body["prompt"]; hasPrompt {
					t.Error("Body should not contain 'prompt' field (wrong nesting)")
				}
				if _, hasParts := body["parts"]; hasParts {
					t.Error("Body should not contain 'parts' field (old format)")
				}
				if _, hasLocation := body["location"]; hasLocation {
					t.Error("Body should not contain 'location' field (set at session creation)")
				}

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"id": "msg-123",
					},
				})
			}))
			defer server.Close()

			// Create client with v2 version
			client := NewClient(server.URL, "", "", "v2")
			// Directory parameter is ignored in v2 (set at session creation)
			err := client.SendMessage(tt.sessionID, tt.message, "/ignored/directory")
			if err != nil {
				t.Fatalf("SendMessage failed: %v", err)
			}
		})
	}
}

func TestInterruptSessionV2(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.EscapedPath() != "/api/session/session%2F123/interrupt" {
			t.Errorf("path = %s, want escaped interrupt path", r.URL.EscapedPath())
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "test-user" || password != "test-pass" {
			t.Errorf("basic auth = %q/%q/%v", username, password, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-user", "test-pass", "v2")
	if err := client.InterruptSession("session/123"); err != nil {
		t.Fatalf("InterruptSession failed: %v", err)
	}
}

func TestInterruptSessionRejectsClassic(t *testing.T) {
	client := NewClient("http://unused", "", "", "classic")
	if err := client.InterruptSession("session-123"); err == nil {
		t.Fatal("expected classic interrupt to be rejected")
	}
}

func TestCreateSessionWithBasicAuth(t *testing.T) {
	username := "testuser"
	password := "testpass"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify basic auth header
		user, pass, ok := r.BasicAuth()
		if !ok {
			t.Error("Expected basic auth header")
		}
		if user != username {
			t.Errorf("Expected username %q, got %q", username, user)
		}
		if pass != password {
			t.Errorf("Expected password %q, got %q", password, pass)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"id": map[string]interface{}{
					"value": "session-123",
				},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, username, password, "v2")
	_, err := client.CreateSession("Test Session", "")
	if err != nil {
		t.Fatalf("CreateSession with auth failed: %v", err)
	}
}

func TestHTTP405ErrorWithDiagnostics(t *testing.T) {
	// Test that 405 errors include detailed diagnostics
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET, PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"error": "Method Not Allowed", "message": "POST is not supported on this endpoint"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")
	_, err := client.CreateSession("Test", "")
	if err == nil {
		t.Fatal("Expected error for 405 response, got nil")
	}

	// Unwrap to get the underlying HTTPError
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("Expected HTTPError in error chain, got %T: %v", err, err)
	}

	// Verify error includes method
	if httpErr.Method != "POST" {
		t.Errorf("Expected method POST, got %s", httpErr.Method)
	}

	// Verify error includes full URL
	expectedURL := server.URL + "/api/session"
	if httpErr.URL != expectedURL {
		t.Errorf("Expected URL %s, got %s", expectedURL, httpErr.URL)
	}

	// Verify error includes status code
	if httpErr.StatusCode != 405 {
		t.Errorf("Expected status code 405, got %d", httpErr.StatusCode)
	}

	// Verify error includes Allow header
	if httpErr.AllowHeader != "GET, PUT" {
		t.Errorf("Expected Allow header 'GET, PUT', got %q", httpErr.AllowHeader)
	}

	// Verify error includes response body
	if !contains(httpErr.ResponseBody, "Method Not Allowed") {
		t.Errorf("Expected response body to contain 'Method Not Allowed', got %q", httpErr.ResponseBody)
	}

	// Verify error message format
	errMsg := err.Error()
	if !contains(errMsg, "POST") {
		t.Errorf("Error message should contain method: %s", errMsg)
	}
	if !contains(errMsg, "/api/session") {
		t.Errorf("Error message should contain path: %s", errMsg)
	}
	if !contains(errMsg, "405") {
		t.Errorf("Error message should contain status code: %s", errMsg)
	}
	if !contains(errMsg, "Allow: GET, PUT") {
		t.Errorf("Error message should contain Allow header: %s", errMsg)
	}
}

func TestClassicVersionCompatibility(t *testing.T) {
	// Test that classic version still works with query params
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			t.Errorf("Expected path /session for classic API, got %s", r.URL.Path)
		}

		// Classic version uses query param for directory
		if dir := r.URL.Query().Get("directory"); dir != "/tmp/test" {
			t.Errorf("Expected directory query param '/tmp/test', got %q", dir)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Session{
			ID:     "classic-session",
			Status: "active",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "classic")
	session, err := client.CreateSession("Test", "/tmp/test")
	if err != nil {
		t.Fatalf("CreateSession with classic version failed: %v", err)
	}

	if session.ID != "classic-session" {
		t.Errorf("Expected session ID 'classic-session', got %q", session.ID)
	}
}

func TestVersionDefaultsToV2(t *testing.T) {
	// Test that empty version defaults to v2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should use v2 API path
		if r.URL.Path != "/api/session" {
			t.Errorf("Expected v2 path /api/session, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"id": map[string]interface{}{
					"value": "v2-default",
				},
			},
		})
	}))
	defer server.Close()

	// Create client with empty version string
	client := NewClient(server.URL, "", "", "")
	session, err := client.CreateSession("Test", "")
	if err != nil {
		t.Fatalf("CreateSession with default version failed: %v", err)
	}

	if session.ID != "v2-default" {
		t.Errorf("Expected session ID 'v2-default', got %q", session.ID)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestIsSessionBusyV2(t *testing.T) {
	tests := []struct {
		name           string
		sessionID      string
		activeSessions map[string]SessionActive
		wantBusy       bool
		wantError      bool
	}{
		{
			name:      "session is active/busy",
			sessionID: "ses_abc123",
			activeSessions: map[string]SessionActive{
				"ses_abc123": {ID: "ses_abc123", Status: "active"},
			},
			wantBusy:  true,
			wantError: false,
		},
		{
			name:      "session is idle (not in active map)",
			sessionID: "ses_xyz789",
			activeSessions: map[string]SessionActive{
				"ses_abc123": {ID: "ses_abc123", Status: "active"},
			},
			wantBusy:  false,
			wantError: false,
		},
		{
			name:           "no active sessions at all",
			sessionID:      "ses_xyz789",
			activeSessions: map[string]SessionActive{},
			wantBusy:       false,
			wantError:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/session/active" {
					t.Errorf("Expected path /api/session/active, got %s", r.URL.Path)
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(V2ActiveSessionsResponse{
					Data: tt.activeSessions,
				})
			}))
			defer server.Close()

			client := NewClient(server.URL, "", "", "v2")
			busy, err := client.IsSessionBusy(tt.sessionID, "")

			if tt.wantError && err == nil {
				t.Fatal("Expected error but got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if busy != tt.wantBusy {
				t.Errorf("Expected busy=%v, got %v", tt.wantBusy, busy)
			}
		})
	}
}

func TestIsSessionBusyClassic(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		status    SessionStatus
		wantBusy  bool
		wantError bool
	}{
		{
			name:      "session is busy",
			sessionID: "session-123",
			status: SessionStatus{
				"session-123": SessionInfo{Status: "active", Busy: true},
			},
			wantBusy:  true,
			wantError: false,
		},
		{
			name:      "session is idle",
			sessionID: "session-123",
			status: SessionStatus{
				"session-123": SessionInfo{Status: "idle", Busy: false},
			},
			wantBusy:  false,
			wantError: false,
		},
		{
			name:      "session not in map (completed)",
			sessionID: "session-456",
			status: SessionStatus{
				"session-123": SessionInfo{Status: "idle", Busy: false},
			},
			wantBusy:  false,
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/session/status" {
					t.Errorf("Expected path /session/status, got %s", r.URL.Path)
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(tt.status)
			}))
			defer server.Close()

			client := NewClient(server.URL, "", "", "classic")
			busy, err := client.IsSessionBusy(tt.sessionID, "")

			if tt.wantError && err == nil {
				t.Fatal("Expected error but got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if busy != tt.wantBusy {
				t.Errorf("Expected busy=%v, got %v", tt.wantBusy, busy)
			}
		})
	}
}

func TestWaitForSessionIdleV2RaceCondition(t *testing.T) {
	// Test that we handle the race condition: empty active map right after prompt
	// should NOT immediately return success - must wait for session to become active first
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Simulate race condition:
		// Call 1-2: session not yet active (OpenCode hasn't started processing)
		// Call 3-4: session becomes active
		// Call 5+: session completes (not in active map)
		var response V2ActiveSessionsResponse
		if callCount <= 2 {
			// Not yet started
			response = V2ActiveSessionsResponse{Data: map[string]SessionActive{}}
		} else if callCount <= 4 {
			// Now active
			response = V2ActiveSessionsResponse{
				Data: map[string]SessionActive{
					"ses_test": {ID: "ses_test", Status: "active"},
				},
			}
		} else {
			// Completed (not in active map)
			response = V2ActiveSessionsResponse{Data: map[string]SessionActive{}}
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	var logMessages []string
	logFunc := func(msg string) {
		logMessages = append(logMessages, msg)
	}

	err := client.WaitForSessionIdle("ses_test", 30*time.Second, "", logFunc)
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}

	// Should have made at least 5 calls (waiting for active, then idle)
	if callCount < 5 {
		t.Errorf("Expected at least 5 API calls to handle race condition, got %d", callCount)
	}

	// Should have logged the transition
	if len(logMessages) < 2 {
		t.Errorf("Expected at least 2 log messages, got %d: %v", len(logMessages), logMessages)
	}
}

func TestWaitForSessionIdleV2ImmediatelyActive(t *testing.T) {
	// Test that if session is immediately active, we wait for it to complete
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Session is active on first check, then completes
		var response V2ActiveSessionsResponse
		if callCount <= 2 {
			response = V2ActiveSessionsResponse{
				Data: map[string]SessionActive{
					"ses_test": {ID: "ses_test", Status: "active"},
				},
			}
		} else {
			response = V2ActiveSessionsResponse{Data: map[string]SessionActive{}}
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	err := client.WaitForSessionIdle("ses_test", 30*time.Second, "", nil)
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}

	if callCount < 3 {
		t.Errorf("Expected at least 3 API calls, got %d", callCount)
	}
}

func TestWaitForSessionIdleV2UsesWaitEndpoint(t *testing.T) {
	var activePolls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			activePolls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{
				"ses_test": {ID: "ses_test", Status: "active"},
			}})
			return
		}
		if r.URL.Path != "/api/session/ses_test/wait" {
			t.Errorf("expected v2 wait path, got %s", r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "opencode" || password != "secret" {
			t.Errorf("expected OpenCode basic auth credentials")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(server.URL, "opencode", "secret", "v2")
	var logs []string
	err := client.WaitForSessionIdle("ses_test", time.Second, "", func(message string) {
		logs = append(logs, message)
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if activePolls == 0 {
		t.Fatal("expected the active-session guard before the wait request")
	}
	if len(logs) != 4 || !contains(logs[0], "to start") || !contains(logs[1], "now active") || !contains(logs[2], "v2 wait endpoint") || !contains(logs[3], "completed") {
		t.Fatalf("unexpected wait logs: %v", logs)
	}
}

func TestWaitForSessionIdleV2DoesNotAcceptPreExecutionIdle(t *testing.T) {
	var waitRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			waitRequests++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{}})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")
	err := client.WaitForSessionIdle("ses_test", 150*time.Millisecond, "", nil)
	if err == nil || !contains(err.Error(), "never appeared in active map") {
		t.Fatalf("expected an inactive prompt to fail instead of completing, got %v", err)
	}
	if waitRequests != 0 {
		t.Fatalf("wait endpoint was called %d times before prompt execution started", waitRequests)
	}
}

func TestWaitForSessionIdleV2WaitEndpointTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{
				"ses_test": {ID: "ses_test", Status: "active"},
			}})
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")
	client.httpClient.Timeout = 10 * time.Millisecond

	start := time.Now()
	err := client.WaitForSessionIdle("ses_test", 100*time.Millisecond, "", nil)
	if err == nil || !contains(err.Error(), "timeout waiting for session") {
		t.Fatalf("expected session timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 75*time.Millisecond {
		t.Fatalf("ordinary HTTP timeout was incorrectly used for wait request: %v", elapsed)
	}
}

func TestWaitForSessionIdleV2WaitEndpointError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{
				"ses_missing": {ID: "ses_missing", Status: "active"},
			}})
			return
		}
		http.Error(w, `{"error":"session missing"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")
	err := client.WaitForSessionIdle("ses_missing", time.Second, "", nil)
	if err == nil || !contains(err.Error(), "404") || !contains(err.Error(), "session missing") {
		t.Fatalf("expected detailed HTTP error, got %v", err)
	}
}

func TestWaitForSessionIdleV2RetriesTransientGatewayErrors(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			requestCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{
						"ses_test": {ID: "ses_test", Status: "active"},
					}})
					return
				}
				requestCount++
				if requestCount == 1 {
					http.Error(w, `{"message":"temporary gateway failure","service":"proxy"}`, statusCode)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			client := NewClient(server.URL, "", "", "v2")
			if err := client.WaitForSessionIdle("ses_test", time.Second, "", nil); err != nil {
				t.Fatalf("expected transient %d retry to succeed, got %v", statusCode, err)
			}
			if requestCount != 2 {
				t.Fatalf("expected 2 wait requests, got %d", requestCount)
			}
		})
	}
}

func TestWaitForSessionIdleV2RetriesTransportFailure(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{
				"ses_test": {ID: "ses_test", Status: "active"},
			}})
			return
		}
		requestCount++
		if requestCount == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("failed to hijack connection: %v", err)
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")
	if err := client.WaitForSessionIdle("ses_test", time.Second, "", nil); err != nil {
		t.Fatalf("expected transport retry to succeed, got %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 wait requests, got %d", requestCount)
	}
}

func TestWaitForSessionIdleV2FallbackPreservesTimeoutBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			time.Sleep(80 * time.Millisecond)
			http.Error(w, `{"name":"OperationUnavailableError","message":"operation unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		active := map[string]SessionActive{
			"ses_test": {ID: "ses_test", Status: "active"},
		}
		json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: active})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")
	start := time.Now()
	err := client.WaitForSessionIdle("ses_test", 150*time.Millisecond, "", nil)
	elapsed := time.Since(start)
	if err == nil || !contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("fallback reset the timeout budget: elapsed %v", elapsed)
	}
}

func TestWaitForSessionIdleClassic(t *testing.T) {
	// Test classic version still works
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// First 2 calls: busy, then idle
		var status SessionStatus
		if callCount <= 2 {
			status = SessionStatus{
				"session-123": SessionInfo{Status: "active", Busy: true},
			}
		} else {
			status = SessionStatus{
				"session-123": SessionInfo{Status: "idle", Busy: false},
			}
		}

		json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "classic")

	err := client.WaitForSessionIdle("session-123", 30*time.Second, "", nil)
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}

	if callCount < 3 {
		t.Errorf("Expected at least 3 API calls, got %d", callCount)
	}
}

func TestWaitForSessionIdleTimeout(t *testing.T) {
	// Test timeout is enforced
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Always return active
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(V2ActiveSessionsResponse{
			Data: map[string]SessionActive{
				"ses_test": {ID: "ses_test", Status: "active"},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	// Use very short timeout
	err := client.WaitForSessionIdle("ses_test", 3*time.Second, "", nil)
	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}

	if !contains(err.Error(), "timeout") {
		t.Errorf("Expected timeout error message, got: %v", err)
	}
}

func TestWaitForSessionIdleReusedSessionNeverActive(t *testing.T) {
	// Test reused session scenario: session never becomes active after second prompt
	// This reproduces the bug where review phase hangs when session doesn't process the prompt
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Session never appears in active map (simulating OpenCode not processing the reused session)
		response := V2ActiveSessionsResponse{Data: map[string]SessionActive{}}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	var logMessages []string
	logFunc := func(msg string) {
		logMessages = append(logMessages, msg)
		t.Logf("LOG: %s", msg)
	}

	// Use shorter timeout to make test faster
	err := client.WaitForSessionIdle("ses_reused", 10*time.Second, "", logFunc)

	// Should timeout or return an error, not hang forever
	if err == nil {
		t.Fatal("Expected timeout or error when session never becomes active, got nil")
	}

	// Should have logged about waiting
	if len(logMessages) == 0 {
		t.Error("Expected log messages during wait, got none")
	}

	t.Logf("Error (expected): %v", err)
	t.Logf("Log messages: %v", logMessages)
}

func TestWaitForSessionIdleReusedSessionStaleActive(t *testing.T) {
	// Test reused session scenario: session appears active from previous use
	// but never transitions to idle (stuck in stale state)
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		var response V2ActiveSessionsResponse
		if callCount <= 1 {
			// Appears active immediately (stale from previous use)
			response = V2ActiveSessionsResponse{
				Data: map[string]SessionActive{
					"ses_reused": {ID: "ses_reused", Status: "active"},
				},
			}
		} else {
			// But then stays in limbo (not in active map, but not actually processing)
			// This simulates the case where OpenCode thinks it's done but hasn't actually processed the new prompt
			response = V2ActiveSessionsResponse{Data: map[string]SessionActive{}}
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	var logMessages []string
	logFunc := func(msg string) {
		logMessages = append(logMessages, msg)
		t.Logf("LOG: %s", msg)
	}

	// Use shorter timeout to make test faster
	err := client.WaitForSessionIdle("ses_reused", 10*time.Second, "", logFunc)

	// This scenario is tricky - if we see it become active briefly then idle,
	// current code would treat it as complete (which might be wrong for a reused session).
	// But at minimum, it should not hang - either return success or timeout
	if err != nil {
		// Timeout is acceptable
		t.Logf("Got error (acceptable for this edge case): %v", err)
	}

	// Should have logged something
	if len(logMessages) == 0 {
		t.Error("Expected log messages during wait, got none")
	}

	t.Logf("Result: %v, logs: %v", err, logMessages)
}

func TestWaitForSessionIdleReusedSessionHangsActive(t *testing.T) {
	// Test reused session scenario that reproduces the actual bug:
	// Session appears active immediately and STAYS active forever (never completes)
	// This simulates what happens when OpenCode gets stuck processing a reused session
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Session is always active (stuck)
		response := V2ActiveSessionsResponse{
			Data: map[string]SessionActive{
				"ses_reused": {ID: "ses_reused", Status: "active"},
			},
		}

		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	var logMessages []string
	logFunc := func(msg string) {
		logMessages = append(logMessages, msg)
		t.Logf("LOG: %s", msg)
	}

	// Use shorter timeout to make test faster
	start := time.Now()
	err := client.WaitForSessionIdle("ses_reused", 12*time.Second, "", logFunc)
	elapsed := time.Since(start)

	// Should timeout, not hang forever
	if err == nil {
		t.Fatal("Expected timeout error when session stays active forever, got nil")
	}

	if !contains(err.Error(), "timeout") {
		t.Errorf("Expected timeout error, got: %v", err)
	}

	// Should have logged multiple heartbeats during the wait
	// With 12s timeout and 30s log interval, we won't see heartbeats in Phase 2
	// But we should at least see the initial logs from Phase 1
	t.Logf("Call count: %d, elapsed: %v", callCount, elapsed)
	t.Logf("Log messages (%d): %v", len(logMessages), logMessages)

	if len(logMessages) < 2 {
		t.Errorf("Expected at least 2 log messages (start + active), got %d: %v", len(logMessages), logMessages)
	}

	// Should have seen "session is now active" message
	hasActiveLog := false
	for _, msg := range logMessages {
		if contains(msg, "is now active") {
			hasActiveLog = true
			break
		}
	}
	if !hasActiveLog {
		t.Error("Expected to see 'is now active' log message")
	}
}

func TestStreamSessionEventsV2(t *testing.T) {
	tests := []struct {
		name           string
		sessionID      string
		endpointPath   string
		events         []string
		expectFiltered bool
	}{
		{
			name:         "/event endpoint works",
			sessionID:    "ses_abc123",
			endpointPath: "/event",
			events: []string{
				`{"type":"server.connected","properties":{}}`,
				`{"type":"session.status","properties":{"sessionID":"ses_abc123","status":"active"}}`,
				`{"type":"session.status","properties":{"sessionID":"other_session","status":"active"}}`,
			},
			expectFiltered: true,
		},
		{
			name:         "/global/event endpoint works",
			sessionID:    "ses_xyz",
			endpointPath: "/global/event",
			events: []string{
				`{"type":"session.next.tool.started","properties":{"sessionID":"ses_xyz"},"data":{"tool":"read_file"}}`,
			},
			expectFiltered: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receivedEvents := []SessionEvent{}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Check that it's trying the right endpoint
				if r.URL.Path != tt.endpointPath {
					http.NotFound(w, r)
					return
				}

				// Verify SSE headers
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Errorf("Expected Accept: text/event-stream header")
				}

				// Stream events
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)

				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Fatal("ResponseWriter doesn't support flushing")
				}

				for _, event := range tt.events {
					w.Write([]byte("data: " + event + "\n\n"))
					flusher.Flush()
				}
			}))
			defer server.Close()

			client := NewClient(server.URL, "", "", "v2")

			handler := func(event SessionEvent) error {
				receivedEvents = append(receivedEvents, event)
				if len(receivedEvents) >= 2 {
					// Stop after receiving expected events
					return fmt.Errorf("done")
				}
				return nil
			}

			stopFunc, err := client.StreamSessionEvents(tt.sessionID, handler)
			if err != nil {
				t.Fatalf("StreamSessionEvents failed: %v", err)
			}
			defer stopFunc()

			// Wait a bit for events to arrive
			time.Sleep(200 * time.Millisecond)

			// Verify we received events
			if len(receivedEvents) == 0 {
				t.Fatal("Expected to receive events, got none")
			}

			// Verify filtering if expected
			if tt.expectFiltered {
				for _, event := range receivedEvents {
					if event.Type == "server.connected" {
						continue // server.connected is always allowed
					}
					// Check that sessionID matches if present
					if sid, ok := event.Properties["sessionID"].(string); ok {
						if sid != tt.sessionID {
							t.Errorf("Expected filtered events for session %s, but got event for %s", tt.sessionID, sid)
						}
					}
				}
			}
		})
	}
}

func TestStreamSessionEventsEndpointFallback(t *testing.T) {
	// Test that client tries multiple endpoints and succeeds with fallback
	sessionID := "ses_test"
	attempts := []string{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts = append(attempts, r.URL.Path)

		// First 3 endpoints return 404
		if len(attempts) <= 3 {
			http.NotFound(w, r)
			return
		}

		// Fourth endpoint succeeds
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"server.connected\"}\n\n"))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	stopFunc, err := client.StreamSessionEvents(sessionID, func(event SessionEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Expected success with fallback, got error: %v", err)
	}
	defer stopFunc()

	// Verify it tried multiple endpoints
	if len(attempts) < 2 {
		t.Errorf("Expected multiple endpoint attempts, got %d: %v", len(attempts), attempts)
	}
}

func TestStreamSessionEventsAllEndpointsFail(t *testing.T) {
	// Test that client returns error when all endpoints fail
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	_, err := client.StreamSessionEvents("ses_test", func(event SessionEvent) error {
		return nil
	})

	if err == nil {
		t.Fatal("Expected error when all endpoints fail, got nil")
	}

	if !contains(err.Error(), "failed") {
		t.Errorf("Expected error message about failure, got: %v", err)
	}
}

func TestStreamSessionEventsClassicUnsupported(t *testing.T) {
	// Test that classic version returns error
	client := NewClient("http://localhost", "", "", "classic")

	_, err := client.StreamSessionEvents("session-123", func(event SessionEvent) error {
		return nil
	})

	if err == nil {
		t.Fatal("Expected error for classic version, got nil")
	}

	if !contains(err.Error(), "only supported for OpenCode v2") {
		t.Errorf("Expected unsupported error message, got: %v", err)
	}
}

func TestAuthorizationHeaderOnActiveSessionsEndpoint(t *testing.T) {
	// Test that 401 is returned when auth is missing, and 200 when auth is correct
	username := "opencode"
	password := "test-password"

	tests := []struct {
		name         string
		clientUser   string
		clientPass   string
		wantStatus   int
		wantAuthSent bool
	}{
		{
			name:         "no auth credentials - 401",
			clientUser:   "",
			clientPass:   "",
			wantStatus:   401,
			wantAuthSent: false,
		},
		{
			name:         "correct auth credentials - 200",
			clientUser:   username,
			clientPass:   password,
			wantStatus:   200,
			wantAuthSent: true,
		},
		{
			name:         "wrong password - 401",
			clientUser:   username,
			clientPass:   "wrong-password",
			wantStatus:   401,
			wantAuthSent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authHeaderReceived := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/session/active" {
					t.Errorf("Expected path /api/session/active, got %s", r.URL.Path)
				}

				// Check for Authorization header
				user, pass, ok := r.BasicAuth()
				if ok {
					authHeaderReceived = true
					// Verify credentials
					if user != username || pass != password {
						w.WriteHeader(http.StatusUnauthorized)
						w.Write([]byte(`{"error": "Invalid credentials"}`))
						return
					}
				} else {
					// No auth header
					if tt.wantAuthSent {
						t.Error("Expected Authorization header but none was sent")
					}
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte(`{"error": "Authorization required"}`))
					return
				}

				// Success response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(V2ActiveSessionsResponse{
					Data: map[string]SessionActive{},
				})
			}))
			defer server.Close()

			client := NewClient(server.URL, tt.clientUser, tt.clientPass, "v2")
			_, err := client.IsSessionBusy("ses_test", "")

			if tt.wantStatus == 401 {
				if err == nil {
					t.Fatal("Expected 401 error, got nil")
				}
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) {
					t.Fatalf("Expected HTTPError, got %T: %v", err, err)
				}
				if httpErr.StatusCode != 401 {
					t.Errorf("Expected status 401, got %d", httpErr.StatusCode)
				}
			} else {
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
			}

			if authHeaderReceived != tt.wantAuthSent {
				t.Errorf("Expected authHeaderReceived=%v, got %v", tt.wantAuthSent, authHeaderReceived)
			}
		})
	}
}

func TestAuthorizationHeaderOnAllEndpoints(t *testing.T) {
	// Test that Authorization header is sent on all request types
	username := "opencode"
	password := "test-password"

	tests := []struct {
		name     string
		testFunc func(*testing.T, *Client, *httptest.Server) error
	}{
		{
			name: "CreateSession sends auth",
			testFunc: func(t *testing.T, client *Client, server *httptest.Server) error {
				_, err := client.CreateSession("Test", "")
				return err
			},
		},
		{
			name: "SendMessage sends auth",
			testFunc: func(t *testing.T, client *Client, server *httptest.Server) error {
				return client.SendMessage("ses_123", "test message", "")
			},
		},
		{
			name: "IsSessionBusy sends auth",
			testFunc: func(t *testing.T, client *Client, server *httptest.Server) error {
				_, err := client.IsSessionBusy("ses_123", "")
				return err
			},
		},
		{
			name: "HealthCheck sends auth",
			testFunc: func(t *testing.T, client *Client, server *httptest.Server) error {
				_, err := client.HealthCheck()
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authReceived := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Check for Authorization header
				user, pass, ok := r.BasicAuth()
				if !ok {
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte(`{"error": "Authorization required"}`))
					return
				}

				authReceived = true

				if user != username || pass != password {
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte(`{"error": "Invalid credentials"}`))
					return
				}

				// Success response based on path
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				switch {
				case contains(r.URL.Path, "/api/session/active"):
					json.NewEncoder(w).Encode(V2ActiveSessionsResponse{Data: map[string]SessionActive{}})
				case contains(r.URL.Path, "/api/session"):
					json.NewEncoder(w).Encode(map[string]interface{}{
						"data": map[string]interface{}{"id": "ses_123"},
					})
				case contains(r.URL.Path, "/api/health"):
					json.NewEncoder(w).Encode(HealthResponse{Healthy: true})
				default:
					json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
				}
			}))
			defer server.Close()

			client := NewClient(server.URL, username, password, "v2")
			err := tt.testFunc(t, client, server)

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if !authReceived {
				t.Error("Authorization header was not sent")
			}
		})
	}
}

func TestStreamSessionEventsWithAuth(t *testing.T) {
	// Test that SSE endpoint receives Authorization header
	username := "opencode"
	password := "test-password"

	authReceived := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for Authorization header
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": "Authorization required"}`))
			return
		}

		authReceived = true

		if user != username || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": "Invalid credentials"}`))
			return
		}

		// Success - send SSE response
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"type\":\"server.connected\"}\n\n"))
	}))
	defer server.Close()

	client := NewClient(server.URL, username, password, "v2")

	stopFunc, err := client.StreamSessionEvents("ses_test", func(event SessionEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamSessionEvents failed: %v", err)
	}
	defer stopFunc()

	// Give it a moment to connect
	time.Sleep(100 * time.Millisecond)

	if !authReceived {
		t.Error("Authorization header was not sent to SSE endpoint")
	}
}

func TestStreamSessionEventsWithoutAuth401(t *testing.T) {
	// Test that SSE endpoint fails without auth
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject without auth
		_, _, ok := r.BasicAuth()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": "Authorization required"}`))
			return
		}
		// Should not reach here in this test
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "", "v2")

	_, err := client.StreamSessionEvents("ses_test", func(event SessionEvent) error {
		return nil
	})

	if err == nil {
		t.Fatal("Expected error for SSE without auth, got nil")
	}

	if !contains(err.Error(), "401") && !contains(err.Error(), "failed") {
		t.Errorf("Expected 401 or connection failure in error, got: %v", err)
	}
}
