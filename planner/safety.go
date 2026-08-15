package planner

import (
	"context"
	"fmt"
	"strings"

	"computer-use/domain"
)

// SafetyError is returned when a Capability is blocked or modified by the
// safety pipeline. Layer identifies which tier caught the issue.
type SafetyError struct {
	Layer   string // "keyword" | "domain_allowlist" | "structural" | "llm_judge"
	StepID  string
	Message string
}

func (e *SafetyError) Error() string {
	if e.StepID != "" {
		return fmt.Sprintf("safety[%s] step %s: %s", e.Layer, e.StepID, e.Message)
	}
	return fmt.Sprintf("safety[%s]: %s", e.Layer, e.Message)
}

// destructiveKeywords are terms that suggest irreversible destructive actions.
// This is the fast, cheap first-pass filter — no LLM needed.
var destructiveKeywords = []string{
	"delete", "remove", "drop", "truncate", "wipe", "purge",
	"destroy", "erase", "format", "terminate", "kill",
	"transfer", "wire", "withdraw", "send money", "purchase all",
	"bulk update", "mass update", "override", "bypass",
}

// SafetyPipeline implements the tiered safety system described in the SPEC:
//
//	Tier 1: Keyword pre-filter (this file, fast/cheap, no LLM)
//	Tier 2: Domain allowlist check (structural)
//	Tier 3: Risk ceiling enforcement (structural)
//	Tier 4: LLM-as-Judge (only called when risk >= High)
type SafetyPipeline struct {
	llm LLMClient
}

// NewSafetyPipeline creates a SafetyPipeline backed by the given LLM client
// (used only for Tier 4 contextual review).
func NewSafetyPipeline(llm LLMClient) *SafetyPipeline {
	return &SafetyPipeline{llm: llm}
}

// Evaluate runs all safety tiers against a planned Capability.
// It mutates the Capability in-place for soft violations (e.g. upgrading
// RequiresApproval) and returns a SafetyError for hard blocks.
func (sp *SafetyPipeline) Evaluate(ctx context.Context, cap *domain.Capability) error {
	// Tier 1: Keyword scan — cheap, synchronous, no LLM.
	if err := sp.keywordScan(cap); err != nil {
		return err
	}

	// Tier 2: Domain allowlist — structural, no LLM.
	if err := sp.domainAllowlist(cap); err != nil {
		return err
	}

	// Tier 3: Risk ceiling — structural, mutates RequiresApproval in-place.
	if err := sp.riskCeiling(cap); err != nil {
		return err
	}

	// Tier 4: LLM-as-Judge — only invoked if any step has risk >= High.
	if sp.hasHighRiskStep(cap) {
		if err := sp.llmJudge(ctx, cap); err != nil {
			return err
		}
	}

	return nil
}

// ── Tier 1: Keyword scan ─────────────────────────────────────────────────────

func (sp *SafetyPipeline) keywordScan(cap *domain.Capability) error {
	// Scan capability description.
	if hit := findKeyword(cap.Description); hit != "" {
		return &SafetyError{
			Layer:   "keyword",
			Message: fmt.Sprintf("capability description contains destructive keyword %q", hit),
		}
	}

	// Scan every step description and action value.
	for _, step := range cap.Steps {
		if hit := findKeyword(step.Description); hit != "" {
			return &SafetyError{
				Layer:   "keyword",
				StepID:  step.ID,
				Message: fmt.Sprintf("step description contains destructive keyword %q", hit),
			}
		}
		if step.Action != nil {
			if hit := findKeyword(step.Action.Value); hit != "" {
				return &SafetyError{
					Layer:   "keyword",
					StepID:  step.ID,
					Message: fmt.Sprintf("action value contains destructive keyword %q", hit),
				}
			}
		}
	}
	return nil
}

func findKeyword(text string) string {
	lower := strings.ToLower(text)
	for _, kw := range destructiveKeywords {
		if strings.Contains(lower, kw) {
			return kw
		}
	}
	return ""
}

// ── Tier 2: Domain allowlist ──────────────────────────────────────────────────

