package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"computer-use/domain"
	"computer-use/planner"
)

// ── Mock LLM ─────────────────────────────────────────────────────────────────

// mockLLM returns a fixed response string. Set Err to simulate failures.
type mockLLM struct {
	Response string
	Err      error
	// Prompts captures every prompt sent, for assertion.
	Prompts []string
}

func (m *mockLLM) Complete(_ context.Context, prompt string) (string, error) {
	m.Prompts = append(m.Prompts, prompt)
	return m.Response, m.Err
}

// ── Fixtures ──────────────────────────────────────────────────────────────────

// sauceDemoPageCtx is a static accessibility tree fixture for saucedemo.com login page.
var sauceDemoPageCtx = planner.PageContext{
	URL:   "https://www.saucedemo.com",
	Title: "Swag Labs",
	Elements: []planner.Element{
		{Role: "textbox", Name: "Username", TestID: "username", Tag: "input", Type: "text"},
		{Role: "textbox", Name: "Password", TestID: "password", Tag: "input", Type: "password"},
		{Role: "button", Name: "Login", TestID: "login-button", Tag: "input", Type: "submit"},
	},
}

// validCapabilityJSON is what a well-behaved LLM would produce for the login task.
func validLoginCapabilityJSON(capID string) string {
	cap := domain.Capability{
		ID:          capID,
		Version:     1,
		Description: "Log into SauceDemo with provided credentials",
		Target: domain.Target{
			URL:         "https://www.saucedemo.com",
			Application: "saucedemo",
		},
		Params: []domain.ParamDef{
			{Name: "username", Type: "string", Required: true},
			{Name: "password", Type: "secret", Required: true},
		},
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed:    domain.RiskMedium,
			RequireApprovalAt: domain.RiskHigh,
			AllowedDomains:    []string{"saucedemo.com"},
		},
		CreatedAt: time.Now(),
		CreatedBy: "llm:mock",
		Steps: []domain.Step{
			{
				ID:   "step_001",
				Type: domain.StepTypeAction,
				Description: "Navigate to login page",
				Risk: domain.RiskLow,
				Action:  &domain.Action{Kind: domain.ActionKindNavigate, Value: "https://www.saucedemo.com"},
				OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
			},
			{
				ID:          "step_002",
				Type:        domain.StepTypeAction,
				Description: "Fill username",
				Risk:        domain.RiskLow,
				Locator: &domain.Locator{
					Primary:   domain.LocatorStrategy{Kind: domain.LocatorKindTestID, Value: "username"},
					Fallbacks: []domain.LocatorStrategy{{Kind: domain.LocatorKindCSS, Value: "#user-name"}},
				},
				Action:  &domain.Action{Kind: domain.ActionKindFill, Value: "{{username}}"},
				OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
			},
			{
				ID:          "step_003",
				Type:        domain.StepTypeAction,
				Description: "Fill password",
				Risk:        domain.RiskLow,
				Locator: &domain.Locator{
					Primary:   domain.LocatorStrategy{Kind: domain.LocatorKindTestID, Value: "password"},
					Fallbacks: []domain.LocatorStrategy{{Kind: domain.LocatorKindCSS, Value: "#password"}},
				},
				Action:  &domain.Action{Kind: domain.ActionKindFill, Value: "{{password}}"},
				OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
			},
			{
				ID:          "step_004",
				Type:        domain.StepTypeAction,
				Description: "Click login button",
				Risk:        domain.RiskMedium,
				Locator: &domain.Locator{
					Primary: domain.LocatorStrategy{Kind: domain.LocatorKindTestID, Value: "login-button"},
				},
				Action:  &domain.Action{Kind: domain.ActionKindClick},
				OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
			},
		},
	}
	b, _ := json.Marshal(cap)
	return string(b)
}

// ── Plan() tests ──────────────────────────────────────────────────────────────

// TestPlan_Success verifies that a well-formed LLM response produces a Capability.
func TestPlan_Success(t *testing.T) {
	capID := "cap_test_login"
	llm := &mockLLM{Response: validLoginCapabilityJSON(capID)}

	p := planner.New(llm)
	cap, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Log in as standard_user",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: capID,
		Params: []domain.ParamDef{
			{Name: "username", Type: "string", Required: true},
			{Name: "password", Type: "secret", Required: true},
		},
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed:    domain.RiskMedium,
			RequireApprovalAt: domain.RiskHigh,
			AllowedDomains:    []string{"saucedemo.com"},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cap == nil {
		t.Fatal("expected a capability, got nil")
	}
	if cap.ID != capID {
		t.Errorf("capability ID: want %q, got %q", capID, cap.ID)
	}
	if len(cap.Steps) == 0 {
		t.Error("capability has no steps")
	}
}

