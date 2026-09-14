package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client handles OpenCode API interactions
type Client struct {
	baseURL    string
	version    string // "v2" or "classic"
	username   string
	password   string
	httpClient *http.Client
}

// NewClient creates a new OpenCode API client
// version should be "v2" (for OpenCode2 beta) or "classic" (for legacy OpenCode)
func NewClient(baseURL, username, password, version string) *Client {
	if version == "" {
		version = "v2"
	}
	return &Client{
		baseURL:  baseURL,
		version:  version,
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

// SessionStatus represents the status of sessions (classic API)
type SessionStatus map[string]SessionInfo

// SessionInfo represents information about a session (classic API)
type SessionInfo struct {
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// V2ActiveSessionsResponse represents the /api/session/active response
type V2ActiveSessionsResponse struct {
	Data map[string]SessionActive `json:"data"`
}

// SessionActive represents an active session in OpenCode2
type SessionActive struct {
	ID     interface{} `json:"id"`
	Status string      `json:"status,omitempty"`
	// Other fields can be added as needed
}

// Session represents an OpenCode session
type Session struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// V2SessionResponse wraps the session info in OpenCode2 format
type V2SessionResponse struct {
	Data SessionData `json:"data"`
	ID   interface{} `json:"id"` // Some responses may have top-level ID
}

// SessionData represents OpenCode2 session data
type SessionData struct {
	ID interface{} `json:"id"`
	// Other fields omitted for brevity
}

// MessagePart represents a part of a message
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// HTTPError provides detailed HTTP error information
type HTTPError struct {
	Method       string
	URL          string
	StatusCode   int
	Status       string
	ResponseBody string
	AllowHeader  string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("%s %s returned %d %s", e.Method, e.URL, e.StatusCode, e.Status)
	if e.AllowHeader != "" {
		msg += fmt.Sprintf(" (Allow: %s)", e.AllowHeader)
	}
	if e.ResponseBody != "" {
		msg += fmt.Sprintf(": %s", e.ResponseBody)
	}
	return msg
}

// SessionEvent represents an OpenCode session event from the SSE stream
type SessionEvent struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
	Data       map[string]interface{} `json:"data"`
}

// HealthCheck checks if OpenCode is healthy
func (c *Client) HealthCheck() (bool, error) {
	var health HealthResponse
	
	// OpenCode2 uses /api/health, classic uses /global/health
	healthPath := "/global/health"
	if c.version == "v2" {
		healthPath = "/api/health"
	}
	
	if err := c.get(healthPath, &health); err != nil {
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
func (c *Client) CreateSession(title, directory string) (*Session, error) {
	if c.version == "v2" {
		return c.createSessionV2(title, directory)
	}
	return c.createSessionClassic(title, directory)
}

// createSessionV2 creates a session using OpenCode2 API
func (c *Client) createSessionV2(title, directory string) (*Session, error) {
	body := map[string]interface{}{
		"title": title,
	}
	
	// Add location with directory for OpenCode2
	if directory != "" {
		body["location"] = map[string]interface{}{
			"directory": directory,
		}
	}

	var response V2SessionResponse
	if err := c.post("/api/session", body, &response); err != nil {
		return nil, fmt.Errorf("failed to create OpenCode2 session: %w", err)
	}

	// Extract session ID robustly from various OpenCode2 response formats
	sessionID := c.extractSessionID(response)

	if sessionID == "" {
		// Include raw response body in error for debugging
		responseBytes, _ := json.Marshal(response)
		responseStr := string(responseBytes)
		if len(responseStr) > 200 {
			responseStr = responseStr[:200] + "..."
		}
		return nil, fmt.Errorf("OpenCode2 returned empty session ID (response: %s)", responseStr)
	}

	return &Session{
		ID:     sessionID,
		Status: "active",
	}, nil
}

// extractSessionID extracts session ID from various OpenCode2 response formats
func (c *Client) extractSessionID(response V2SessionResponse) string {
	// Try 1: data.id as plain string (most common)
	if idStr, ok := response.Data.ID.(string); ok && idStr != "" {
		return idStr
	}

	// Try 2: data.id as object with "value" or "id" field
	if idMap, ok := response.Data.ID.(map[string]interface{}); ok {
		if id, ok := idMap["value"].(string); ok && id != "" {
			return id
		}
		if id, ok := idMap["id"].(string); ok && id != "" {
			return id
		}
	}

	// Try 3: top-level id (no data wrapper)
	if idStr, ok := response.ID.(string); ok && idStr != "" {
		return idStr
	}

	// Try 4: top-level id as object
	if idMap, ok := response.ID.(map[string]interface{}); ok {
		if id, ok := idMap["value"].(string); ok && id != "" {
			return id
		}
		if id, ok := idMap["id"].(string); ok && id != "" {
			return id
		}
	}

	return ""
}

// createSessionClassic creates a session using classic OpenCode API
func (c *Client) createSessionClassic(title, directory string) (*Session, error) {
	body := map[string]interface{}{
		"title": title,
	}

	path := "/session"
	if directory != "" {
		path += "?directory=" + directory
	}

	var session Session
	if err := c.post(path, body, &session); err != nil {
		return nil, fmt.Errorf("failed to create classic OpenCode session: %w", err)
	}

	return &session, nil
}

// SendMessage sends a message to a session
func (c *Client) SendMessage(sessionID, message, directory string) error {
	if c.version == "v2" {
		return c.sendMessageV2(sessionID, message, directory)
	}
	return c.sendMessageClassic(sessionID, message, directory)
}

// IsSessionBusy checks if a specific session is busy (version-aware)
// For v2: session is busy if present in /api/session/active map
// For classic: session is busy if status shows busy field = true
// directory parameter is optional and only used for v2 instance-scoped endpoints
func (c *Client) IsSessionBusy(sessionID string, directory string) (bool, error) {
	if c.version == "v2" {
		return c.isSessionBusyV2(sessionID, directory)
	}
	return c.isSessionBusyClassic(sessionID)
}

// isSessionBusyV2 checks if a session is busy using OpenCode2 /api/session/active
func (c *Client) isSessionBusyV2(sessionID string, directory string) (bool, error) {
	path := "/api/session/active"
	
	// Add directory header if provided (for instance-scoped routing)
	var response V2ActiveSessionsResponse
	if directory != "" {
		// For now, directory routing is handled at session creation
		// If we need per-request routing, we'd add x-opencode-directory header here
		// or ?directory= query parameter
	}
	
	if err := c.get(path, &response); err != nil {
		return false, fmt.Errorf("failed to get active sessions: %w", err)
	}
	
	// Session is busy if it's present in the active map
	_, isActive := response.Data[sessionID]
	return isActive, nil
}

// isSessionBusyClassic checks if a session is busy using classic /session/status
func (c *Client) isSessionBusyClassic(sessionID string) (bool, error) {
	status, err := c.GetSessionStatus()
	if err != nil {
		return false, err
	}
	
	sessionInfo, exists := status[sessionID]
	if !exists {
		// Session not in status map - could mean completed or not started yet
		// Return false (not busy) - caller should handle this case
		return false, nil
	}
	
	return sessionInfo.Busy, nil
}

// WaitForSessionIdle polls the session status until it becomes idle or timeout
// Handles race condition: waits for session to become active before checking for idle
// directory parameter is optional and used for v2 instance-scoped routing
// logFunc is called periodically with status updates (can be nil)
func (c *Client) WaitForSessionIdle(sessionID string, timeout time.Duration, directory string, logFunc func(string)) error {
	start := time.Now()
	pollInterval := 5 * time.Second
	logInterval := 10 * time.Second // Reduced from 30s to 10s for more frequent heartbeats
	lastLog := time.Now()
	
	// Phase 1: Wait for session to become active (race condition protection)
	// For v2: after sending prompt, OpenCode may take a moment to start processing
	// Don't treat "not in active map" as idle until we've seen it become active at least once
	sawActive := false
	warmupDeadline := start.Add(30 * time.Second) // Give it 30s to start
	warmupLogTimer := time.Now() // Separate timer for warmup phase logging
	
	if logFunc != nil {
		logFunc(fmt.Sprintf("Waiting for OpenCode session %s to start (version: %s)", sessionID, c.version))
	}
	
	for !sawActive && time.Now().Before(warmupDeadline) {
		if time.Now().After(start.Add(timeout)) {
			return fmt.Errorf("timeout waiting for session %s to start after %v", sessionID, timeout)
		}
		
		busy, err := c.IsSessionBusy(sessionID, directory)
		if err != nil {
			// Log error but continue - might be temporary
			if logFunc != nil && time.Now().Sub(warmupLogTimer) >= logInterval {
				logFunc(fmt.Sprintf("Warning: failed to check session status: %v", err))
				warmupLogTimer = time.Now()
			}
			time.Sleep(pollInterval)
			continue
		}
		
		if busy {
			sawActive = true
			if logFunc != nil {
				logFunc(fmt.Sprintf("OpenCode session %s is now active, waiting for completion", sessionID))
			}
			break
		}
		
		// Not yet active, log progress periodically during warmup
		if logFunc != nil && time.Now().Sub(warmupLogTimer) >= logInterval {
			warmupElapsed := time.Since(start)
			logFunc(fmt.Sprintf("Still waiting for session %s to start (elapsed: %v)", sessionID, warmupElapsed))
			warmupLogTimer = time.Now()
		}
		
		// Not yet active, wait a bit
		time.Sleep(pollInterval)
	}
	
	// If we never saw the session become active after warmup period, that's a problem
	// For reused sessions, this likely means OpenCode didn't process the new prompt
	if !sawActive {
		if logFunc != nil {
			logFunc(fmt.Sprintf("Warning: session %s never appeared in active map after %v warmup", sessionID, time.Since(start)))
		}
		// Continue to Phase 2 but with a shorter timeout since something is likely wrong
		// If it's truly done quickly, Phase 2 will detect it and return success
		// If it's stuck, Phase 2 will timeout
	}
	
	// Phase 2: Wait for session to become idle
	lastLog = time.Now()
	loopCount := 0
	for {
		loopCount++
		elapsed := time.Since(start)
		if elapsed >= timeout {
			// Provide context about what we saw during the wait
			state := "never became active"
			if sawActive {
				state = "became active but never completed"
			}
			return fmt.Errorf("timeout waiting for session %s after %v (%s, checked %d times)", 
				sessionID, elapsed, state, loopCount)
		}
		
		busy, err := c.IsSessionBusy(sessionID, directory)
		if err != nil {
			// Log error but continue - might be temporary
			if logFunc != nil && time.Now().Sub(lastLog) >= logInterval {
				logFunc(fmt.Sprintf("Warning: failed to check session status: %v (elapsed: %v)", err, elapsed))
				lastLog = time.Now()
			}
			time.Sleep(pollInterval)
			continue
		}
		
		// Session is idle only if we saw it active before OR it's not in the map
		if !busy {
			if sawActive || c.version == "classic" {
				// For v2: idle after being active = done
				// For classic: not busy = idle
				if logFunc != nil {
					logFunc(fmt.Sprintf("OpenCode session %s completed after %v", sessionID, elapsed))
				}
				return nil
			}
			// For v2: not active yet in Phase 2
			// This is a weird state - we're past warmup but still haven't seen it active
			// Log this more prominently and eventually timeout
			if logFunc != nil && time.Now().Sub(lastLog) >= logInterval {
				logFunc(fmt.Sprintf("Session %s still not active after %v (may not have started processing)", sessionID, elapsed))
				lastLog = time.Now()
			}
		} else {
			// Session is busy - normal state, log progress periodically
			if logFunc != nil && time.Now().Sub(lastLog) >= logInterval {
				logFunc(fmt.Sprintf("Still waiting for OpenCode session %s (elapsed: %v)", sessionID, elapsed))
				lastLog = time.Now()
			}
		}
		
		time.Sleep(pollInterval)
	}
}

// sendMessageV2 sends a message using OpenCode2 /api/session/{sessionID}/prompt
func (c *Client) sendMessageV2(sessionID, message, directory string) error {
	// OpenCode2 beta-19135 expects FLAT structure with text at root level:
	//   {"text": "message"}
	// NOT nested: {"prompt": {"text": "message"}}
	// NOT parts: {"parts": [{"type": "text", "text": "message"}]}
	body := map[string]interface{}{
		"text": message,
	}

	// Directory is NOT sent in prompt body - it's set at session creation
	
	var result map[string]interface{}
	path := fmt.Sprintf("/api/session/%s/prompt", sessionID)
	err := c.post(path, body, &result)
	
	// Log request body on failure for debugging
	if err != nil {
		bodyBytes, _ := json.Marshal(body)
		return fmt.Errorf("%w (request body: %s)", err, string(bodyBytes))
	}
	
	return nil
}

// sendMessageClassic sends a message using classic OpenCode API
func (c *Client) sendMessageClassic(sessionID, message, directory string) error {
	body := map[string]interface{}{
		"parts": []MessagePart{
			{Type: "text", Text: message},
		},
	}

	path := fmt.Sprintf("/session/%s/message", sessionID)
	if directory != "" {
		path += "?directory=" + directory
	}

	var result map[string]interface{}
	return c.post(path, body, &result)
}

// get performs a GET request
func (c *Client) get(path string, result interface{}) error {
	fullURL := c.baseURL + path
	req, err := http.NewRequest("GET", fullURL, nil)
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

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return &HTTPError{
			Method:       "GET",
			URL:          fullURL,
			StatusCode:   resp.StatusCode,
			Status:       resp.Status,
			ResponseBody: strings.TrimSpace(string(respBody)),
			AllowHeader:  resp.Header.Get("Allow"),
		}
	}

	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("failed to decode response: %w (body: %s)", err, string(respBody))
	}

	return nil
}

// post performs a POST request
func (c *Client) post(path string, body interface{}, result interface{}) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	fullURL := c.baseURL + path
	req, err := http.NewRequest("POST", fullURL, bytes.NewBuffer(bodyBytes))
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

	// Read response body for error reporting
	respBody, _ := io.ReadAll(resp.Body)
	
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &HTTPError{
			Method:       "POST",
			URL:          fullURL,
			StatusCode:   resp.StatusCode,
			Status:       resp.Status,
			ResponseBody: strings.TrimSpace(string(respBody)),
			AllowHeader:  resp.Header.Get("Allow"),
		}
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to decode response: %w (body: %s)", err, string(respBody))
		}
	}

	return nil
}

