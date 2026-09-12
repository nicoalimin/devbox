package linear

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const apiURL = "https://api.linear.app/graphql"

// Client handles Linear API interactions
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new Linear API client
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Issue represents a Linear issue
type Issue struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Priority    int       `json:"priority"`
	State       State     `json:"state"`
	Team        Team      `json:"team"`
	Project     *Project  `json:"project"`
	Labels      []Label   `json:"labels"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// State represents an issue state
type State struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Team represents a Linear team
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Project represents a Linear project
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Label represents an issue label
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetIssue fetches a Linear issue by ID or identifier
func (c *Client) GetIssue(idOrIdentifier string) (*Issue, error) {
	query := `
		query GetIssue($id: String!) {
			issue(id: $id) {
				id
				identifier
				title
				description
				url
				priority
				state {
					id
					name
					type
				}
				team {
					id
					name
					key
				}
				project {
					id
					name
				}
				labels {
					nodes {
						id
						name
					}
				}
				createdAt
				updatedAt
			}
		}
	`

	variables := map[string]interface{}{
		"id": idOrIdentifier,
	}

	// Use intermediate struct to handle connection pattern
	var result struct {
		Data struct {
			Issue struct {
				ID          string    `json:"id"`
				Identifier  string    `json:"identifier"`
				Title       string    `json:"title"`
				Description string    `json:"description"`
				URL         string    `json:"url"`
				Priority    int       `json:"priority"`
				State       State     `json:"state"`
				Team        Team      `json:"team"`
				Project     *Project  `json:"project"`
				Labels      struct {
					Nodes []Label `json:"nodes"`
				} `json:"labels"`
				CreatedAt   time.Time `json:"createdAt"`
				UpdatedAt   time.Time `json:"updatedAt"`
			} `json:"issue"`
		} `json:"data"`
	}

	if err := c.query(query, variables, &result); err != nil {
		return nil, err
	}

	// Convert to Issue struct
	issue := &Issue{
		ID:          result.Data.Issue.ID,
		Identifier:  result.Data.Issue.Identifier,
		Title:       result.Data.Issue.Title,
		Description: result.Data.Issue.Description,
		URL:         result.Data.Issue.URL,
		Priority:    result.Data.Issue.Priority,
		State:       result.Data.Issue.State,
		Team:        result.Data.Issue.Team,
		Project:     result.Data.Issue.Project,
		Labels:      result.Data.Issue.Labels.Nodes,
		CreatedAt:   result.Data.Issue.CreatedAt,
		UpdatedAt:   result.Data.Issue.UpdatedAt,
	}

	return issue, nil
}

// UpdateIssueState updates the state of an issue
func (c *Client) UpdateIssueState(issueID, stateID string) error {
	mutation := `
		mutation UpdateIssue($id: String!, $stateId: String!) {
			issueUpdate(id: $id, input: { stateId: $stateId }) {
				success
				issue {
					id
					state {
						name
					}
				}
			}
		}
	`

	variables := map[string]interface{}{
		"id":      issueID,
		"stateId": stateID,
	}

	var result struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}

	if err := c.query(mutation, variables, &result); err != nil {
		return err
	}

	if !result.Data.IssueUpdate.Success {
		return fmt.Errorf("failed to update issue state")
	}

	return nil
}

// AddComment adds a comment to an issue
func (c *Client) AddComment(issueID, body string) error {
	mutation := `
		mutation CreateComment($issueId: String!, $body: String!) {
			commentCreate(input: { issueId: $issueId, body: $body }) {
				success
				comment {
					id
				}
			}
		}
	`

	variables := map[string]interface{}{
		"issueId": issueID,
		"body":    body,
	}

	var result struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}

	if err := c.query(mutation, variables, &result); err != nil {
		return err
	}

	if !result.Data.CommentCreate.Success {
		return fmt.Errorf("failed to create comment")
	}

	return nil
}

// GetWorkflowStates retrieves workflow states for a team
func (c *Client) GetWorkflowStates(teamID string) ([]State, error) {
	query := `
		query GetWorkflowStates($teamId: String!) {
			team(id: $teamId) {
				states {
					nodes {
						id
						name
						type
					}
				}
			}
		}
	`

	variables := map[string]interface{}{
		"teamId": teamID,
	}

	var result struct {
		Data struct {
			Team struct {
				States struct {
					Nodes []State `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"data"`
	}

	if err := c.query(query, variables, &result); err != nil {
		return nil, err
	}

	return result.Data.Team.States.Nodes, nil
}

// FindStateByName finds a state ID by name for a team
func (c *Client) FindStateByName(teamID, stateName string) (string, error) {
	states, err := c.GetWorkflowStates(teamID)
	if err != nil {
		return "", err
	}

	for _, state := range states {
		if state.Name == stateName {
			return state.ID, nil
		}
	}

	return "", fmt.Errorf("state %q not found for team", stateName)
}

// query executes a GraphQL query
func (c *Client) query(query string, variables map[string]interface{}, result interface{}) error {
	reqBody := map[string]interface{}{
		"query":     query,
		"variables": variables,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

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