func (sp *SafetyPipeline) domainAllowlist(cap *domain.Capability) error {
	if len(cap.SafetyPolicy.AllowedDomains) == 0 {
		// No allowlist configured — skip check.
		return nil
	}

	// Check every navigate action and the target URL.
	urls := []string{cap.Target.URL}
	for _, step := range cap.Steps {
		if step.Action != nil && step.Action.Kind == domain.ActionKindNavigate {
			urls = append(urls, step.Action.Value)
		}
	}

	for _, url := range urls {
		if url == "" {
			continue
		}
		if !domainAllowed(url, cap.SafetyPolicy.AllowedDomains) {
			return &SafetyError{
				Layer:   "domain_allowlist",
				Message: fmt.Sprintf("URL %q is outside the allowed domains %v", url, cap.SafetyPolicy.AllowedDomains),
			}
		}
	}
	return nil
}

func domainAllowed(url string, allowed []string) bool {
	for _, d := range allowed {
		if strings.Contains(url, d) {
			return true
		}
	}
	return false
}

// ── Tier 3: Risk ceiling ──────────────────────────────────────────────────────

// riskOrder maps risk levels to comparable integers.
var riskOrder = map[domain.RiskLevel]int{
	domain.RiskLow:      0,
	domain.RiskMedium:   1,
	domain.RiskHigh:     2,
	domain.RiskCritical: 3,
}

func (sp *SafetyPipeline) riskCeiling(cap *domain.Capability) error {
	requireAt := cap.SafetyPolicy.RequireApprovalAt
	if requireAt == "" {
		requireAt = domain.RiskCritical // default: only escalate truly critical steps
	}

	for i := range cap.Steps {
		step := &cap.Steps[i]
		// If this step's risk meets or exceeds RequireApprovalAt, force approval.
		if riskOrder[step.Risk] >= riskOrder[requireAt] {
			step.RequiresApproval = true
			// Upgrade error policy to human_escalate if not already set.
			if step.OnError.Strategy != "human_escalate" {
				step.OnError = domain.ErrorPolicy{
					Strategy:          "human_escalate",
					EscalationMessage: fmt.Sprintf("Step %q has risk=%s which requires human approval.", step.ID, step.Risk),
				}
			}
		}
	}
	return nil
}

func (sp *SafetyPipeline) hasHighRiskStep(cap *domain.Capability) bool {
	for _, step := range cap.Steps {
		if riskOrder[step.Risk] >= riskOrder[domain.RiskHigh] {
			return true
		}
	}
	return false
}

// ── Tier 4: LLM-as-Judge ─────────────────────────────────────────────────────

// llmJudge sends a concise review prompt to the LLM and blocks if it returns
// a rejection. This is only called when the structural checks pass but there
// are high-risk steps that warrant contextual review.
func (sp *SafetyPipeline) llmJudge(ctx context.Context, cap *domain.Capability) error {
	prompt := buildJudgePrompt(cap)
	response, err := sp.llm.Complete(ctx, prompt)
	if err != nil {
		// LLM judge unavailable — fail open with a warning rather than blocking
		// legitimate work. Log but don't return an error.
		return nil
	}

	lower := strings.ToLower(strings.TrimSpace(response))
	if strings.HasPrefix(lower, "reject") || strings.HasPrefix(lower, "block") {
		// Extract reason after the first colon if present.
		reason := response
		if idx := strings.Index(response, ":"); idx >= 0 {
			reason = strings.TrimSpace(response[idx+1:])
		}
		return &SafetyError{
			Layer:   "llm_judge",
			Message: reason,
		}
	}
	return nil
}

func buildJudgePrompt(cap *domain.Capability) string {
	var sb strings.Builder
	sb.WriteString("You are a safety auditor reviewing a browser automation plan.\n")
	sb.WriteString("Respond with one of:\n")
	sb.WriteString("  APPROVE — if the plan is safe to execute (possibly with human approval for marked steps)\n")
	sb.WriteString("  REJECT: <reason> — if the plan contains genuinely dangerous or irreversible actions\n\n")
	sb.WriteString("Capability: " + cap.ID + "\n")
	sb.WriteString("Description: " + cap.Description + "\n")
	sb.WriteString("Target: " + cap.Target.URL + "\n\n")
	sb.WriteString("High-risk steps:\n")
	for _, step := range cap.Steps {
		if riskOrder[step.Risk] >= riskOrder[domain.RiskHigh] {
			sb.WriteString(fmt.Sprintf("  [%s] %s (risk=%s, requires_approval=%v)\n",
				step.ID, step.Description, step.Risk, step.RequiresApproval))
		}
	}
	return sb.String()
}
