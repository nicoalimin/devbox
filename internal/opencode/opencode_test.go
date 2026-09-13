package opencode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestCreateSession(t *testing.T) {
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
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request method and path
				if r.Method != "POST" {
					t.Errorf("Expected POST method, got %s", r.Method)
				}
				if r.URL.Path != "/session" {
					t.Errorf("Expected path /session, got %s", r.URL.Path)
				}

				// Verify query parameter
				if tt.wantDirectory != "" {
					dirParam := r.URL.Query().Get("directory")
					if dirParam != tt.wantDirectory {
						t.Errorf("Expected directory query param %q, got %q", tt.wantDirectory, dirParam)
					}
				}

				// Verify request body
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("Failed to decode request body: %v", err)
				}

				if title, ok := body["title"].(string); !ok || title != tt.wantTitle {
					t.Errorf("Expected title %q in body, got %v", tt.wantTitle, body["title"])
				}

				// Check that "name" is not in the body (old field)
				if _, hasName := body["name"]; hasName {
					t.Error("Body should not contain 'name' field, should use 'title'")
				}

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(Session{
					ID:     "session-123",
					Status: "active",
				})
			}))
			defer server.Close()

			// Create client and test
			client := NewClient(server.URL, "", "")
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

func TestSendMessage(t *testing.T) {
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
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request method and path
				if r.Method != "POST" {
					t.Errorf("Expected POST method, got %s", r.Method)
				}
				expectedPath := "/session/" + tt.sessionID + "/message"
				if r.URL.Path != expectedPath {
					t.Errorf("Expected path %s, got %s", expectedPath, r.URL.Path)
				}

				// Verify query parameter
				if tt.wantDirectory != "" {
					dirParam := r.URL.Query().Get("directory")
					if dirParam != tt.wantDirectory {
						t.Errorf("Expected directory query param %q, got %q", tt.wantDirectory, dirParam)
					}
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

				// Send response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"info": map[string]string{
						"id":   "msg-123",
						"role": "assistant",
					},
				})
			}))
			defer server.Close()

			// Create client and test
			client := NewClient(server.URL, "", "")
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
		json.NewEncoder(w).Encode(Session{ID: "session-123", Status: "active"})
	}))
	defer server.Close()

	client := NewClient(server.URL, username, password)
	_, err := client.CreateSession("Test Session", "")
	if err != nil {
		t.Fatalf("CreateSession with auth failed: %v", err)
	}
}

func TestHTTP405Error(t *testing.T) {
	// Test that 405 errors are properly reported
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("Method Not Allowed"))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "")
	_, err := client.CreateSession("Test", "")
	if err == nil {
		t.Fatal("Expected error for 405 response, got nil")
	}

	// Verify error message contains status code
	expectedMsg := "API returned status 405"
	if !contains(err.Error(), expectedMsg) {
		t.Errorf("Expected error to contain %q, got %q", expectedMsg, err.Error())
	}
}

func TestURLEncoding(t *testing.T) {
	// Test that special characters in directory paths are properly encoded
	directory := "/tmp/work tree/ENG-123"
	expectedEncoded := url.QueryEscape(directory)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check raw query encoding
		expectedQuery := "directory=" + expectedEncoded
		if r.URL.RawQuery != expectedQuery {
			t.Errorf("Expected raw query %q, got %q", expectedQuery, r.URL.RawQuery)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Session{ID: "session-123", Status: "active"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "")
	_, err := client.CreateSession("Test", directory)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
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
