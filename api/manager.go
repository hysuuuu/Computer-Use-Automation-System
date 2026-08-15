// Package api implements the HTTP server for the computer-use automation system.
package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"computer-use/domain"
	"computer-use/replay"
	"computer-use/store"
)

// BrowserFactory creates a fresh Browser for each run.
// This allows the server to inject real Playwright or a stub browser.
type BrowserFactory func() replay.Browser

// RunState holds the live state of an in-progress or completed run.
type RunState struct {
	mu         sync.RWMutex
	Run        *domain.Run
	Escalation *domain.Escalation // non-nil when status == escalated
	resumeCh   chan ResumeRequest  // used to unblock an escalated run
}

// ResumeRequest carries the human operator's decision.
type ResumeRequest struct {
	Resolution     string // "approved" | "completed_manually" | "skip" | "abort"
	ResumeFromStep string
	Notes          string
}

// RunManager owns the lifecycle of all runs.
// It launches goroutines, stores state, and coordinates resumption.
type RunManager struct {
	mu      sync.RWMutex
	states  map[string]*RunState
	store   *store.Store
	factory BrowserFactory
	baseURL string // used to construct resume_url in escalation responses
}

// NewRunManager creates a RunManager.
func NewRunManager(s *store.Store, factory BrowserFactory, baseURL string) *RunManager {
	return &RunManager{
		states:  make(map[string]*RunState),
		store:   s,
		factory: factory,
		baseURL: baseURL,
	}
}

// StartRunRequest is the parsed body of POST /runs.
type StartRunRequest struct {
	Instruction string            `json:"instruction"`
	Capability  *domain.Capability `json:"capability,omitempty"` // provide inline OR via capability_id
	CapabilityID string            `json:"capability_id,omitempty"`
	Params      map[string]string `json:"params"`
	Options     RunOptions        `json:"options"`
}

// RunOptions are per-run configuration overrides.
type RunOptions struct {
	TimeoutSeconds int  `json:"timeout_seconds"`
	DryRun         bool `json:"dry_run"`
	SaveCapability bool `json:"save_capability"`
}

// StartRun launches an async run and returns its ID immediately.
func (m *RunManager) StartRun(req StartRunRequest) (string, error) {
	// Resolve capability: inline or from store.
	cap := req.Capability
	if cap == nil && req.CapabilityID != "" {
		var err error
		cap, err = m.store.GetCapability(req.CapabilityID)
		if err != nil {
			return "", fmt.Errorf("capability not found: %w", err)
		}
	}
	if cap == nil {
		return "", fmt.Errorf("must provide either 'capability' or 'capability_id'")
	}

	if req.Options.SaveCapability {
		if err := m.store.SaveCapability(cap); err != nil {
			log.Printf("warn: failed to save capability: %v", err)
		}
	}

	runID := newID("run")
	timeoutSecs := req.Options.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = 120
	}

	state := &RunState{
		resumeCh: make(chan ResumeRequest, 1),
		Run: &domain.Run{
			RunID:        runID,
			CapabilityID: cap.ID,
			Version:      cap.Version,
			Status:       domain.RunStatusRunning,
			StartedAt:    time.Now(),
		},
	}

	m.mu.Lock()
	m.states[runID] = state
	m.mu.Unlock()

	go m.executeRun(state, cap, req.Params, req.Options, timeoutSecs)

	return runID, nil
}

// executeRun runs the engine in a goroutine and updates state on completion.
func (m *RunManager) executeRun(
	state *RunState,
	cap *domain.Capability,
	params map[string]string,
	opts RunOptions,
	timeoutSecs int,
) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	browser := m.factory()
	engine := replay.NewEngine(browser, cap, replay.EngineOptions{
		DryRun:         opts.DryRun,
		DefaultTimeout: 30 * time.Second,
		RunID:          state.Run.RunID,
	})

	run, err := engine.Run(ctx, params)

	// If escalated, wait for human resolution before finalizing.
	var ee *domain.EscalationError
	if errors.As(err, &ee) {
		escalation := &domain.Escalation{
			StepID:    ee.StepID,
			Message:   ee.Message,
			ResumeURL: m.baseURL + "/runs/" + state.Run.RunID + "/resume",
		}

		state.mu.Lock()
		state.Run = run
		state.Escalation = escalation
		state.mu.Unlock()

		// Persist the escalated state.
		_ = m.store.SaveRun(run)

		// Wait for human to POST /resume or for context to expire.
		select {
		case resumeReq := <-state.resumeCh:
			m.handleResume(state, run, resumeReq)
		case <-ctx.Done():
			state.mu.Lock()
			state.Run.Status = domain.RunStatusPartial
			state.Run.FinishedAt = time.Now()
			state.mu.Unlock()
		}
		return
	}

	// Non-escalation terminal state.
	state.mu.Lock()
	if run != nil {
		state.Run = run
	}
	state.mu.Unlock()

	if run != nil {
		_ = m.store.SaveRun(run)
	}
}

// handleResume processes a human operator's resume decision.
func (m *RunManager) handleResume(state *RunState, run *domain.Run, req ResumeRequest) {
	state.mu.Lock()
	defer state.mu.Unlock()

	run.FinishedAt = time.Now()

	switch req.Resolution {
	case "abort":
		run.Status = domain.RunStatusHardFailure
	case "approved", "completed_manually", "skip":
		// Human completed or approved the blocked step — treat as success.
		run.Status = domain.RunStatusSuccess
	default:
		run.Status = domain.RunStatusPartial
	}

	state.Run = run
	state.Escalation = nil
	_ = m.store.SaveRun(run)
}

// GetState returns the current RunState for a run ID.
func (m *RunManager) GetState(runID string) (*RunState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.states[runID]
	return state, ok
}

// Resume sends a ResumeRequest to an escalated run.
// Returns an error if the run is not in escalated state.
func (m *RunManager) Resume(runID string, req ResumeRequest) error {
	state, ok := m.GetState(runID)
	if !ok {
		return fmt.Errorf("run %q: %w", runID, store.ErrNotFound)
	}

	state.mu.RLock()
	status := state.Run.Status
	state.mu.RUnlock()

	if status != domain.RunStatusEscalated {
		return fmt.Errorf("run %q is not in escalated state (current: %s)", runID, status)
	}

	select {
	case state.resumeCh <- req:
		return nil
	default:
		return fmt.Errorf("run %q resume channel full", runID)
	}
}

// newID generates a simple unique ID with a prefix.
func newID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
