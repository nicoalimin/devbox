package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/job"
)

const Version = "0.1.0"

// Server represents the HTTP API server
type Server struct {
	cfg          *config.Config
	db           *db.DB
	orchestrator *job.Orchestrator
}

// NewServer creates a new API server
func NewServer(cfg *config.Config, database *db.DB, orch *job.Orchestrator) *Server {
	return &Server{
		cfg:          cfg,
		db:           database,
		orchestrator: orch,
	}
}

// Start starts the HTTP server
func (s *Server) Start() error {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	// Public routes
	r.Get("/health", s.handleHealth)

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)

		r.Get("/v1/status", s.handleStatus)
		r.Post("/v1/jobs", s.handleCreateJob)
		r.Get("/v1/jobs", s.handleListJobs)
		r.Get("/v1/jobs/{id}", s.handleGetJob)
		r.Get("/v1/blockers", s.handleGetBlockers)
		r.Post("/v1/jobs/{id}/reply", s.handleReplyToJob)
		r.Post("/v1/jobs/{id}/cancel", s.handleCancelJob)
		r.Get("/v1/jobs/{id}/logs", s.handleGetLogs)
	})

	fmt.Printf("Starting devboxd server on %s\n", s.cfg.Server.Listen)
	return http.ListenAndServe(s.cfg.Server.Listen, r)
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
		"healthy": true,
		"version": Version,
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
		"version": Version,
		"busy":    currentJob != nil,
	}

	if currentJob != nil {
		response["currentJobId"] = currentJob.ID
		response["currentJobState"] = currentJob.State
	}

	s.writeJSON(w, http.StatusOK, response)
}

// CreateJobRequest represents a request to create a job
type CreateJobRequest struct {
	LinearIssueID string `json:"linearIssueId"`
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

	job, err := s.orchestrator.CreateJob(req.LinearIssueID)
	if err != nil {
		if err.Error() == fmt.Sprintf("server busy with job %s", job.ID) {
			s.writeError(w, http.StatusConflict, err.Error())
			return
		}
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