// TestPlan_PromptContainsInstruction verifies the LLM receives the instruction
// and page context in the prompt.
func TestPlan_PromptContainsInstruction(t *testing.T) {
	capID := "cap_test_prompt"
	llm := &mockLLM{Response: validLoginCapabilityJSON(capID)}
	p := planner.New(llm)

	_ , _ = p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Log in as standard_user",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: capID,
	})

	if len(llm.Prompts) == 0 {
		t.Fatal("LLM was never called")
	}
	prompt := llm.Prompts[0]
	if !strings.Contains(prompt, "Log in as standard_user") {
		t.Error("prompt should contain the instruction")
	}
	if !strings.Contains(prompt, "saucedemo.com") {
		t.Error("prompt should contain the target URL from page context")
	}
	if !strings.Contains(prompt, "login-button") {
		t.Error("prompt should surface test-ids from the accessibility tree")
	}
}

// TestPlan_PrioritisesTestIDLocators verifies the planner rewards test-id
// locators from the accessibility tree over fragile CSS selectors.
func TestPlan_PrioritisesTestIDLocators(t *testing.T) {
	capID := "cap_test_locators"
	llm := &mockLLM{Response: validLoginCapabilityJSON(capID)}
	p := planner.New(llm)

	cap, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Log in",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: capID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, step := range cap.Steps {
		if step.Locator == nil {
			continue
		}
		if step.Locator.Primary.Kind == domain.LocatorKindCSS ||
			step.Locator.Primary.Kind == domain.LocatorKindXPath {
			t.Errorf("step %s: primary locator should not be CSS/XPath (got %s); use test-id or role first",
				step.ID, step.Locator.Primary.Kind)
		}
	}
}

// TestPlan_LLMFailure verifies that an LLM error surfaces as a PlannerError.
func TestPlan_LLMFailure(t *testing.T) {
	llm := &mockLLM{Err: errors.New("rate limited")}
	p := planner.New(llm)

	_, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Log in",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: "cap_fail",
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *domain.PlannerError
	if !errors.As(err, &pe) {
		t.Errorf("expected *domain.PlannerError, got %T: %v", err, err)
	}
}

// TestPlan_MalformedJSON verifies that an LLM returning garbage is caught.
func TestPlan_MalformedJSON(t *testing.T) {
	llm := &mockLLM{Response: "Sure! Here is your plan: { this is not json }"}
	p := planner.New(llm)

	_, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Log in",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: "cap_garbage",
	})

	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	var pe *domain.PlannerError
	if !errors.As(err, &pe) {
		t.Errorf("expected *domain.PlannerError, got %T: %v", err, err)
	}
}

// ── Safety pipeline tests ─────────────────────────────────────────────────────

// TestSafety_KeywordBlock verifies that capabilities with destructive keywords
// are blocked before any structural analysis.
func TestSafety_KeywordBlock(t *testing.T) {
	deleteCapJSON := func() string {
		cap := domain.Capability{
			ID:      "cap_delete_account",
			Version: 1,
			Steps: []domain.Step{
				{
					ID:          "step_001",
					Type:        domain.StepTypeAction,
					Description: "Delete all user accounts from the system",
					Risk:        domain.RiskHigh,
					Action:      &domain.Action{Kind: domain.ActionKindClick},
					Locator:     &domain.Locator{Primary: domain.LocatorStrategy{Kind: domain.LocatorKindCSS, Value: "#delete-all"}},
					OnError:     domain.ErrorPolicy{Strategy: "fail_fast"},
				},
			},
		}
		b, _ := json.Marshal(cap)
		return string(b)
	}

	llm := &mockLLM{Response: deleteCapJSON()}
	p := planner.New(llm)

	_, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Delete all user accounts",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: "cap_delete_account",
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed:    domain.RiskMedium,
			RequireApprovalAt: domain.RiskHigh,
		},
	})

	if err == nil {
		t.Fatal("expected safety block, got nil error")
	}
	var se *planner.SafetyError
	if !errors.As(err, &se) {
		t.Errorf("expected *planner.SafetyError, got %T: %v", err, err)
	}
	if se.Layer != "keyword" {
		t.Errorf("expected layer=keyword, got %q", se.Layer)
	}
}

