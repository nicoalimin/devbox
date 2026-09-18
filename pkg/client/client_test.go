package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReviewWaitReportsDeliveryOutcome(t *testing.T) {
	for _, finalState := range []string{"pr_open", "failed", "cancelled", "blocked"} {
		t.Run(finalState, func(t *testing.T) {
			polls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					w.WriteHeader(http.StatusAccepted)
					json.NewEncoder(w).Encode(ReviewAcceptance{JobID: "canonical-id", State: "reviewing"})
					return
				}
				if r.URL.Path != "/v1/jobs/canonical-id" {
					t.Errorf("wrong job: %s", r.URL.Path)
				}
				polls++
				state := "pushing"
				if polls > 1 {
					state = finalState
				}
				json.NewEncoder(w).Encode(Job{ID: "canonical-id", State: state, BlockerReason: "validation failed; checkpoint pushed"})
			}))
			defer server.Close()
			client := NewClient(server.URL, "token")
			accepted, err := client.StartReview("TEST-1", "Fix it")
			if err != nil {
				t.Fatal(err)
			}
			job, err := client.WaitForReview(context.Background(), accepted.JobID, time.Millisecond)
			if job == nil || job.State != finalState {
				t.Fatalf("job: %+v, err: %v", job, err)
			}
			if finalState == "pr_open" && err != nil {
				t.Fatal(err)
			}
			if finalState != "pr_open" && (err == nil || !strings.Contains(err.Error(), "checkpoint pushed")) {
				t.Fatalf("failure hidden: %v", err)
			}
		})
	}
}

func TestReviewWaitCanBeCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Job{State: "reviewing"})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := NewClient(server.URL, "token").WaitForReview(ctx, "job", time.Millisecond); err == nil {
		t.Fatal("wait did not stop")
	}
}

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
