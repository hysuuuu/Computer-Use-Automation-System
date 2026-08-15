package planner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"computer-use/domain"
)

// buildPrompt constructs the planning prompt sent to the LLM.
// It includes:
//  1. System framing (role, output contract)
//  2. The accessibility tree (structured, not raw HTML)
//  3. The natural-language instruction
//  4. The parameter hints so the LLM knows what to parameterise
//
// The prompt is designed to elicit JSON-only output, which parseCapability
// then extracts and validates.
func buildPrompt(req PlanRequest) string {
	var sb strings.Builder

	sb.WriteString("You are a browser automation planner. Your ONLY output is a valid JSON object.\n")
	sb.WriteString("Do NOT wrap the JSON in markdown code blocks or add any explanation.\n\n")

	sb.WriteString("=== TASK ===\n")
	sb.WriteString(req.Instruction + "\n\n")

	sb.WriteString("=== TARGET PAGE ===\n")
	sb.WriteString("URL: " + req.PageContext.URL + "\n")
	sb.WriteString("Title: " + req.PageContext.Title + "\n\n")

	sb.WriteString("=== INTERACTIVE ELEMENTS (accessibility tree) ===\n")
	for _, el := range req.PageContext.Elements {
		sb.WriteString(fmt.Sprintf("  role=%s name=%q", el.Role, el.Name))
		if el.TestID != "" {
			sb.WriteString(fmt.Sprintf(" test-id=%q", el.TestID))
		}
		if el.Type != "" {
			sb.WriteString(fmt.Sprintf(" type=%s", el.Type))
		}
		if el.Tag != "" {
			sb.WriteString(fmt.Sprintf(" tag=<%s>", el.Tag))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	if len(req.Params) > 0 {
		sb.WriteString("=== RUNTIME PARAMETERS ===\n")
		sb.WriteString("Use {{param_name}} interpolation for these values:\n")
		for _, p := range req.Params {
			sb.WriteString(fmt.Sprintf("  {{%s}} — %s (type: %s)\n", p.Name, p.Description, p.Type))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("=== OUTPUT CONTRACT ===\n")
	sb.WriteString("Return a single JSON object matching the Capability schema:\n")
	sb.WriteString(`{
  "id": "` + req.CapabilityID + `",
  "version": 1,
  "description": "<one sentence>",
  "target": {"url": "...", "application": "...", "description": "..."},
  "params": [{"name": "...", "type": "string|secret|number", "description": "...", "required": true}],
  "steps": [
    {
      "id": "step_NNN",
      "type": "action|assert|branch|checkpoint",
      "description": "<what this step does>",
      "locator": {
        "primary": {"kind": "test-id|role|label|text|css", "value": "..."},
        "fallbacks": [{"kind": "css", "value": "..."}]
      },
      "action": {"kind": "navigate|fill|click|select|check|key_press", "value": "..."},
      "risk": "low|medium|high|critical",
      "on_error": {"strategy": "fail_fast|retry|skip|human_escalate"}
    }
  ]
}

LOCATOR PRIORITY RULES (CRITICAL):
1. Always prefer test-id if data-testid is visible in the accessibility tree.
2. Then prefer role (with accessible name).
3. Only use css/xpath as a last-resort fallback.
4. Include at least one CSS fallback in the fallbacks array.

STEP RULES:
- Start with a navigate step to the target URL.
- After a login/submit, add an assert step to detect success or failure.
- Use branch steps to handle multiple possible page states without re-planning.
- Mark steps that spend money, delete data, or send communications as risk=high or critical.
`)

	return sb.String()
}

// parseCapability extracts and validates a Capability JSON from the raw LLM
// completion. The LLM is instructed to return JSON directly, but may wrap it
// in markdown backticks or prefix with explanation text. This function strips
// those artifacts before parsing.
func parseCapability(raw string, req PlanRequest) (*domain.Capability, error) {
	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON object found in LLM response")
	}

	var cap domain.Capability
	if err := json.Unmarshal([]byte(jsonStr), &cap); err != nil {
		return nil, fmt.Errorf("JSON unmarshal: %w", err)
	}

	// Apply operator-controlled fields that the LLM cannot override.
	cap.SafetyPolicy = req.SafetyPolicy
	cap.CreatedAt = time.Now()
	cap.CreatedBy = "llm:planner"

	// Force the capability ID from the request — the LLM should match it,
	// but we guarantee it here regardless.
	if req.CapabilityID != "" {
		cap.ID = req.CapabilityID
	}

	if err := validateCapability(&cap); err != nil {
		return nil, err
	}

	return &cap, nil
}

// extractJSON tries to isolate a JSON object from a string that may contain
// surrounding prose or markdown fences. It returns empty string if not found.
func extractJSON(s string) string {
	// Strip markdown code fences if present.
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```", "")
	s = strings.TrimSpace(s)

	// Find the outermost { ... } pair.
	start := strings.Index(s, "{")
	if start == -1 {
		return ""
	}

	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// validateCapability enforces structural constraints that must hold regardless
// of what the LLM produced.
func validateCapability(cap *domain.Capability) error {
	if cap.ID == "" {
		return fmt.Errorf("capability must have an id")
	}
	if len(cap.Steps) == 0 {
		return fmt.Errorf("capability must have at least one step")
	}

	seenIDs := make(map[string]bool)
	for i, step := range cap.Steps {
		if step.ID == "" {
			return fmt.Errorf("step[%d] has no id", i)
		}
		if seenIDs[step.ID] {
			return fmt.Errorf("duplicate step id %q", step.ID)
		}
		seenIDs[step.ID] = true

		if step.Type == "" {
			return fmt.Errorf("step %q has no type", step.ID)
		}

		// Ensure every action step (except navigate) has a locator.
		if step.Type == domain.StepTypeAction &&
			step.Action != nil &&
			step.Action.Kind != domain.ActionKindNavigate &&
			step.Locator == nil {
			return fmt.Errorf("step %q is an action (%s) but has no locator", step.ID, step.Action.Kind)
		}
	}
	return nil
}