// TestSafety_DomainAllowlist verifies that a capability targeting a domain
// outside the allowlist is blocked.
func TestSafety_DomainAllowlist(t *testing.T) {
	offDomainCap := func() string {
		cap := domain.Capability{
			ID:      "cap_evil",
			Version: 1,
			Target:  domain.Target{URL: "https://competitor.com"},
			Steps: []domain.Step{
				{
					ID:      "step_001",
					Type:    domain.StepTypeAction,
					Risk:    domain.RiskLow,
					Action:  &domain.Action{Kind: domain.ActionKindNavigate, Value: "https://competitor.com/steal"},
					OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
				},
			},
		}
		b, _ := json.Marshal(cap)
		return string(b)
	}

	llm := &mockLLM{Response: offDomainCap()}
	p := planner.New(llm)

	_, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Check prices on competitor.com",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: "cap_evil",
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed: domain.RiskMedium,
			AllowedDomains: []string{"saucedemo.com"},
		},
	})

	if err == nil {
		t.Fatal("expected domain allowlist block, got nil")
	}
	var se *planner.SafetyError
	if !errors.As(err, &se) {
		t.Errorf("expected *planner.SafetyError, got %T: %v", err, err)
	}
	if se.Layer != "domain_allowlist" {
		t.Errorf("expected layer=domain_allowlist, got %q", se.Layer)
	}
}

// TestSafety_RiskCeilingEscalation verifies that a step whose risk exceeds
// MaxRiskAllowed is upgraded to require human approval rather than blocked.
func TestSafety_RiskCeilingEscalation(t *testing.T) {
	highRiskCapJSON := func() string {
		cap := domain.Capability{
			ID:      "cap_high_risk",
			Version: 1,
			Target:  domain.Target{URL: "https://saucedemo.com"},
			Steps: []domain.Step{
				{
					ID:          "step_001",
					Type:        domain.StepTypeAction,
					Description: "Confirm large purchase",
					Risk:        domain.RiskCritical, // over the ceiling
					Action:      &domain.Action{Kind: domain.ActionKindClick},
					Locator:     &domain.Locator{Primary: domain.LocatorStrategy{Kind: domain.LocatorKindCSS, Value: "#confirm"}},
					OnError:     domain.ErrorPolicy{Strategy: "fail_fast"},
				},
			},
		}
		b, _ := json.Marshal(cap)
		return string(b)
	}

	llm := &mockLLM{Response: highRiskCapJSON()}
	p := planner.New(llm)

	cap, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Buy everything in the cart",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: "cap_high_risk",
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed:    domain.RiskMedium,
			RequireApprovalAt: domain.RiskHigh,
			AllowedDomains:    []string{"saucedemo.com"},
		},
	})

	// Should succeed — the pipeline upgrades to RequiresApproval rather than blocking.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, s := range cap.Steps {
		if s.ID == "step_001" {
			found = true
			if !s.RequiresApproval {
				t.Error("step_001 exceeds max risk: RequiresApproval should be true")
			}
		}
	}
	if !found {
		t.Error("step_001 not found in result")
	}
}

// TestSafety_CleanCapabilityPasses verifies a well-formed low-risk capability
// passes all safety checks without modification.
func TestSafety_CleanCapabilityPasses(t *testing.T) {
	capID := "cap_clean"
	llm := &mockLLM{Response: validLoginCapabilityJSON(capID)}
	p := planner.New(llm)

	cap, err := p.Plan(context.Background(), planner.PlanRequest{
		Instruction:  "Log in",
		PageContext:  sauceDemoPageCtx,
		CapabilityID: capID,
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed:    domain.RiskMedium,
			RequireApprovalAt: domain.RiskHigh,
			AllowedDomains:    []string{"saucedemo.com"},
		},
	})

	if err != nil {
		t.Fatalf("clean capability should pass safety: %v", err)
	}
	if cap == nil {
		t.Fatal("expected a capability, got nil")
	}
}
