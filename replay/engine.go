// Package replay implements the deterministic Capability replay engine.
// It depends on the domain package and the Browser interface.
// It has no direct dependency on Playwright — that is injected via Browser.
package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"computer-use/domain"
)

// Browser is the interface the engine uses to interact with the page.
// In production this is backed by Playwright. In tests it is mocked.
type Browser interface {
	// Navigate navigates to the given URL.
	Navigate(ctx context.Context, url string) error
	// Click resolves the locator and clicks the element.
	Click(ctx context.Context, loc domain.Locator) error
	// Fill clears and fills the located input with value.
	Fill(ctx context.Context, loc domain.Locator, value string) error
	// Select chooses an option in a <select> element.
	Select(ctx context.Context, loc domain.Locator, value string) error
	// Check checks or unchecks a checkbox.
	Check(ctx context.Context, loc domain.Locator, checked bool) error
	// KeyPress sends a keyboard event to the located element.
	KeyPress(ctx context.Context, loc domain.Locator, key string) error
	// TextVisible returns true if the given text is visible on the page.
	TextVisible(ctx context.Context, text string) (bool, error)
	// URLContains returns true if the current URL contains the substring.
	URLContains(ctx context.Context, substring string) (bool, error)
	// ElementExists returns true if the located element is present in the DOM.
	ElementExists(ctx context.Context, loc domain.Locator) (bool, error)
	// GetText extracts the visible text of the located element.
	GetText(ctx context.Context, loc domain.Locator) (string, error)
	// Screenshot captures the current page and returns the file path.
	Screenshot(ctx context.Context, name string) (string, error)
}

// EngineOptions configure the replay engine.
type EngineOptions struct {
	// DryRun resolves all locators and takes screenshots but executes no actions.
	DryRun         bool
	DefaultTimeout time.Duration
	EvidenceDir    string // directory to write screenshots
	RunID          string
}

// Engine walks a Capability's steps and executes them via the Browser.
type Engine struct {
	browser Browser
	cap     *domain.Capability
	opts    EngineOptions
	vars    map[string]string // runtime variables: params + captured extract values
	results []domain.StepResult
	status  domain.RunStatus
}

// NewEngine creates a new replay engine for the given capability.
func NewEngine(browser Browser, cap *domain.Capability, opts EngineOptions) *Engine {
	return &Engine{
		browser: browser,
		cap:     cap,
		opts:    opts,
		vars:    make(map[string]string),
		status:  domain.RunStatusRunning,
	}
}

// Run executes the capability against the injected browser.
// It respects ctx for cancellation (e.g. a per-run timeout).
func (e *Engine) Run(ctx context.Context, params map[string]string) (*domain.Run, error) {
	startedAt := time.Now()

	// Populate runtime variables from params (secrets will be redacted on output).
	for k, v := range params {
		e.vars[k] = v
	}

	// Build a step-ID → index map for O(1) branch jumps.
	stepIndex := make(map[string]int, len(e.cap.Steps))
	for i, s := range e.cap.Steps {
		stepIndex[s.ID] = i
	}

	i := 0
	for i < len(e.cap.Steps) {
		select {
		case <-ctx.Done():
			e.status = domain.RunStatusPartial
			return e.buildRun(startedAt), fmt.Errorf("run cancelled: %w", ctx.Err())
		default:
		}

		step := e.cap.Steps[i]
		nextID, err := e.dispatch(ctx, step)
		if err != nil {
			// BusinessError and EscalationError set their own status inside dispatch.
			if e.status == domain.RunStatusRunning {
				e.status = domain.RunStatusHardFailure
			}
			return e.buildRun(startedAt), err
		}

		if nextID != "" {
			idx, ok := stepIndex[nextID]
			if !ok {
				e.status = domain.RunStatusHardFailure
				return e.buildRun(startedAt), fmt.Errorf("branch target step %q not found", nextID)
			}
			i = idx
		} else {
			i++
		}
	}

	e.status = domain.RunStatusSuccess
	return e.buildRun(startedAt), nil
}

