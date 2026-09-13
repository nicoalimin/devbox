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

func TestSendMessageV2(t *testing.T) {
	tests := []struct {
		name          string
		sessionID     string
		message       string
		directory     string
		wantDirectory string
	}{
		{
			name:          "send message with directory",
			sessionID:     "session-123",
			message:       "Fix the authentication bug",
			directory:     "/tmp/worktree/ENG-123",
			wantDirectory: "/tmp/worktree/ENG-123",
		},
		{
			name:          "send message without directory",
			sessionID:     "session-456",
			message:       "Add tests",
			directory:     "",
			wantDirectory: "",
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

				// Verify request body
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("Failed to decode request body: %v", err)
				}

				parts, ok := body["parts"].([]interface{})
				if !ok || len(parts) != 1 {
					t.Fatalf("Expected parts array with 1 element, got %v", body["parts"])
				}

				part := parts[0].(map[string]interface{})
				if part["type"] != "text" {
					t.Errorf("Expected part type 'text', got %v", part["type"])
				}
				if part["text"] != tt.message {
					t.Errorf("Expected part text %q, got %v", tt.message, part["text"])
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
			err := client.SendMessage(tt.sessionID, tt.message, tt.directory)
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
