package opencode

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server for OpenCode2 API
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request method and path
				if r.Method != "POST" {
					t.Errorf("Expected POST method, got %s", r.Method)
				}
				expectedPath := "/api/session/" + tt.sessionID + "/prompt"
				if r.URL.Path != expectedPath {
					t.Errorf("Expected path %s, got %s", expectedPath, r.URL.Path)
				}

				// Verify request body matches OpenCode2 v2 API
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("Failed to decode request body: %v", err)
				}

				// Verify prompt object exists
				prompt, ok := body["prompt"].(map[string]interface{})
				if !ok {
					t.Fatalf("Expected 'prompt' object in body, got %v", body)
				}

				// Verify text field
				text, ok := prompt["text"].(string)
				if !ok {
					t.Fatalf("Expected 'text' field in prompt, got %v", prompt)
				}
				if text != tt.message {
					t.Errorf("Expected text %q, got %q", tt.message, text)
				}

				// Verify old 'parts' format is NOT used
				if _, hasParts := body["parts"]; hasParts {
					t.Error("Body should not contain 'parts' field (old format)")
				}

				// Verify location is NOT in body (set at session creation)
				if _, hasLocation := body["location"]; hasLocation {
					t.Error("Body should not contain 'location' field (set at session creation)")
				}

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"id": map[string]interface{}{
							"value": "msg-123",
						},
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