// dispatch routes a step to the correct handler and returns the next step ID
// (non-empty only for Branch steps) or an error.
func (e *Engine) dispatch(ctx context.Context, step domain.Step) (nextStepID string, err error) {
	// Safety gate: pause before critical/approved steps.
	if step.RequiresApproval || step.Risk == domain.RiskCritical {
		return "", e.escalate(step)
	}

	start := time.Now()
	var result domain.StepResult

	defer func() {
		result.StepID = step.ID
		result.DurationMs = time.Since(start).Milliseconds()
		if err != nil && result.Status == "" {
			result.Status = domain.StepStatusFailed
			result.Err = err.Error()
		}
		e.results = append(e.results, result)
	}()

	switch step.Type {
	case domain.StepTypeAction:
		err = e.executeAction(ctx, step)
	case domain.StepTypeAssert:
		err = e.executeAssert(ctx, step)
	case domain.StepTypeBranch:
		nextStepID, err = e.executeBranch(ctx, step)
	case domain.StepTypeExtract:
		err = e.executeExtract(ctx, step)
	case domain.StepTypeCheckpoint:
		// No-op during a live run. Marks a safe resume point.
		result.Status = domain.StepStatusPassed
	case domain.StepTypeWait:
		// Placeholder: wait steps are handled by browser timeouts in real Playwright.
		result.Status = domain.StepStatusPassed
	default:
		err = fmt.Errorf("unknown step type: %q", step.Type)
	}

	if err != nil {
		policyErr := e.applyErrorPolicy(ctx, step, err)
		if policyErr == nil {
			// Policy handled it (e.g. skip) — mark the result here so the
			// defer doesn't double-append with an empty status.
			result.Status = domain.StepStatusSkipped
			result.Err = err.Error()
		}
		return nextStepID, policyErr
	}

	if result.Status == "" {
		result.Status = domain.StepStatusPassed
	}
	return nextStepID, nil
}

// executeAction performs a browser interaction.
func (e *Engine) executeAction(ctx context.Context, step domain.Step) error {
	if step.Action == nil {
		return fmt.Errorf("step %s: type=action but Action is nil", step.ID)
	}

	value := e.interpolate(step.Action.Value)

	if e.opts.DryRun {
		// In dry-run mode, resolve locator to verify it exists but do not act.
		if step.Locator != nil {
			if _, err := e.browser.ElementExists(ctx, *step.Locator); err != nil {
				return err
			}
		}
		return nil
	}

	switch step.Action.Kind {
	case domain.ActionKindNavigate:
		return e.browser.Navigate(ctx, value)
	case domain.ActionKindClick:
		if step.Locator == nil {
			return fmt.Errorf("step %s: click requires a locator", step.ID)
		}
		return e.browser.Click(ctx, *step.Locator)
	case domain.ActionKindFill:
		if step.Locator == nil {
			return fmt.Errorf("step %s: fill requires a locator", step.ID)
		}
		return e.browser.Fill(ctx, *step.Locator, value)
	case domain.ActionKindSelect:
		if step.Locator == nil {
			return fmt.Errorf("step %s: select requires a locator", step.ID)
		}
		return e.browser.Select(ctx, *step.Locator, value)
	case domain.ActionKindCheck:
		if step.Locator == nil {
			return fmt.Errorf("step %s: check requires a locator", step.ID)
		}
		return e.browser.Check(ctx, *step.Locator, true)
	case domain.ActionKindUncheck:
		if step.Locator == nil {
			return fmt.Errorf("step %s: uncheck requires a locator", step.ID)
		}
		return e.browser.Check(ctx, *step.Locator, false)
	case domain.ActionKindKeyPress:
		if step.Locator == nil {
			return fmt.Errorf("step %s: key_press requires a locator", step.ID)
		}
		return e.browser.KeyPress(ctx, *step.Locator, step.Action.Key)
	default:
		return fmt.Errorf("step %s: unsupported action kind %q", step.ID, step.Action.Kind)
	}
}

// executeAssert checks page state without side effects.
func (e *Engine) executeAssert(ctx context.Context, step domain.Step) error {
	if step.Assert == nil {
		return fmt.Errorf("step %s: type=assert but Assert is nil", step.ID)
	}
	_, err := e.evaluate(ctx, step.ID, *step.Assert)
	return err
}

// executeExtract reads a value from the page and stores it in e.vars.
func (e *Engine) executeExtract(ctx context.Context, step domain.Step) error {
	if step.Locator == nil {
		return fmt.Errorf("step %s: extract requires a locator", step.ID)
	}
	if step.Assert == nil || step.Assert.CaptureAs == "" {
		return fmt.Errorf("step %s: extract requires Assert.CaptureAs", step.ID)
	}

	text, err := e.browser.GetText(ctx, *step.Locator)
	if err != nil {
		return err
	}
	e.vars[step.Assert.CaptureAs] = text
	return nil
}

// executeBranch evaluates the branch condition and returns the ID of the next step.
func (e *Engine) executeBranch(ctx context.Context, step domain.Step) (string, error) {
	if step.Branch == nil {
		return "", fmt.Errorf("step %s: type=branch but Branch is nil", step.ID)
	}
	b := step.Branch

	met, err := e.evaluate(ctx, step.ID, b.Condition)
	if err != nil {
		// BusinessError propagates up — branch condition matched an expected bad state.
		return "", err
	}

	if met {
		if len(b.IfTrue) == 0 {
			return "", fmt.Errorf("step %s: branch condition true but IfTrue is empty", step.ID)
		}
		return b.IfTrue[0], nil
	}
	if len(b.IfFalse) == 0 {
		return "", fmt.Errorf("step %s: branch condition false but IfFalse is empty", step.ID)
	}
	return b.IfFalse[0], nil
}

