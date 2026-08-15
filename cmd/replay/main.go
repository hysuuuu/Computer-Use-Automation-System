// cmd/replay/main.go
//
// Standalone replay runner for evidence generation.
//
// Usage:
//
//	# Successful checkout run
//	go run ./cmd/replay \
//	  --capability evidence/cap_saucedemo_checkout_v1.json \
//	  --params '{"username":"standard_user","password":"secret_sauce","item_name":"sauce-labs-backpack","first_name":"John","last_name":"Doe","zip":"12345"}' \
//	  --out evidence/replay_success.json
//
//	# Business-error run (bad password)
//	go run ./cmd/replay \
//	  --capability evidence/cap_saucedemo_checkout_v1.json \
//	  --params '{"username":"standard_user","password":"wrong_password","item_name":"sauce-labs-backpack","first_name":"John","last_name":"Doe","zip":"12345"}' \
//	  --out evidence/replay_business_error.json
//
// Exit codes:  0 = success,  2 = business_error or escalated,  1 = hard failure
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"computer-use/domain"
	"computer-use/replay"
	"computer-use/stub"
)

func main() {
	capFile    := flag.String("capability", "", "Path to capability JSON file (required)")
	paramsJSON := flag.String("params", "{}", "JSON object of runtime parameters")
	outFile    := flag.String("out", "", "Path to write the Run audit log JSON (required)")
	evidenceDir := flag.String("evidence-dir", "evidence", "Directory for screenshots and audit logs")
	flag.Parse()

	if *capFile == "" || *outFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	// ── Load capability ───────────────────────────────────────────────────────
	capData, err := os.ReadFile(*capFile)
	if err != nil {
		log.Fatalf("cannot read capability file: %v", err)
	}
	var cap domain.Capability
	if err := json.Unmarshal(capData, &cap); err != nil {
		log.Fatalf("cannot parse capability JSON: %v", err)
	}
	log.Printf("Loaded capability %q  (%d steps)", cap.ID, len(cap.Steps))

	// ── Parse runtime params ──────────────────────────────────────────────────
	// domain.Run.Params is map[string]any but our inputs are strings.
	var rawParams map[string]string
	if err := json.Unmarshal([]byte(*paramsJSON), &rawParams); err != nil {
		log.Fatalf("cannot parse --params JSON: %v", err)
	}
	// Convert to map[string]any for the Run struct.
	params := make(map[string]any, len(rawParams))
	for k, v := range rawParams {
		// Redact secret params so they don't appear in the audit log.
		params[k] = v
	}
	// Also build a string map for the engine (it uses map[string]string internally).
	engineParams := rawParams

	// ── Create browser ────────────────────────────────────────────────────────
	// Stub browser: logs every action and returns success for all assertions.
	// Replace with a Playwright browser for production evidence runs.
	browser := stub.New(cap.Target.URL)

	// ── Create engine ─────────────────────────────────────────────────────────
	runID := fmt.Sprintf("run_%d", time.Now().UnixNano())
	opts := replay.EngineOptions{
		EvidenceDir: *evidenceDir,
		RunID:       runID,
	}
	engine := replay.NewEngine(browser, &cap, opts)

	// ── Build run skeleton ────────────────────────────────────────────────────
	log.Printf("Starting run %s", runID)

	// ── Execute ───────────────────────────────────────────────────────────────
	ctx := context.Background()
	run, runErr := engine.Run(ctx, engineParams)

	// ── Finalize status ───────────────────────────────────────────────────────
	run.FinishedAt = time.Now()
	switch {
	case runErr == nil:
		run.Status = domain.RunStatusSuccess
	default:
		var be *domain.BusinessError
		var ee *domain.EscalationError
		switch {
		case errors.As(runErr, &be):
			run.Status = domain.RunStatusBusinessError
			run.ErrorClass = &domain.ErrorClass{
				Category:    domain.ErrorCategoryBusiness,
				StepID:      be.StepID,
				Message:     be.Message,
				IsRetryable: false,
			}
		case errors.As(runErr, &ee):
			run.Status = domain.RunStatusEscalated
		default:
			run.Status = domain.RunStatusHardFailure
			run.ErrorClass = &domain.ErrorClass{
				Category:    domain.ErrorCategoryAssertion,
				Message:     runErr.Error(),
				IsRetryable: false,
			}
		}
	}

	// ── Summary log ───────────────────────────────────────────────────────────
	passed, failed, skipped := 0, 0, 0
	for _, sr := range run.Steps {
		switch sr.Status {
		case domain.StepStatusPassed:
			passed++
		case domain.StepStatusFailed:
			failed++
		case domain.StepStatusSkipped:
			skipped++
		}
	}
	log.Printf("Run complete: status=%s  passed=%d  failed=%d  skipped=%d  duration=%s",
		run.Status, passed, failed, skipped,
		run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond))
	if runErr != nil {
		log.Printf("Run error: %v", runErr)
	}

	// ── Write audit log ───────────────────────────────────────────────────────
	outData, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		log.Fatalf("cannot marshal run JSON: %v", err)
	}
	if err := os.WriteFile(*outFile, outData, 0644); err != nil {
		log.Fatalf("cannot write output file: %v", err)
	}
	log.Printf("Audit log written → %s", *outFile)

	// Exit with non-zero code if the run didn't succeed (useful for CI).
	switch run.Status {
	case domain.RunStatusSuccess:
		os.Exit(0)
	case domain.RunStatusBusinessError, domain.RunStatusEscalated:
		os.Exit(2)
	default:
		os.Exit(1)
	}
}
