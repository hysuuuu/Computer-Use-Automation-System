// cmd/replay/main.go
//
// Standalone replay runner for evidence generation.
//
// Usage (stub browser, default):
//
//	go run ./cmd/replay \
//	  --capability evidence/cap_checkout.json \
//	  --params '{"username":"standard_user","password":"secret_sauce","item_name":"sauce-labs-backpack","first_name":"John","last_name":"Doe","zip":"12345"}' \
//	  --out evidence/replay_success.json
//
// Usage (real Chromium browser):
//
//	go run ./cmd/replay \
//	  --real \
//	  --chromium /home/hysu/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome \
//	  --capability evidence/cap_checkout.json \
//	  --params '{"username":"standard_user","password":"secret_sauce","item_name":"sauce-labs-backpack","first_name":"John","last_name":"Doe","zip":"12345"}' \
//	  --out evidence/replay_real.json
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
	"computer-use/pwbrowser"
	"computer-use/replay"
	"computer-use/stub"
)

func main() {
	capFile     := flag.String("capability",   "",        "Path to capability JSON file (required)")
	paramsJSON  := flag.String("params",       "{}",      "JSON object of runtime parameters")
	outFile     := flag.String("out",          "",        "Path to write the Run audit log JSON (required)")
	evidenceDir := flag.String("evidence-dir", "evidence","Directory for screenshots and audit logs")
	useReal     := flag.Bool("real",           false,     "Use a real Chromium browser instead of the stub")
	chromiumPath := flag.String("chromium",   "",        "Path to Chromium executable (used with --real)")
	headless    := flag.Bool("headless",       true,      "Run Chromium headless (used with --real)")
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
	var rawParams map[string]string
	if err := json.Unmarshal([]byte(*paramsJSON), &rawParams); err != nil {
		log.Fatalf("cannot parse --params JSON: %v", err)
	}
	params := make(map[string]any, len(rawParams))
	for k, v := range rawParams {
		params[k] = v
	}
	engineParams := rawParams

	// ── Create browser ────────────────────────────────────────────────────────
	var browser replay.Browser
	var cleanup func()

	if *useReal {
		log.Printf("Launching real Chromium browser (headless=%v path=%q)...", *headless, *chromiumPath)
		b, cleanupFn, launchErr := pwbrowser.Launch(pwbrowser.Options{
			ChromiumPath: *chromiumPath,
			Headless:     *headless,
			EvidenceDir:  *evidenceDir,
			})
		if launchErr != nil {
			log.Fatalf("could not launch browser: %v", launchErr)
		}
		browser = b
		cleanup = cleanupFn
		log.Printf("Browser ready.")
	} else {
		log.Printf("Using stub browser (pass --real to use Chromium).")
		browser = stub.New(cap.Target.URL)
		cleanup = func() {}
	}
	defer cleanup()

	// ── Create engine ─────────────────────────────────────────────────────────
	runID := fmt.Sprintf("run_%d", time.Now().UnixNano())
	opts := replay.EngineOptions{
		EvidenceDir: *evidenceDir,
		RunID:       runID,
	}
	engine := replay.NewEngine(browser, &cap, opts)

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

	switch run.Status {
	case domain.RunStatusSuccess:
		os.Exit(0)
	case domain.RunStatusBusinessError, domain.RunStatusEscalated:
		os.Exit(2)
	default:
		os.Exit(1)
	}
}
