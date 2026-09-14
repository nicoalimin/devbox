package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientHandles202Accepted(t *testing.T) {
	// Create a test server that returns 202 Accepted
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST request, got %s", r.Method)
		}
		
		if r.URL.Path == "/v1/jobs/test-job/review" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "review feedback accepted and processing",
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Create client
	client := NewClient(server.URL, "test-token")

	// Test Review endpoint (which returns 202)
	err := client.Review("test-job", "Please fix the bug")
	if err != nil {
		t.Errorf("Review should succeed with 202 Accepted, got error: %v", err)
	}
}

func TestClientHandles200And201(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"200 OK", http.StatusOK},
		{"201 Created", http.StatusCreated},
		{"202 Accepted", http.StatusAccepted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": true,
				})
			}))
			defer server.Close()

			client := NewClient(server.URL, "test-token")

			// Test with Reply endpoint (arbitrary POST endpoint)
			err := client.Reply("test-job", "test message")
			if err != nil {
				t.Errorf("Expected success for status %d, got error: %v", tt.statusCode, err)
			}
		})
	}
}

func TestClientRejectsOtherStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"400 Bad Request", http.StatusBadRequest},
		{"401 Unauthorized", http.StatusUnauthorized},
		{"404 Not Found", http.StatusNotFound},
		{"500 Internal Server Error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": "test error",
				})
			}))
			defer server.Close()

			client := NewClient(server.URL, "test-token")

			// Test with Reply endpoint
			err := client.Reply("test-job", "test message")
			if err == nil {
				t.Errorf("Expected error for status %d, got nil", tt.statusCode)
			}
		})
	}
}