// evaluate checks an Assertion against the live page and applies IsBusinessOutcome logic.
// Returns (true, nil) if the condition is met, (false, nil) if not met and not a business outcome,
// or (false, *BusinessError) if not met and IsBusinessOutcome=true.
func (e *Engine) evaluate(ctx context.Context, stepID string, a domain.Assertion) (bool, error) {
	expected := e.interpolate(a.Expected)
	var met bool
	var err error

	switch a.Kind {
	case domain.AssertionKindTextVisible:
		met, err = e.browser.TextVisible(ctx, expected)
	case domain.AssertionKindTextNotVisible:
		visible, verr := e.browser.TextVisible(ctx, expected)
		met, err = !visible, verr
	case domain.AssertionKindURLContains:
		met, err = e.browser.URLContains(ctx, expected)
	case domain.AssertionKindElementExists:
		// Element exists assertions require a locator — use Expected as CSS selector fallback.
		loc := domain.Locator{Primary: domain.LocatorStrategy{Kind: domain.LocatorKindCSS, Value: expected}}
		met, err = e.browser.ElementExists(ctx, loc)
	default:
		return false, fmt.Errorf("step %s: unsupported assertion kind %q", stepID, a.Kind)
	}

	if err != nil {
		return false, err
	}

	if !met && a.IsBusinessOutcome {
		e.status = domain.RunStatusBusinessError
		return false, &domain.BusinessError{
			StepID:  stepID,
			Message: fmt.Sprintf("expected business outcome: %q not found", expected),
		}
	}

	return met, nil
}

// applyErrorPolicy handles a step failure according to its configured policy.
func (e *Engine) applyErrorPolicy(ctx context.Context, step domain.Step, err error) error {
	// BusinessErrors and EscalationErrors are already fully handled — propagate immediately.
	var be *domain.BusinessError
	if errors.As(err, &be) {
		return be
	}
	var ee *domain.EscalationError
	if errors.As(err, &ee) {
		return ee
	}

	switch step.OnError.Strategy {
	case "retry":
		for attempt := 0; attempt < step.OnError.MaxRetries; attempt++ {
			if step.OnError.RetryDelayMs > 0 {
				timer := time.NewTimer(time.Duration(step.OnError.RetryDelayMs) * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			// Re-execute the specific action directly to avoid recursive dispatch.
			var retryErr error
			switch step.Type {
			case domain.StepTypeAction:
				retryErr = e.executeAction(ctx, step)
			case domain.StepTypeAssert:
				retryErr = e.executeAssert(ctx, step)
			default:
				retryErr = fmt.Errorf("retry not supported for step type %q", step.Type)
			}
			if retryErr == nil {
				return nil
			}
			err = retryErr
		}
		e.status = domain.RunStatusHardFailure
		return err

	case "skip":
		// Return nil so the caller marks the step as skipped and continues.
		return nil

	case "human_escalate":
		return e.escalate(step)

	default: // "fail_fast"
		e.status = domain.RunStatusHardFailure
		return err
	}
}

// escalate pauses execution by setting status and returning an EscalationError.
func (e *Engine) escalate(step domain.Step) error {
	msg := step.OnError.EscalationMessage
	if msg == "" {
		msg = fmt.Sprintf("Step %q requires human approval before execution.", step.Description)
	}
	e.status = domain.RunStatusEscalated
	return &domain.EscalationError{StepID: step.ID, Message: msg}
}

// interpolate replaces "{{param_name}}" tokens with their runtime values.
func (e *Engine) interpolate(s string) string {
	for k, v := range e.vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}

// buildRun constructs the Run audit log from accumulated step results.
func (e *Engine) buildRun(startedAt time.Time) *domain.Run {
	// Redact secret parameters before writing to the audit log.
	redacted := make(map[string]any, len(e.cap.Params))
	for _, p := range e.cap.Params {
		if p.Type == "secret" {
			redacted[p.Name] = "[REDACTED]"
		} else {
			redacted[p.Name] = e.vars[p.Name]
		}
	}

	return &domain.Run{
		RunID:        e.opts.RunID,
		CapabilityID: e.cap.ID,
		Version:      e.cap.Version,
		Params:       redacted,
		Status:       e.status,
		Steps:        e.results,
		StartedAt:    startedAt,
		FinishedAt:   time.Now(),
	}
}