// StreamSessionEvents subscribes to session events via SSE and calls the handler for each event.
// Returns a stop function to terminate the stream and any subscription errors.
// The handler receives parsed events and should return quickly to avoid blocking the stream.
// For OpenCode2, this connects to the global event stream and filters events by sessionID.
func (c *Client) StreamSessionEvents(sessionID string, handler func(SessionEvent) error) (stopFunc func(), err error) {
	if c.version != "v2" {
		return nil, fmt.Errorf("event streaming only supported for OpenCode v2")
	}

	// OpenCode2 SSE endpoints to try, in order of preference
	// The API uses /event or /global/event, NOT /api/session/{id}/event
	paths := []string{
		"/event",        // Directory-scoped (preferred if available)
		"/global/event", // Global event stream
		"/api/event",    // Alternative with /api prefix
		"/api/global/event",
	}

	var resp *http.Response
	var fullURL string
	var lastErr error

	// Use a new client with no timeout for SSE
	sseClient := &http.Client{
		Timeout: 0, // No timeout for long-lived SSE connection
	}

	// Try each endpoint until one succeeds
	for _, path := range paths {
		fullURL = c.baseURL + path

		req, err := http.NewRequest("GET", fullURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		if c.username != "" && c.password != "" {
			req.SetBasicAuth(c.username, c.password)
		}

		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		resp, err = sseClient.Do(req)
		if err != nil {
			lastErr = err
			continue // Try next endpoint
		}

		if resp.StatusCode == http.StatusOK {
			// Success! Use this endpoint
			break
		}

		// Non-200 response, try next endpoint
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastErr = &HTTPError{
			Method:       "GET",
			URL:          fullURL,
			StatusCode:   resp.StatusCode,
			Status:       resp.Status,
			ResponseBody: strings.TrimSpace(string(respBody)),
		}
	}

	// If all endpoints failed, return the last error
	if resp == nil || resp.StatusCode != http.StatusOK {
		if lastErr != nil {
			return nil, fmt.Errorf("all SSE endpoints failed, last error: %w", lastErr)
		}
		return nil, fmt.Errorf("failed to connect to any SSE endpoint")
	}

	// Create stop channel and cleanup function
	stopCh := make(chan struct{})
	stopped := false
	var mu sync.Mutex

	stopFunc = func() {
		mu.Lock()
		defer mu.Unlock()
		if !stopped {
			stopped = true
			close(stopCh)
			resp.Body.Close()
		}
	}

	// Start goroutine to read SSE stream
	go func() {
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)

		for {
			select {
			case <-stopCh:
				return
			default:
			}

			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					// Stream error, call handler with error event
					handler(SessionEvent{
						Type: "stream.error",
						Data: map[string]interface{}{
							"error": err.Error(),
						},
					})
				}
				return
			}

			line = strings.TrimSpace(line)

			// Skip empty lines and comments
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}

			// Parse SSE data line
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				
				var event SessionEvent
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					// Skip malformed events
					continue
				}

				// Filter events by sessionID
				// OpenCode2 global event stream includes events for all sessions
				// Check properties.sessionID or properties.session.id
				eventSessionID := ""
				if sid, ok := event.Properties["sessionID"].(string); ok {
					eventSessionID = sid
				} else if sid, ok := event.Properties["sessionId"].(string); ok {
					eventSessionID = sid
				} else if sessionObj, ok := event.Properties["session"].(map[string]interface{}); ok {
					if sid, ok := sessionObj["id"].(string); ok {
						eventSessionID = sid
					}
				}

				// Skip events that don't match our sessionID (unless it's server.connected)
				if eventSessionID != "" && eventSessionID != sessionID && event.Type != "server.connected" {
					continue
				}

				// Call handler for matching events
				if err := handler(event); err != nil {
					// Handler error, stop streaming
					stopFunc()
					return
				}
			}
		}
	}()

	return stopFunc, nil
}
