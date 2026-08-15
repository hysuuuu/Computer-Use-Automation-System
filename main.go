package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"computer-use/api"
	"computer-use/domain"
	"computer-use/planner"
	"computer-use/replay"
	"computer-use/store"
	"computer-use/stub"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data", "./data", "Directory to store capabilities and runs")
	baseURL := flag.String("base-url", "http://localhost:8080", "Base URL for resume links")
	flag.Parse()

	// Initialise the store.
	s, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("failed to create store: %v", err)
	}

	// Seed a demo capability so the system has something to run immediately.
	if err := seedDemoCapability(s); err != nil {
		log.Printf("warn: failed to seed demo capability: %v", err)
	}

	// BrowserFactory creates a fresh stub browser per run.
	factory := func() replay.Browser {
		return stub.New("about:blank")
	}

	manager := api.NewRunManager(s, factory, *baseURL)

	// Stub LLM: returns a fixed SauceDemo login capability for any prompt.
	// Replace with a real OpenAI/Gemini client for production use.
	stubLLM := &stubLLMClient{}
	p := planner.New(stubLLM)

	server := api.NewServer(manager, s, p)

	log.Printf("computer-use server listening on %s", *addr)
	log.Printf("data directory: %s", *dataDir)
	log.Println()
	log.Println("Try it out:")
	log.Printf("  curl -X POST %s/runs -H 'Content-Type: application/json' \\", *baseURL)
	log.Println(`       -d '{"capability_id":"cap_saucedemo_login_v1","params":{"username":"standard_user","password":"secret_sauce"}}'`)

	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// seedDemoCapability writes a complete SauceDemo login capability to the store.
