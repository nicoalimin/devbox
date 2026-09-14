package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
)

func TestHandleReviewJob_NotFound(t *testing.T) {
	// Create temporary database
	dbPath := "test_review_api.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create test config
	cfg := &config.Config{
		Server: config.ServerConfig{
			AuthToken: "test-token",
		},
	}

	// Create orchestrator and server
	orch := job.NewOrchestrator(cfg, database)
	server := NewServer(cfg, database, orch)

	// Create chi router to handle path parameters properly
	r := chi.NewRouter()
	r.Post("/v1/jobs/{id}/review", server.handleReviewJob)

	tests := []struct {
		name           string
		jobID          string
		expectedStatus int
	}{
		{
			name:           "Non-existent job ID",
			jobID:          "non-existent-job-id",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "Non-existent Linear ID",
			jobID:          "UTA-999",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create request
			body := map[string]string{
				"feedback": "Please fix the bug",
			}
			bodyBytes, _ := json.Marshal(body)

			req := httptest.NewRequest("POST", "/v1/jobs/"+tt.jobID+"/review", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			// Create response recorder
			w := httptest.NewRecorder()

			// Use chi router to handle the request
			r.ServeHTTP(w, req)

			// Check status code
			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			// Check that error response contains "job not found"
			var resp map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("Failed to parse response: %v", err)
			}

			if errorMsg, ok := resp["error"].(string); ok {
				if !strings.Contains(errorMsg, "job not found") {
					t.Errorf("Expected error containing 'job not found', got '%s'", errorMsg)
				}
			} else {
				t.Errorf("Expected error in response, got: %v", resp)
			}
		})
	}
}

func TestHandleReviewJob_NoPR(t *testing.T) {
	// Create temporary database
	dbPath := "test_review_no_pr.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create a job without a PR
	testJob := &db.Job{
		ID:            "test-job-1",
		LinearIssueID: "ENG-123",
		State:         db.StateCoding,
		PRURL:         "", // No PR yet
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(testJob); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Server: config.ServerConfig{
			AuthToken: "test-token",
		},
	}

	// Create orchestrator and server
	orch := job.NewOrchestrator(cfg, database)
	server := NewServer(cfg, database, orch)

	// Create chi router
	r := chi.NewRouter()
	r.Post("/v1/jobs/{id}/review", server.handleReviewJob)

	// Create request
	body := map[string]string{
		"feedback": "Please fix the bug",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/jobs/test-job-1/review", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	w := httptest.NewRecorder()

	// Use chi router
	r.ServeHTTP(w, req)

	// Should return 400 Bad Request
	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d. Response: %s", w.Code, w.Body.String())
	}

	// Check error message
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if errorMsg, ok := resp["error"].(string); !ok || !strings.Contains(errorMsg, "pull request") {
		t.Errorf("Expected error message about missing PR, got: %v", resp)
	}
}

func TestHandleReviewJob_Success(t *testing.T) {
	// Create temporary database
	dbPath := "test_review_success.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create a job with a PR
	testJob := &db.Job{
		ID:            "test-job-2",
		LinearIssueID: "ENG-124",
		State:         db.StatePROpen,
		PRURL:         "https://github.com/test/repo/pull/1",
		BranchName:    "eng-124-feature",
		RepoPath:      "/tmp/test-repo",
		WorktreePath:  "/tmp/test-worktree",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(testJob); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Server: config.ServerConfig{
			AuthToken: "test-token",
		},
	}

	// Create orchestrator and server
	orch := job.NewOrchestrator(cfg, database)
	server := NewServer(cfg, database, orch)

	// Create chi router
	r := chi.NewRouter()
	r.Post("/v1/jobs/{id}/review", server.handleReviewJob)

	// Create request
	body := map[string]string{
		"feedback": "Please fix the bug",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/jobs/test-job-2/review", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	w := httptest.NewRecorder()

	// Use chi router
	r.ServeHTTP(w, req)

	// Should return 202 Accepted
	if w.Code != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d. Response: %s", w.Code, w.Body.String())
	}

	// Check response body
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if success, ok := resp["success"].(bool); !ok || !success {
		t.Errorf("Expected success=true, got: %v", resp)
	}

	if message, ok := resp["message"].(string); !ok || !strings.Contains(message, "accepted") {
		t.Errorf("Expected success message containing 'accepted', got: %v", resp)
	}
}

