package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"computer-use/domain"
	"computer-use/store"
)

// Server holds the HTTP mux and the RunManager.
type Server struct {
	manager *RunManager
	store   *store.Store
}

// NewServer creates and returns a configured HTTP server.
func NewServer(manager *RunManager, s *store.Store) *Server {
	return &Server{manager: manager, store: s}
}

// Handler returns the HTTP handler for the server.
// Uses Go 1.22+ pattern matching for path parameters.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", s.handleStartRun)
	mux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	mux.HandleFunc("POST /runs/{id}/resume", s.handleResumeRun)
	mux.HandleFunc("GET /capabilities/{id}", s.handleGetCapability)
	return mux
}

// ── POST /runs ────────────────────────────────────────────────────────────────

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req StartRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	runID, err := s.manager.StartRun(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	log.Printf("started run %s", runID)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id":   runID,
		"status":   string(domain.RunStatusRunning),
		"poll_url": "/runs/" + runID,
	})
}

// ── GET /runs/{id} ────────────────────────────────────────────────────────────

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	// Check live in-memory state first (run may still be running).
	if state, ok := s.manager.GetState(runID); ok {
		state.mu.RLock()
		run := state.Run
		escalation := state.Escalation
		state.mu.RUnlock()

		resp := buildRunResponse(run, escalation)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Fall back to persisted runs.
	run, err := s.store.GetRun(runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, buildRunResponse(run, nil))
}

// ── POST /runs/{id}/resume ───────────────────────────────────────────────────

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	var req ResumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Resolution == "" {
		writeError(w, http.StatusBadRequest, "'resolution' is required: approved | completed_manually | skip | abort")
		return
	}

	if err := s.manager.Resume(runID, req); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	log.Printf("run %s resumed with resolution=%q", runID, req.Resolution)
	writeJSON(w, http.StatusOK, map[string]string{
		"run_id":     runID,
		"resolution": req.Resolution,
		"status":     "resumed",
	})
}

// ── GET /capabilities/{id} ───────────────────────────────────────────────────

func (s *Server) handleGetCapability(w http.ResponseWriter, r *http.Request) {
	capID := r.PathValue("id")

	cap, err := s.store.GetCapability(capID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "capability not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, cap)
}

// ── Response helpers ─────────────────────────────────────────────────────────

// RunResponse is the shape returned by GET /runs/:id.
type RunResponse struct {
	RunID        string              `json:"run_id"`
	CapabilityID string              `json:"capability_id"`
	Status       domain.RunStatus    `json:"status"`
	Params       map[string]any      `json:"params,omitempty"`
	Output       map[string]string   `json:"output,omitempty"`
	Evidence     *EvidenceResponse   `json:"evidence,omitempty"`
	Error        *ErrorResponse      `json:"error,omitempty"`
	Escalation   *domain.Escalation  `json:"escalation,omitempty"`
}

// EvidenceResponse summarises what happened.
type EvidenceResponse struct {
	Screenshots  []string `json:"screenshots"`
	StepsPassed  int      `json:"steps_passed"`
	StepsFailed  int      `json:"steps_failed"`
	StepsSkipped int      `json:"steps_skipped"`
	StepsTotal   int      `json:"steps_total"`
	DurationMs   int64    `json:"duration_ms"`
}

// ErrorResponse surfaces the error taxonomy.
type ErrorResponse struct {
	Category    domain.ErrorCategory `json:"category,omitempty"`
	StepID      string               `json:"step_id,omitempty"`
	Message     string               `json:"message"`
	IsRetryable bool                 `json:"is_retryable"`
}

func buildRunResponse(run *domain.Run, escalation *domain.Escalation) RunResponse {
	resp := RunResponse{
		RunID:        run.RunID,
		CapabilityID: run.CapabilityID,
		Status:       run.Status,
		Params:       run.Params,
		Escalation:   escalation,
	}

	if run.ErrorClass != nil {
		resp.Error = &ErrorResponse{
			Category:    run.ErrorClass.Category,
			StepID:      run.ErrorClass.StepID,
			Message:     run.ErrorClass.Message,
			IsRetryable: run.ErrorClass.IsRetryable,
		}
	}

	ev := &EvidenceResponse{
		Screenshots: []string{},
		StepsTotal:  len(run.Steps),
		DurationMs:  run.FinishedAt.Sub(run.StartedAt).Milliseconds(),
	}

	for _, step := range run.Steps {
		switch step.Status {
		case domain.StepStatusPassed:
			ev.StepsPassed++
		case domain.StepStatusFailed:
			ev.StepsFailed++
		case domain.StepStatusSkipped:
			ev.StepsSkipped++
		}
		if step.Screenshot != "" {
			ev.Screenshots = append(ev.Screenshots, step.Screenshot)
		}
	}

	resp.Evidence = ev
	return resp
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("warn: failed to encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
