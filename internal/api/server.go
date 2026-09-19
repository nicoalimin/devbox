package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/nicoalimin/devbox/internal/buildinfo"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
	"github.com/nicoalimin/devbox/internal/upgrade"
)

const Version = buildinfo.Version

// Server represents the HTTP API server
type Server struct {
	cfg          *config.Config
	db           *db.DB
	orchestrator *job.Orchestrator
	upgrader     *upgrade.Manager
	instanceID   string
	httpServer   *http.Server
}

// NewServer creates a new API server
func NewServer(cfg *config.Config, database *db.DB, orch *job.Orchestrator) *Server {
	return &Server{
		cfg:          cfg,
		db:           database,
		orchestrator: orch,
		instanceID:   uuid.NewString(),
		httpServer:   &http.Server{ReadHeaderTimeout: 10 * time.Second},
	}
}

func (s *Server) InstanceID() string                   { return s.instanceID }
func (s *Server) SetUpgrader(manager *upgrade.Manager) { s.upgrader = manager }
func (s *Server) Shutdown(ctx context.Context) error   { return s.httpServer.Shutdown(ctx) }

// Start starts the HTTP server
func (s *Server) Start() error {
	return s.StartWithLogger(nil)
}

// StartWithLogger starts the HTTP server with optional custom logger
func (s *Server) StartWithLogger(customLogger func(string, ...interface{})) error {
	listener, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return err
	}

	return s.ServeWithLogger(listener, customLogger)
}

// ServeWithLogger serves the HTTP API on an already-bound listener. Accepting
// the listener separately lets callers surface bind failures before starting a
// full-screen UI or other long-running foreground work.
func (s *Server) ServeWithLogger(listener net.Listener, customLogger func(string, ...interface{})) error {
	s.httpServer.Handler = s.Router(customLogger)
	err := s.httpServer.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Router(customLogger func(string, ...interface{})) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Devbox-Revision", buildinfo.Revision)
			w.Header().Set("X-Devbox-Instance", s.instanceID)
			next.ServeHTTP(w, r)
		})
	})

	// Middleware
	if customLogger != nil {
		// Custom logger middleware that goes to the log buffer
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
				t1 := time.Now()

				defer func() {
					elapsed := time.Since(t1)
					customLogger("%s %s %s from %s - %d in %s",
						r.Method,
						r.URL.String(),
						r.Proto,
						r.RemoteAddr,
						ww.Status(),
						elapsed)
				}()

				next.ServeHTTP(ww, r)
			})
		})
	} else {
		r.Use(middleware.Logger)
	}
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	// Public routes
	r.Get("/health", s.handleHealth)

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)

		r.Get("/v1/status", s.handleStatus)
		r.Get("/v1/upgrade", s.handleUpgradeStatus)
		r.Post("/v1/upgrade", s.handleUpgrade)
		r.Post("/v1/jobs", s.handleCreateJob)
		r.Get("/v1/jobs", s.handleListJobs)
		r.Get("/v1/jobs/{id}", s.handleGetJob)
		r.Get("/v1/blockers", s.handleGetBlockers)
		r.Post("/v1/jobs/{id}/reply", s.handleReplyToJob)
		r.Post("/v1/jobs/{id}/review", s.handleReviewJob)
		r.Post("/v1/jobs/{id}/cancel", s.handleCancelJob)
		r.Get("/v1/jobs/{id}/logs", s.handleGetLogs)
	})

	if customLogger == nil {
		fmt.Printf("Starting devboxd server on %s\n", s.cfg.Server.Listen)
	} else {
		customLogger("Starting devboxd server on %s", s.cfg.Server.Listen)
	}
	return r
}

// authMiddleware validates the bearer token
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			s.writeError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		expectedToken := "Bearer " + s.cfg.Server.AuthToken
		if token != expectedToken {
			s.writeError(w, http.StatusUnauthorized, "invalid authorization token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleHealth handles health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"healthy":    true,
		"version":    Version,
		"revision":   buildinfo.Revision,
		"instanceId": s.instanceID,
	})
}

