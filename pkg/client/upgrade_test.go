package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpgradeWaitVerifiesRestartHealth(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "old instance"}[mismatch], func(t *testing.T) {
			polls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					instance := "new"
					if mismatch {
						instance = "old"
					}
					json.NewEncoder(w).Encode(HealthResponse{Healthy: true, Revision: "sha", InstanceID: instance})
					return
				}
				polls++
				if polls == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				json.NewEncoder(w).Encode(UpgradeStatus{ID: "upgrade", Phase: "complete", TargetRevision: "sha", PreviousInstanceID: "old", InstanceID: "new"})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := NewClient(server.URL, "token").WaitForUpgrade(ctx, "upgrade", time.Millisecond, nil)
			if mismatch && err == nil {
				t.Fatal("old process reported successful restart")
			}
			if !mismatch && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpgradeWaitReportsPersistedFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UpgradeStatus{ID: "id", Phase: "failed", Error: "build failed"})
	}))
	defer server.Close()
	if _, err := NewClient(server.URL, "").WaitForUpgrade(context.Background(), "id", time.Millisecond, nil); err == nil {
		t.Fatal("failure hidden")
	}
}

func TestClientNotifiesOnServerInstanceChange(t *testing.T) {
	instance := "first"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Devbox-Instance", instance)
		w.Header().Set("X-Devbox-Revision", "sha")
		json.NewEncoder(w).Encode(HealthResponse{Healthy: true})
	}))
	defer server.Close()
	c := NewClient(server.URL, "")
	changes := 0
	c.SetServerRestartHandler(func(revision, id string) {
		changes++
		if revision != "sha" || id != "second" {
			t.Errorf("bad notification: %s %s", revision, id)
		}
	})
	if _, err := c.Health(); err != nil {
		t.Fatal(err)
	}
	instance = "second"
	if _, err := c.Health(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Fatalf("restart notifications: %d", changes)
	}
}

func TestUpgradeWaitDoesNotRetryAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := NewClient(server.URL, "").WaitForUpgrade(ctx, "id", time.Millisecond, nil); err == nil || ctx.Err() != nil {
		t.Fatalf("authentication was retried until timeout: %v", err)
	}
}