func TestHandleStatus_NotBusyWhenPROpen(t *testing.T) {
	// Create temporary database
	dbPath := "test_status_pr_open.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create a job in pr_open state
	testJob := &db.Job{
		ID:            "job-pr-open",
		LinearIssueID: "ENG-456",
		State:         db.StatePROpen,
		PRURL:         "https://github.com/test/repo/pull/1",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(testJob); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Server: config.ServerConfig{
			AuthToken: "test-token",
		},
	}

	// Create orchestrator and server
	orch := job.NewOrchestrator(cfg, database)
	server := NewServer(cfg, database, orch)

	// Create request
	req := httptest.NewRequest("GET", "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")

	// Create response recorder
	w := httptest.NewRecorder()

	// Handle request
	server.handleStatus(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Parse response
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	// Check that busy is false (pr_open should not keep server busy)
	if busy, ok := resp["busy"].(bool); !ok || busy {
		t.Errorf("Expected busy=false when job is in pr_open state, got: %v", resp)
	}

	// currentJobId and currentJobState should not be present
	if _, ok := resp["currentJobId"]; ok {
		t.Errorf("Expected no currentJobId when not busy, got: %v", resp)
	}
}

func TestHandleStatus_NotBusyWhenBlocked(t *testing.T) {
	// Create temporary database
	dbPath := "test_status_blocked.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create a job in blocked state
	testJob := &db.Job{
		ID:            "job-blocked",
		LinearIssueID: "ENG-789",
		State:         db.StateBlocked,
		BlockerReason: "Waiting for clarification",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(testJob); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Server: config.ServerConfig{
			AuthToken: "test-token",
		},
	}

	// Create orchestrator and server
	orch := job.NewOrchestrator(cfg, database)
	server := NewServer(cfg, database, orch)

	// Create request
	req := httptest.NewRequest("GET", "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")

	// Create response recorder
	w := httptest.NewRecorder()

	// Handle request
	server.handleStatus(w, req)

	// Check status code
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Parse response
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	// Check that busy is false (blocked should not keep server busy)
	if busy, ok := resp["busy"].(bool); !ok || busy {
		t.Errorf("Expected busy=false when job is blocked, got: %v", resp)
	}
}

func TestHandleCreateJob_AllowedAfterPROpen(t *testing.T) {
	// Create temporary database
	dbPath := "test_assign_after_pr_open.db"
	defer os.Remove(dbPath)

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Create a job in pr_open state
	existingJob := &db.Job{
		ID:            "job-1",
		LinearIssueID: "ENG-100",
		State:         db.StatePROpen,
		PRURL:         "https://github.com/test/repo/pull/1",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := database.CreateJob(existingJob); err != nil {
		t.Fatalf("Failed to create existing job: %v", err)
	}

	// Create test config (minimal, since we're not actually running jobs)
	cfg := &config.Config{
		Server: config.ServerConfig{
			AuthToken: "test-token",
		},
		Queue: config.QueueConfig{
			Enabled: false,
		},
	}

	// Create orchestrator and server
	orch := job.NewOrchestrator(cfg, database)
	server := NewServer(cfg, database, orch)

	// Create request to assign a new job
	body := map[string]string{
		"linearIssueId": "ENG-200",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	w := httptest.NewRecorder()

	// Handle request
	server.handleCreateJob(w, req)

	// Should return 201 Created (not 500 "server busy")
	if w.Code != http.StatusCreated {
		t.Errorf("Expected status 201, got %d. Response: %s", w.Code, w.Body.String())
	}

	// Parse response
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	// Verify we got a job back with the correct Linear issue ID
	if linearIssueID, ok := resp["linear_issue_id"].(string); !ok || linearIssueID != "ENG-200" {
		t.Errorf("Expected linear_issue_id=ENG-200, got: %v", resp)
	}
}
