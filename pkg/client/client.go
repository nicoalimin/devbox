package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client represents the devbox API client
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new API client
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
}

// StatusResponse represents the status response
type StatusResponse struct {
	Version         string `json:"version"`
	Busy            bool   `json:"busy"`
	CurrentJobID    string `json:"currentJobId,omitempty"`
	CurrentJobState string `json:"currentJobState,omitempty"`
}

// Job represents a job
type Job struct {
	ID                string     `json:"id"`
	LinearIssueID     string     `json:"linear_issue_id"`
	LinearURL         string     `json:"linear_url"`
	State             string     `json:"state"`
	RepoPath          string     `json:"repo_path"`
	BranchName        string     `json:"branch_name"`
	WorktreePath      string     `json:"worktree_path"`
	PRURL             string     `json:"pr_url"`
	BlockerReason     string     `json:"blocker_reason"`
	OpenCodeSessionID string     `json:"opencode_session_id"`
	OperatorContext   string     `json:"operator_context"`
	ReviewFeedback    string     `json:"review_feedback"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	CompletedAt       *time.Time `json:"completed_at"`
}

// JobLog represents a log entry
type JobLog struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

// Health checks server health
func (c *Client) Health() (*HealthResponse, error) {
	var resp HealthResponse
	if err := c.get("/health", &resp, false); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Status gets server status
func (c *Client) Status() (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.get("/v1/status", &resp, true); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Assign creates a new job with optional operator context
func (c *Client) Assign(linearIssueID string, operatorContext string) (*Job, error) {
	body := map[string]interface{}{
		"linearIssueId": linearIssueID,
	}
	if operatorContext != "" {
		body["operatorContext"] = operatorContext
	}
	
	var job Job
	if err := c.post("/v1/jobs", body, &job, true); err != nil {
		return nil, err
	}
	return &job, nil
}

// ListJobs lists jobs
func (c *Client) ListJobs(limit int) ([]*Job, error) {
	path := fmt.Sprintf("/v1/jobs?limit=%d", limit)
	
	var resp struct {
		Jobs []*Job `json:"jobs"`
	}
	if err := c.get(path, &resp, true); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// GetJob gets a specific job
func (c *Client) GetJob(jobID string) (*Job, error) {
	var job Job
	if err := c.get(fmt.Sprintf("/v1/jobs/%s", jobID), &job, true); err != nil {
		return nil, err
	}
	return &job, nil
}

// GetBlockers gets blocked jobs
func (c *Client) GetBlockers() ([]*Job, error) {
	var resp struct {
		Blockers []*Job `json:"blockers"`
	}
	if err := c.get("/v1/blockers", &resp, true); err != nil {
		return nil, err
	}
	return resp.Blockers, nil
}

// Reply sends a reply to a blocked job
func (c *Client) Reply(jobID, message string) error {
	body := map[string]string{
		"message": message,
	}
	
	var resp map[string]interface{}
	return c.post(fmt.Sprintf("/v1/jobs/%s/reply", jobID), body, &resp, true)
}

// Review sends review feedback to a job
func (c *Client) Review(jobIDOrLinearID, feedback string) error {
	body := map[string]string{
		"feedback": feedback,
	}
	
	var resp map[string]interface{}
	return c.post(fmt.Sprintf("/v1/jobs/%s/review", jobIDOrLinearID), body, &resp, true)
}

// Cancel cancels a job
func (c *Client) Cancel(jobID string) error {
	var resp map[string]interface{}
	return c.post(fmt.Sprintf("/v1/jobs/%s/cancel", jobID), nil, &resp, true)
}

// GetLogs gets job logs
func (c *Client) GetLogs(jobID string, tail int) ([]*JobLog, error) {
	path := fmt.Sprintf("/v1/jobs/%s/logs", jobID)
	if tail > 0 {
		path += fmt.Sprintf("?tail=%d", tail)
	}
	
	var resp struct {
		Logs []*JobLog `json:"logs"`
	}
	if err := c.get(path, &resp, true); err != nil {
		return nil, err
	}
	return resp.Logs, nil
}

// get performs a GET request
func (c *Client) get(path string, result interface{}, auth bool) error {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	return nil
}

// post performs a POST request
func (c *Client) post(path string, body interface{}, result interface{}, auth bool) error {
	var reqBody io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewBuffer(bodyBytes)
	}

	req, err := http.NewRequest("POST", c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}
