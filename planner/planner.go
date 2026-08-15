// Package planner converts a natural-language instruction and a live page
// accessibility tree into a validated, safe Capability artifact.
//
// It is the only package in the system that depends on an LLM. All other
// packages are deterministic.
package planner

import (
	"computer-use/domain"
	"context"
)

// PageContext is a pruned accessibility tree snapshot of the current browser
// page. It is passed to the LLM instead of raw HTML to prevent context
// explosion and hallucinated CSS selectors.
//
// Using a structured tree means the LLM only sees interactive elements and
// their stable identifiers (test-id, role, aria-label), not transient class
// names or inline styles.
type PageContext struct {
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Elements []Element `json:"elements"`
}

// Element is a single interactive node from the accessibility tree.
type Element struct {
	// Role is the ARIA role (e.g. "button", "textbox", "link").
	Role string `json:"role"`
	// Name is the accessible name (aria-label, placeholder, or visible label text).
	Name string `json:"name"`
	// TestID is the value of data-testid / data-test-id, if present.
	TestID string `json:"test_id,omitempty"`
	// Tag is the HTML tag (e.g. "input", "button", "select").
	Tag string `json:"tag"`
	// Type is the HTML input type (e.g. "text", "password", "submit").
	Type string `json:"type,omitempty"`
	// Value is the current field value (redacted for password fields).
	Value string `json:"value,omitempty"`
	// Disabled reports whether the element is disabled.
	Disabled bool `json:"disabled,omitempty"`
}

// PlanRequest is the input to the planner.
type PlanRequest struct {
	// Instruction is the natural-language goal (e.g. "Log in as standard_user").
	Instruction string
	// PageContext is the accessibility tree of the current page.
	PageContext PageContext
	// CapabilityID is used to name the resulting Capability.
	CapabilityID string
	// Params are hints about what runtime parameters the Capability should accept.
	// The LLM uses these to know which values to parameterise.
	Params []domain.ParamDef
	// SafetyPolicy is the operator-defined ceiling. It is injected verbatim into
	// the resulting Capability — the planner cannot lower it.
	SafetyPolicy domain.SafetyPolicy
}

// LLMClient is the interface the planner uses to call the language model.
// Abstracting it allows tests to inject static fixtures and avoids API keys
// in CI. The real implementation wraps an OpenAI / Gemini client.
type LLMClient interface {
	// Complete sends a prompt and returns the text completion.
	Complete(ctx context.Context, prompt string) (string, error)
}

// Planner converts natural-language instructions into Capability artifacts.
type Planner struct {
	llm    LLMClient
	safety *SafetyPipeline
}

// New creates a Planner with the given LLM client.
func New(llm LLMClient) *Planner {
	return &Planner{
		llm:    llm,
		safety: NewSafetyPipeline(llm),
	}
}

// Plan generates a Capability from the given request.
// It calls the LLM once to draft a plan, then runs it through the safety
// pipeline before returning it.
func (p *Planner) Plan(ctx context.Context, req PlanRequest) (*domain.Capability, error) {
	prompt := buildPrompt(req)

	raw, err := p.llm.Complete(ctx, prompt)
	if err != nil {
		return nil, &domain.PlannerError{Message: "llm call failed: " + err.Error()}
	}

	cap, err := parseCapability(raw, req)
	if err != nil {
		return nil, &domain.PlannerError{Message: "parse failed: " + err.Error()}
	}

	if err := p.safety.Evaluate(ctx, cap); err != nil {
		return nil, err
	}

	return cap, nil
}
