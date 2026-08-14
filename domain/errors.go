// Package domain — typed sentinel errors.
// These are the error types the replay engine returns.
// Using typed errors (not strings) allows callers to use errors.As() to
// branch on the specific failure mode cleanly.
package domain

import "fmt"

// BusinessError is returned when an assertion with IsBusinessOutcome=true fails.
// This signals a clean, expected stop — NOT a crash. The engine will NOT retry.
type BusinessError struct {
	StepID  string
	Message string
}

func (e *BusinessError) Error() string {
	return fmt.Sprintf("business error at step %s: %s", e.StepID, e.Message)
}

// LocatorError is returned when all strategies in a Locator's fallback chain fail.
type LocatorError struct {
	StepID     string
	Strategies []LocatorStrategy
	LastErr    error
}

func (e *LocatorError) Error() string {
	return fmt.Sprintf("locator not found at step %s after %d strategies: %v",
		e.StepID, len(e.Strategies), e.LastErr)
}

func (e *LocatorError) Unwrap() error { return e.LastErr }

// EscalationError is returned when a step triggers human_escalate policy.
// It carries the escalation context for the API layer to surface.
type EscalationError struct {
	StepID  string
	Message string
}

func (e *EscalationError) Error() string {
	return fmt.Sprintf("escalated at step %s: %s", e.StepID, e.Message)
}

// PlannerError is returned when the LLM emits a structurally invalid plan.
type PlannerError struct {
	Message string
}

func (e *PlannerError) Error() string {
	return fmt.Sprintf("planner error: %s", e.Message)
}
