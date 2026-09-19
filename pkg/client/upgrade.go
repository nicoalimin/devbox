package client

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type UpgradeStatus struct {
	ID                 string `json:"id"`
	Phase              string `json:"phase"`
	FromRevision       string `json:"fromRevision"`
	TargetRevision     string `json:"targetRevision"`
	PreviousInstanceID string `json:"previousInstanceId"`
	InstanceID         string `json:"instanceId"`
	Error              string `json:"error"`
	ClientAction       string `json:"clientAction"`
}

func (c *Client) Upgrade() (*UpgradeStatus, error) {
	var state UpgradeStatus
	if err := c.post("/v1/upgrade", nil, &state, true); err != nil {
		return nil, err
	}
	return &state, nil
}

func (c *Client) UpgradeStatus() (*UpgradeStatus, error) {
	var state UpgradeStatus
	if err := c.get("/v1/upgrade", &state, true); err != nil {
		return nil, err
	}
	return &state, nil
}

// WaitForUpgrade tolerates the connection gap during exec and only reports
// success after an HTTP health check proves the requested revision is running.
func (c *Client) WaitForUpgrade(ctx context.Context, id string, interval time.Duration, progress func(string)) (*UpgradeStatus, error) {
	if interval <= 0 {
		interval = time.Second
	}
	lastPhase := ""
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("stopped waiting for upgrade %s; check devbox upgrade --status (last error: %v): %w", id, lastErr, err)
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var state UpgradeStatus
		err := c.getContext(requestCtx, "/v1/upgrade", &state, true)
		cancel()
		var responseErr *ResponseError
		if errors.As(err, &responseErr) && responseErr.StatusCode < 500 {
			return nil, err
		}
		if err == nil {
			if state.ID != id {
				return nil, fmt.Errorf("upgrade status changed to another request; expected %s, got %s", id, state.ID)
			}
			if state.Phase != lastPhase && progress != nil {
				progress(state.Phase)
			}
			lastPhase = state.Phase
			if state.Phase == "failed" {
				return &state, fmt.Errorf("upgrade failed: %s", state.Error)
			}
			if state.Phase == "complete" || state.Phase == "up_to_date" {
				var health HealthResponse
				requestCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
				err = c.getContext(requestCtx, "/health", &health, false)
				cancel()
				if err == nil && health.Healthy && health.Revision == state.TargetRevision && health.InstanceID == state.InstanceID && (state.Phase == "up_to_date" || health.InstanceID != state.PreviousInstanceID) {
					return &state, nil
				}
				if err == nil {
					err = fmt.Errorf("server health does not match upgrade revision and instance")
				}
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}