// This lets the system be demo'd immediately with curl without a planner step.
func seedDemoCapability(s *store.Store) error {
	cap := &domain.Capability{
		ID:          "cap_saucedemo_login_v1",
		Version:     1,
		Description: "Log in to SauceDemo and verify successful landing on inventory page",
		Target: domain.Target{
			URL:         "https://www.saucedemo.com",
			Application: "saucedemo",
			Description: "Sauce Labs demo e-commerce site",
		},
		Params: []domain.ParamDef{
			{Name: "username", Type: "string", Required: true, Description: "Login username"},
			{Name: "password", Type: "secret", Required: true, Description: "Login password"},
		},
		SafetyPolicy: domain.SafetyPolicy{
			MaxRiskAllowed:    domain.RiskMedium,
			RequireApprovalAt: domain.RiskHigh,
			AllowedDomains:    []string{"saucedemo.com"},
		},
		CreatedAt: time.Now(),
		CreatedBy: "seed",
		Steps: []domain.Step{
			{
				ID:          "step_001",
				Type:        domain.StepTypeAction,
				Description: "Navigate to login page",
				Risk:        domain.RiskLow,
				Action:      &domain.Action{Kind: domain.ActionKindNavigate, Value: "https://www.saucedemo.com"},
				OnError:     domain.ErrorPolicy{Strategy: "fail_fast"},
				TimeoutMs:   5000,
			},
			{
				ID:          "step_002",
				Type:        domain.StepTypeAction,
				Description: "Fill username field",
				Risk:        domain.RiskLow,
				Locator: &domain.Locator{
					Primary:   domain.LocatorStrategy{Kind: domain.LocatorKindTestID, Value: "username"},
					Fallbacks: []domain.LocatorStrategy{{Kind: domain.LocatorKindCSS, Value: "#user-name"}},
				},
				Action:    &domain.Action{Kind: domain.ActionKindFill, Value: "{{username}}"},
				OnError:   domain.ErrorPolicy{Strategy: "fail_fast"},
				TimeoutMs: 3000,
			},
			{
				ID:          "step_003",
				Type:        domain.StepTypeAction,
				Description: "Fill password field",
				Risk:        domain.RiskLow,
				Locator: &domain.Locator{
					Primary:   domain.LocatorStrategy{Kind: domain.LocatorKindTestID, Value: "password"},
					Fallbacks: []domain.LocatorStrategy{{Kind: domain.LocatorKindCSS, Value: "#password"}},
				},
				Action:    &domain.Action{Kind: domain.ActionKindFill, Value: "{{password}}"},
				OnError:   domain.ErrorPolicy{Strategy: "fail_fast"},
				TimeoutMs: 3000,
			},
			{
				ID:          "step_004",
				Type:        domain.StepTypeAction,
				Description: "Click login button",
				Risk:        domain.RiskMedium,
				Locator: &domain.Locator{
					Primary:   domain.LocatorStrategy{Kind: domain.LocatorKindTestID, Value: "login-button"},
					Fallbacks: []domain.LocatorStrategy{{Kind: domain.LocatorKindRole, Value: "button", Name: "Login"}},
				},
				Action:    &domain.Action{Kind: domain.ActionKindClick},
				OnError:   domain.ErrorPolicy{Strategy: "fail_fast"},
				TimeoutMs: 3000,
			},
			{
				// Branch: did login fail with an error message?
				ID:          "step_005",
				Type:        domain.StepTypeBranch,
				Description: "Check whether login failed",
				Risk:        domain.RiskLow,
				Branch: &domain.Branch{
					Condition: domain.Assertion{
						Kind:              domain.AssertionKindTextVisible,
						Expected:          "Epic sadface",
						IsBusinessOutcome: false, // routing only
					},
					IfTrue:  []string{"step_005_err"},
					IfFalse: []string{"step_006"},
				},
				OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
			},
			{
				// Error path: login was rejected — mark as business error and stop.
				ID:          "step_005_err",
				Type:        domain.StepTypeAssert,
				Description: "Record login rejection as a business error",
				Risk:        domain.RiskLow,
				Assert: &domain.Assertion{
					Kind:              domain.AssertionKindTextVisible,
					Expected:          "Epic sadface",
					IsBusinessOutcome: true, // triggers RunStatus: business_error
				},
				OnError: domain.ErrorPolicy{Strategy: "fail_fast"},
			},
			{
				ID:          "step_006",
				Type:        domain.StepTypeAssert,
				Description: "Verify we landed on the inventory page",
				Risk:        domain.RiskLow,
				Assert: &domain.Assertion{
					Kind:              domain.AssertionKindURLContains,
					Expected:          "/inventory.html",
					IsBusinessOutcome: false,
				},
				OnError:   domain.ErrorPolicy{Strategy: "retry", MaxRetries: 2, RetryDelayMs: 1000},
				TimeoutMs: 5000,
			},
			{
				ID:          "step_007",
				Type:        domain.StepTypeCheckpoint,
				Description: "Login successful — safe savepoint for partial replay",
				Risk:        domain.RiskLow,
				OnError:     domain.ErrorPolicy{Strategy: "fail_fast"},
			},
		},
	}

	data, _ := json.MarshalIndent(cap, "", "  ")
	log.Printf("seeding capability:\n%s", data)
	return s.SaveCapability(cap)
}

// stubLLMClient is a development-only LLM that returns a pre-canned SauceDemo
// login capability for any prompt. Replace with openai.Client or similar.
type stubLLMClient struct{}

func (s *stubLLMClient) Complete(_ context.Context, _ string) (string, error) {
	// Return a minimal valid capability JSON for any prompt.
	return `{
  "id": "cap_planned",
  "version": 1,
  "description": "Planned capability (stub LLM)",
  "target": {"url": "https://www.saucedemo.com", "application": "saucedemo"},
  "steps": [
    {
      "id": "step_001",
      "type": "action",
      "description": "Navigate to target",
      "action": {"kind": "navigate", "value": "https://www.saucedemo.com"},
      "risk": "low",
      "on_error": {"strategy": "fail_fast"}
    }
  ]
}`, nil
}
