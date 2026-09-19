package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/nicoalimin/devbox/internal/buildinfo"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
)

func TestUpgradeRequiresAuthAndOptIn(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	cfg := &config.Config{Server: config.ServerConfig{AuthToken: "secret"}}
	server := NewServer(cfg, database, job.NewOrchestrator(cfg, database))
	router := server.Router(nil)
	for _, auth := range []bool{false, true} {
		req := httptest.NewRequest("POST", "/v1/upgrade", nil)
		if auth {
			req.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		want := http.StatusUnauthorized
		if auth {
			want = http.StatusServiceUnavailable
		}
		if w.Code != want {
			t.Fatalf("auth=%v: %d", auth, w.Code)
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	var health map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["revision"] != buildinfo.Revision || health["instanceId"] != server.InstanceID() || w.Header().Get("X-Devbox-Instance") != server.InstanceID() {
		t.Fatalf("missing restart signal: %v", health)
	}
}