// handleStatus handles status requests
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	currentJob, err := s.db.GetCurrentJob()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get current job: %v", err))
		return
	}

	response := map[string]interface{}{
		"version":    Version,
		"busy":       currentJob != nil,
		"revision":   buildinfo.Revision,
		"instanceId": s.instanceID,
		"draining":   s.orchestrator.IsDraining(),
	}

	if currentJob != nil {
		response["currentJobId"] = currentJob.ID
		response["currentJobState"] = currentJob.State
	}

	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.upgrader == nil || !s.upgrader.Enabled() {
		s.writeError(w, http.StatusServiceUnavailable, "self-upgrade is not configured")
		return
	}
	state, err := s.upgrader.Start()
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, http.StatusAccepted, state)
}

func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	if s.upgrader == nil {
		s.writeJSON(w, http.StatusOK, upgrade.State{Phase: "disabled"})
		return
	}
	state, err := s.upgrader.Status()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, state)
}

// CreateJobRequest represents a request to create a job
type CreateJobRequest struct {
	LinearIssueID   string `json:"linearIssueId"`
	OperatorContext string `json:"operatorContext,omitempty"`
}

// handleCreateJob handles job creation requests
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req CreateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.LinearIssueID == "" {
		s.writeError(w, http.StatusBadRequest, "linearIssueId is required")
		return
	}

	job, err := s.orchestrator.CreateJob(req.LinearIssueID, req.OperatorContext)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusCreated, job)
}

// handleListJobs handles job listing requests
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 10
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	jobs, err := s.db.ListJobs(limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list jobs: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs": jobs,
	})
}

// handleGetJob handles individual job requests
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")

	job, err := s.db.GetJob(jobID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get job: %v", err))
		return
	}
	if job == nil {
		s.writeError(w, http.StatusNotFound, "job not found")
		return
	}

	s.writeJSON(w, http.StatusOK, job)
}

// handleGetBlockers handles blocked jobs requests
func (s *Server) handleGetBlockers(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.db.GetBlockedJobs()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get blocked jobs: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"blockers": jobs,
	})
}

// ReplyRequest represents a reply to a blocked job
type ReplyRequest struct {
	Message string `json:"message"`
}

// ReviewRequest represents review feedback for a job
type ReviewRequest struct {
	Feedback string `json:"feedback"`
}

// handleReplyToJob handles job reply requests
func (s *Server) handleReplyToJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")

	var req ReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Message == "" {
		s.writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	if err := s.orchestrator.ReplyToJob(jobID, req.Message); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "reply sent",
	})
}

// handleReviewJob handles review feedback requests
func (s *Server) handleReviewJob(w http.ResponseWriter, r *http.Request) {
	jobIDOrLinearID := chi.URLParam(r, "id")

	var req ReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Feedback == "" {
		s.writeError(w, http.StatusBadRequest, "feedback is required")
		return
	}

	// Validate job exists before accepting the review request
	// Try by job ID first, then by Linear issue ID
	job, err := s.db.GetJob(jobIDOrLinearID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get job: %v", err))
		return
	}
	if job == nil {
		// Try finding by Linear issue ID
		job, err = s.db.GetJobByLinearIssueID(jobIDOrLinearID)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get job by Linear ID: %v", err))
			return
		}
		if job == nil {
			s.writeError(w, http.StatusNotFound, fmt.Sprintf("job not found: %s", jobIDOrLinearID))
			return
		}
	}

	// Validate job has a PR (can't review without one)
	if job.PRURL == "" {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("job does not have a pull request yet (state: %s)", job.State))
		return
	}

	// Persist and reserve the review before acknowledging it. Worker errors
	// are recorded on the job, so accepted requests cannot disappear silently.
	accepted, err := s.orchestrator.StartReview(job.ID, req.Feedback)
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Return 202 Accepted immediately
	s.writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"success": true,
		"message": "review feedback accepted and processing",
		"jobId":   accepted.ID,
		"state":   accepted.State,
	})
}

// handleCancelJob handles job cancellation requests
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")

	if err := s.orchestrator.CancelJob(jobID); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "job cancelled",
	})
}

// handleGetLogs handles job logs requests
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")

	tailStr := r.URL.Query().Get("tail")
	tail := 0
	if tailStr != "" {
		if t, err := strconv.Atoi(tailStr); err == nil && t > 0 {
			tail = t
		}
	}

	logs, err := s.db.GetLogs(jobID, tail)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get logs: %v", err))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"logs": logs,
	})
}

// writeJSON writes a JSON response
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes an error response
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]interface{}{
		"error": message,
	})
}
