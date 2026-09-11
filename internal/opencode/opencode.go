package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client handles OpenCode API interactions
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient creates a new OpenCode API client
func NewClient(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Healthy bool `json:"healthy"`
}

// SessionStatus represents the status of sessions
type SessionStatus map[string]SessionInfo

// SessionInfo represents information about a session
type SessionInfo struct {
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// Session represents an OpenCode session
type Session struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// MessagePart represents a part of a message
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// HealthCheck checks if OpenCode is healthy
func (c *Client) HealthCheck() (bool, error) {
	var health HealthResponse
	if err := c.get("/global/health", &health); err != nil {
		return false, err
	}
	return health.Healthy, nil
}

// GetSessionStatus retrieves the status of all sessions
func (c *Client) GetSessionStatus() (SessionStatus, error) {
	var status SessionStatus
	if err := c.get("/session/status", &status); err != nil {
		return nil, err
	}
	return status, nil
}

// CreateSession creates a new OpenCode session
func (c *Client) CreateSession(name string) (*Session, error) {
	body := map[string]interface{}{
		"name": name,
	}

	var session Session
	if err := c.post("/session", body, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// SendMessage sends a message to a session
func (c *Client) SendMessage(sessionID, message string) error {
	body := map[string]interface{}{
		"parts": []MessagePart{
			{Type: "text", Text: message},
		},
	}

	var result map[string]interface{}
	return c.post(fmt.Sprintf("/session/%s/message", sessionID), body, &result)
}

// get performs a GET request
func (c *Client) get(path string, result interface{}) error {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	return nil
}

// post performs a POST request
func (c *Client) post(path string, body interface{}, result interface{}) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}
