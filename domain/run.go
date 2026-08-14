package domain

import "time"

// Run is the audit log written after execution.
// It captures the actual runtime values seen, not just the plan.
type Run struct {
	RunID        string         `json:"run_id"`
	CapabilityID string         `json:"capability_id"`
	Version      int            `json:"version"`
	Params       map[string]any `json:"params"`    // secret values redacted
	Status       RunStatus      `json:"status"`
	Steps        []StepResult   `json:"steps"`
	StartedAt    time.Time      `json:"started_at"`
	FinishedAt   time.Time      `json:"finished_at"`
	ErrorClass   *ErrorClass    `json:"error_class,omitempty"`
	Escalation   *Escalation    `json:"escalation,omitempty"`
}

// RunStatus represents the terminal state of a run.
type RunStatus string

const (
	RunStatusRunning       RunStatus = "running"
	RunStatusSuccess       RunStatus = "success"
	RunStatusBusinessError RunStatus = "business_error" // expected, clean stop — no retry
	RunStatusHardFailure   RunStatus = "hard_failure"   // unexpected — needs investigation
	RunStatusEscalated     RunStatus = "escalated"      // paused for human approval
	RunStatusPartial       RunStatus = "partial"        // stopped at a checkpoint
)

// StepResult records what actually happened for one step.
type StepResult struct {
	StepID      string        `json:"step_id"`
	Status      StepStatus    `json:"status"`
	ActualValue string        `json:"actual_value,omitempty"` // value seen or captured
	Screenshot  string        `json:"screenshot,omitempty"`   // path to screenshot file
	Err         string        `json:"err,omitempty"`
	DurationMs  int64         `json:"duration_ms"`
}

// StepStatus is the outcome of a single step execution.
type StepStatus string

const (
	StepStatusPassed  StepStatus = "passed"
	StepStatusFailed  StepStatus = "failed"
	StepStatusSkipped StepStatus = "skipped"
)

// ErrorClass is the structured error taxonomy attached to a failed Run.
type ErrorClass struct {
	Category    ErrorCategory `json:"category"`
	StepID      string        `json:"step_id"`
	Message     string        `json:"message"`
	IsRetryable bool          `json:"is_retryable"`
}

// ErrorCategory classifies the root cause of a failure.
type ErrorCategory string

const (
	ErrorCategoryLocator    ErrorCategory = "locator_not_found" // DOM changed
	ErrorCategoryTimeout    ErrorCategory = "timeout"
	ErrorCategoryNavigation ErrorCategory = "navigation_error"
	ErrorCategoryAssertion  ErrorCategory = "assertion_failed"  // unexpected state
	ErrorCategoryBusiness   ErrorCategory = "business_rule"     // expected business outcome
	ErrorCategoryPlanner    ErrorCategory = "planner_error"     // bad LLM plan
)

// Escalation captures context when the engine pauses for human intervention.
type Escalation struct {
	StepID     string `json:"step_id"`
	Message    string `json:"message"`
	Screenshot string `json:"screenshot,omitempty"`
	ResumeURL  string `json:"resume_url"`
}
