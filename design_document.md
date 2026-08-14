# Computer-Use Automation System — Technical Design Document

> **Language:** Go  
> **Target Application:** Sauce Labs Demo (saucedemo.com)  
> **Author:** Take-Home Assessment Submission

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Problem Analysis](#2-problem-analysis)
3. [Target Application](#3-target-application)
4. [Domain Model & Schema](#4-domain-model--schema)
5. [Planner — LLM Integration](#5-planner--llm-integration)
6. [Replay Engine](#6-replay-engine)
7. [Error Taxonomy](#7-error-taxonomy)
8. [Safety Pipeline](#8-safety-pipeline)
9. [API Design](#9-api-design)
10. [Project Structure](#10-project-structure)
11. [Problems & Solutions](#11-problems--solutions)
12. [Design Decisions Summary](#12-design-decisions-summary)

---

## 1. System Overview

This system automates interactions with a target web application using a large language model (LLM) as the planner and Playwright (via `playwright-community/playwright-go`) as the execution engine. 

The core loop is:

```
Natural Language Instruction
         ↓
    [Planner]  LLM generates a structured, versioned Capability (JSON)
         ↓
   [Validator] Capability is verified against the live page before execution
         ↓
[Safety Pipeline] Risk assessment gates destructive actions
         ↓
 [Replay Engine]  Executes the Capability step-by-step via Playwright
         ↓
  [Run Record]    Emits a typed audit log with evidence (screenshots)
```

The system is explicitly designed around **two separate concerns**:
- **Planning** — what to do (LLM's job)
- **Execution** — how to do it safely and reliably (engine's job)

This separation is what makes capabilities reusable, auditable, and safe.

---

## 2. Problem Analysis

A naïve computer-use agent simply asks an LLM to emit browser commands and executes them. This approach fails in production for several well-understood reasons:

| Problem | Consequence |
|---|---|
| LLM outputs raw Playwright scripts | Not auditable, not replayable, not parameterizable |
| Selectors tied to brittle CSS IDs | Breaks on any UI update |
| No distinction between business errors and technical failures | Retrying "locked out user" forever |
| No safety layer | LLM could submit a $10,000 purchase |
| No human-in-the-loop mechanism | System is fully autonomous even when it shouldn't be |
| Single LLM call with full HTML | Context window exhaustion, poor selector quality |

Each of these problems has a concrete design solution in this system. See [§11](#11-problems--solutions) for the full mapping.

---

## 3. Target Application

**Site:** [https://www.saucedemo.com](https://www.saucedemo.com) (Sauce Labs Demo)

Chosen because it covers all required complexity:

| Feature | Why it matters |
|---|---|
| Login with multiple user types | Exercises business error paths (`locked_out_user`) |
| Multi-step checkout flow | Exercises checkpoints and stateful replay |
| Product inventory with search/filter | Exercises parameterized capabilities |
| Stable `data-testid` attributes | Exercises locator priority (testid > css) |
| Deterministic, publicly available | No auth tokens required; reproducible |

**Demonstrated capability:** `"Log in and add a product to cart, then complete checkout"` — a 12-step flow that exercises every part of the system.

---

## 4. Domain Model & Schema

The central artifact of the system is the `Capability`. It is the contract between the planner and the replay engine — a typed, versioned, serializable description of what to do, not how to do it in Playwright terms.

### 4.1 Capability (Top-Level Artifact)

```go
// Capability is the versioned artifact emitted after a successful LLM planning run.
// It is the contract between the planner and the replay engine.
type Capability struct {
    ID          string     `json:"id"`          // e.g., "cap_checkout_guest_v1"
    Version     int        `json:"version"`     // monotonically increasing
    Description string     `json:"description"` // human-readable intent
    Target      Target     `json:"target"`
    Params      []ParamDef `json:"params"`      // parameterizable input slots
    Steps       []Step     `json:"steps"`       // ordered execution plan
    SafetyPolicy SafetyPolicy `json:"safety_policy"`
    CreatedAt   time.Time  `json:"created_at"`
    CreatedBy   string     `json:"created_by"`  // "llm:gpt-4o" or "human"
}

type Target struct {
    URL         string `json:"url"`
    Application string `json:"application"`
    Description string `json:"description"`
}

// ParamDef turns a recorded capability into a reusable "function".
// At replay time, {{param_name}} tokens in Action.Value are substituted.
type ParamDef struct {
    Name        string `json:"name"`        // e.g., "username"
    Type        string `json:"type"`        // "string" | "secret" | "number" | "bool"
    Description string `json:"description"`
    Required    bool   `json:"required"`
    Default     string `json:"default,omitempty"`
}
```

### 4.2 Step — The Atomic Unit

```go
// Step represents one atomic interaction, assertion, or decision point.
// One step = one thing the engine does.
// It is NOT a raw Playwright call — the engine translates it.
type Step struct {
    ID          string      `json:"id"`          // unique within capability
    Type        StepType    `json:"type"`
    Description string      `json:"description"` // for audit trail and human review
    Locator     *Locator    `json:"locator,omitempty"`
    Action      *Action     `json:"action,omitempty"`
    Assert      *Assertion  `json:"assert,omitempty"`
    Branch      *Branch     `json:"branch,omitempty"`
    Risk        RiskLevel   `json:"risk"`        // set by LLM, verified by safety scanner
    RequiresApproval bool   `json:"requires_approval"`
    OnError     ErrorPolicy `json:"on_error"`
    TimeoutMs   int         `json:"timeout_ms"`  // 0 = use system default
}

type StepType string

const (
    StepTypeAction     StepType = "action"     // interact with a UI element
    StepTypeAssert     StepType = "assert"      // verify state without side effects
    StepTypeBranch     StepType = "branch"      // conditional fork
    StepTypeExtract    StepType = "extract"     // scrape value into named variable
    StepTypeWait       StepType = "wait"        // wait for network idle, selector, or duration
    StepTypeCheckpoint StepType = "checkpoint"  // savepoint for partial replay
)
```

### 4.3 Locator — Resilient Element Targeting

The most common real-world failure is a locator that breaks when the page is updated. We solve this with a **priority-ordered fallback chain**. The engine tries each strategy in sequence until one resolves.

```go
// Locator uses a fallback chain. The engine tries Primary first,
// then each Fallback in order. This survives minor UI changes
// without requiring re-planning.
type Locator struct {
    Primary   LocatorStrategy   `json:"primary"`
    Fallbacks []LocatorStrategy `json:"fallbacks,omitempty"`
    Frame     string            `json:"frame,omitempty"` // for iframe support
}

type LocatorStrategy struct {
    Kind  LocatorKind `json:"kind"`
    Value string      `json:"value"`
    Name  string      `json:"name,omitempty"` // ARIA accessible name
}

type LocatorKind string

const (
    LocatorKindTestID  LocatorKind = "test-id" // Most stable — preferred primary
    LocatorKindRole    LocatorKind = "role"     // ARIA role + accessible name
    LocatorKindLabel   LocatorKind = "label"    // Form label association
    LocatorKindText    LocatorKind = "text"     // Visible text content
    LocatorKindCSS     LocatorKind = "css"      // Last resort — most brittle
    LocatorKindXPath   LocatorKind = "xpath"    // Absolute fallback
)
```

**Locator priority rule** enforced at planning time:
> `test-id` → `role` → `label` → `text` → `css` → `xpath`

The planner's system prompt explicitly instructs the LLM to use this ordering. The validator rejects capabilities where `css` is the `primary` locator when a `test-id` exists on the element.

### 4.4 Action

```go
type Action struct {
    Kind  ActionKind `json:"kind"`
    Value string     `json:"value,omitempty"` // supports "{{param_name}}" interpolation
    Key   string     `json:"key,omitempty"`   // for key_press: "Enter", "Tab", etc.
}

type ActionKind string

const (
    ActionKindClick    ActionKind = "click"
    ActionKindFill     ActionKind = "fill"      // clear + type
    ActionKindSelect   ActionKind = "select"
    ActionKindCheck    ActionKind = "check"
    ActionKindUncheck  ActionKind = "uncheck"
    ActionKindKeyPress ActionKind = "key_press"
    ActionKindScroll   ActionKind = "scroll"
    ActionKindHover    ActionKind = "hover"
    ActionKindNavigate ActionKind = "navigate"  // no locator needed
    ActionKindUpload   ActionKind = "upload"
)
```

### 4.5 Assertion — The Core of Error Taxonomy

This is the single most important design decision in the schema. The `IsBusinessOutcome` flag is what allows the engine to distinguish between a **clean expected stop** and an **unexpected failure**.

```go
type Assertion struct {
    Kind     AssertionKind `json:"kind"`
    Expected string        `json:"expected"` // supports "{{param_name}}" interpolation
    
    // KEY DESIGN DECISION:
    // If true: this assertion FAILING means we reached an expected business state.
    //   → Engine sets RunStatus = "business_error" and stops cleanly. No retry.
    // If false: this assertion FAILING is an unexpected technical failure.
    //   → Engine applies OnError policy (retry / escalate / fail_fast).
    IsBusinessOutcome bool `json:"is_business_outcome"`
    
    CaptureAs string `json:"capture_as,omitempty"` // extract step: store value as variable
}

type AssertionKind string

const (
    AssertionKindTextVisible    AssertionKind = "text_visible"
    AssertionKindTextNotVisible AssertionKind = "text_not_visible"
    AssertionKindURLContains    AssertionKind = "url_contains"
    AssertionKindElementExists  AssertionKind = "element_exists"
    AssertionKindElementHidden  AssertionKind = "element_hidden"
    AssertionKindValueEquals    AssertionKind = "value_equals"
    AssertionKindCustomScript   AssertionKind = "custom_script" // JS evaluate
)
```

### 4.6 Branch — Conditional Logic Without Re-planning

```go
// Branch handles multiple expected page states (e.g., CAPTCHA vs normal login)
// without invoking the LLM again.
type Branch struct {
    Condition Assertion `json:"condition"`
    IfTrue    []string  `json:"if_true"`  // step IDs to jump to
    IfFalse   []string  `json:"if_false"` // step IDs to jump to
}
```

### 4.7 Error Policy

```go
type ErrorPolicy struct {
    // "fail_fast" | "retry" | "skip" | "human_escalate"
    Strategy          string `json:"strategy"`
    MaxRetries        int    `json:"max_retries,omitempty"`
    RetryDelayMs      int    `json:"retry_delay_ms,omitempty"`
    EscalationMessage string `json:"escalation_message,omitempty"`
}
```

### 4.8 Run — The Execution Record

```go
// Run is the audit log written after execution.
// It captures actual runtime values, not just the plan.
type Run struct {
    RunID        string         `json:"run_id"`
    CapabilityID string         `json:"capability_id"`
    Version      int            `json:"version"`
    Params       map[string]any `json:"params"`
    Status       RunStatus      `json:"status"`
    Steps        []StepResult   `json:"steps"`
    StartedAt    time.Time      `json:"started_at"`
    FinishedAt   time.Time      `json:"finished_at"`
    ErrorClass   *ErrorClass    `json:"error_class,omitempty"`
    Escalation   *Escalation    `json:"escalation,omitempty"`
}

type RunStatus string

const (
    RunStatusSuccess       RunStatus = "success"
    RunStatusBusinessError RunStatus = "business_error" // expected, clean stop
    RunStatusHardFailure   RunStatus = "hard_failure"   // unexpected, investigate
    RunStatusEscalated     RunStatus = "escalated"      // human took over
    RunStatusPartial       RunStatus = "partial"        // stopped at checkpoint
)

type StepResult struct {
    StepID      string        `json:"step_id"`
    Status      StepStatus    `json:"status"`   // "passed" | "failed" | "skipped"
    ActualValue string        `json:"actual_value,omitempty"`
    Screenshot  string        `json:"screenshot,omitempty"` // absolute path
    Err         string        `json:"err,omitempty"`
    DurationMs  int64         `json:"duration_ms"`
}

type ErrorClass struct {
    Category    ErrorCategory `json:"category"`
    StepID      string        `json:"step_id"`
    Message     string        `json:"message"`
    IsRetryable bool          `json:"is_retryable"`
}
```

---

## 5. Planner — LLM Integration

### 5.1 The Core Problem With Naive Planning

A single LLM call that receives raw HTML and outputs a plan has two critical weaknesses:
1. **Context explosion** — real pages contain 50,000+ HTML tokens
2. **Hallucinated selectors** — LLMs confidently emit `#login-btn` when the real ID is `#login-button`

### 5.2 Input: Screenshot + Pruned Accessibility Tree

Instead of raw HTML, the planner receives a `PageContext`:

```go
type PageContext struct {
    Screenshot      []byte          // PNG, for visual layout understanding
    AccessibilityTree []A11yNode    // pruned: only roles, labels, testids, visible text
    URL             string
    Title           string
}

type A11yNode struct {
    Role     string
    Name     string   // accessible name
    TestID   string   // data-testid attribute
    Text     string   // visible text content
    Children []A11yNode
}
```

The accessibility tree is compact (hundreds of tokens vs. tens of thousands for HTML), semantically rich, and directly maps to our preferred locator kinds (`role`, `test-id`, `label`).

### 5.3 The Self-Healing Planning Loop

```
Instruction + PageContext
         ↓
  [1] planner.Generate()    → LLM call with planning system prompt
         ↓
  [2] validator.Validate()  → resolve every Locator against live page
         ↓
    All pass? ──────────────────────────────────► emit Capability ✓
         │
    Some fail? → build ValidationError list
         ↓
  [3] planner.Fix()         → second LLM call: "these locators failed,
                              here are valid alternatives from the page"
         ↓
  [4] validator.Validate()  → re-check fixed steps
         ↓
    Pass? ──────────────────────────────────────► emit Capability ✓
    Still fail after 3 attempts? ───────────────► PlannerError, abort

```

```go
func (p *Planner) Plan(ctx context.Context, instruction string, pageCtx PageContext) (*Capability, error) {
    cap, err := p.generate(ctx, instruction, pageCtx)
    if err != nil {
        return nil, err
    }

    for attempt := 0; attempt < 3; attempt++ {
        errs := p.validator.Validate(ctx, cap, pageCtx)
        if len(errs) == 0 {
            return cap, nil // all locators resolved
        }
        cap, err = p.fix(ctx, cap, errs, pageCtx)
        if err != nil {
            return nil, err
        }
    }
    return nil, &PlannerError{Message: "failed to produce valid locators after 3 attempts"}
}
```

### 5.4 Planner System Prompt (Key Constraints)

The planning system prompt enforces structure. Critical rules embedded in it:

```
1. You MUST output valid JSON matching the Capability schema. No prose.
2. For locators, you MUST use this priority order:
   test-id > role > label > text > css > xpath
   Never use css as the primary locator if test-id or role is available.
3. Any step that submits, purchases, deletes, or is irreversible MUST have:
   risk: "critical" and requires_approval: true
4. Any expected business failure (login error, item not found) MUST have:
   is_business_outcome: true on its assertion.
5. Every capability MUST have at least one checkpoint step after a
   stateful, irreversible action.
```

---

## 6. Replay Engine

### 6.1 Engine Structure

```go
type Engine struct {
    page    playwright.Page
    cap     *Capability
    opts    EngineOptions
    vars    map[string]string // runtime variables from extract steps
    results []StepResult
    status  RunStatus
}

type EngineOptions struct {
    DryRun         bool          // resolve locators but don't execute actions
    DefaultTimeout time.Duration
    EvidenceDir    string        // where to save screenshots
}
```

### 6.2 Execution Loop

```go
func (e *Engine) Run(ctx context.Context, params map[string]string) (*Run, error) {
    e.vars = params
    stepIndex := make(map[string]int, len(e.cap.Steps))
    for i, s := range e.cap.Steps {
        stepIndex[s.ID] = i
    }

    i := 0
    for i < len(e.cap.Steps) {
        select {
        case <-ctx.Done():
            // Stopped at last checkpoint — partial run
            e.status = RunStatusPartial
            return e.buildRun(), ctx.Err()
        default:
        }

        step := e.cap.Steps[i]
        nextID, err := e.dispatch(ctx, step)
        if err != nil {
            return e.buildRun(), err
        }

        if nextID != "" {
            i = stepIndex[nextID] // branch jump
        } else {
            i++
        }
    }

    e.status = RunStatusSuccess
    return e.buildRun(), nil
}
```

### 6.3 Step Dispatch

```go
func (e *Engine) dispatch(ctx context.Context, step Step) (nextStepID string, err error) {
    // Safety gate: pause before critical steps
    if step.RequiresApproval || step.Risk == RiskCritical {
        return "", e.escalate(step, "Critical action requires human approval")
    }

    switch step.Type {
    case StepTypeAction:
        err = e.executeAction(ctx, step)
    case StepTypeAssert:
        err = e.executeAssert(ctx, step)
    case StepTypeBranch:
        nextStepID, err = e.executeBranch(ctx, step)
    case StepTypeExtract:
        err = e.executeExtract(ctx, step)
    case StepTypeCheckpoint:
        // Savepoint — no-op during live run, but marks resume point
        e.recordCheckpoint(step.ID)
    case StepTypeWait:
        err = e.executeWait(ctx, step)
    }

    if err != nil {
        return "", e.applyErrorPolicy(step, err)
    }
    return nextStepID, nil
}
```

### 6.4 Locator Resolution — Fallback Chain

```go
func (e *Engine) resolveLocator(ctx context.Context, loc *Locator) (playwright.Locator, error) {
    if loc.Frame != "" {
        // Switch into iframe before resolving
    }
    
    strategies := append([]LocatorStrategy{loc.Primary}, loc.Fallbacks...)
    var lastErr error
    
    for _, s := range strategies {
        l, err := e.toPlaywrightLocator(s)
        if err != nil {
            lastErr = err
            continue
        }
        // Check if element is actually present (non-blocking, short timeout)
        if err := l.WaitFor(playwright.LocatorWaitForOptions{
            State:   playwright.WaitForSelectorStateAttached,
            Timeout: playwright.Float(500),
        }); err != nil {
            lastErr = err
            continue
        }
        return l, nil // found it
    }
    
    return nil, &LocatorError{
        Strategies: strategies,
        LastErr:    lastErr,
    }
}
```

### 6.5 Error Policy Application

```go
func (e *Engine) applyErrorPolicy(step Step, err error) error {
    // Business errors are clean stops, never retried
    var be *BusinessError
    if errors.As(err, &be) {
        e.status = RunStatusBusinessError
        return be
    }

    switch step.OnError.Strategy {
    case "retry":
        for attempt := 0; attempt < step.OnError.MaxRetries; attempt++ {
            time.Sleep(time.Duration(step.OnError.RetryDelayMs) * time.Millisecond)
            if _, err = e.dispatch(context.Background(), step); err == nil {
                return nil
            }
        }
        e.status = RunStatusHardFailure
        return err

    case "skip":
        e.recordSkipped(step.ID)
        return nil

    case "human_escalate":
        return e.escalate(step, step.OnError.EscalationMessage)

    default: // "fail_fast"
        e.status = RunStatusHardFailure
        return err
    }
}
```

### 6.6 Branch Execution

```go
func (e *Engine) executeBranch(ctx context.Context, step Step) (string, error) {
    b := step.Branch
    condMet, err := e.evaluate(ctx, b.Condition)
    if err != nil {
        return "", e.applyErrorPolicy(step, err)
    }

    if condMet {
        return b.IfTrue[0], nil
    }
    return b.IfFalse[0], nil
}

func (e *Engine) evaluate(ctx context.Context, a Assertion) (bool, error) {
    met, err := e.checkAssertion(ctx, a)
    if err != nil {
        return false, err
    }
    
    if !met && a.IsBusinessOutcome {
        // Not a crash — this is an expected business outcome
        return false, &BusinessError{
            StepID:  "branch_condition",
            Message: "expected business outcome reached: " + a.Expected,
        }
    }
    return met, nil
}
```

---

## 7. Error Taxonomy

The system distinguishes between **six error categories** across **five run statuses**.

```go
type ErrorCategory string

const (
    ErrorCategoryLocator    ErrorCategory = "locator_not_found"  // DOM changed
    ErrorCategoryTimeout    ErrorCategory = "timeout"             // page too slow
    ErrorCategoryNavigation ErrorCategory = "navigation_error"    // network/404
    ErrorCategoryAssertion  ErrorCategory = "assertion_failed"    // unexpected state
    ErrorCategoryBusiness   ErrorCategory = "business_rule"       // expected outcome
    ErrorCategoryPlanner    ErrorCategory = "planner_error"       // bad plan from LLM
)
```

### Taxonomy Table

| Scenario | `ErrorCategory` | `RunStatus` | `IsRetryable` | Triggered By |
|---|---|---|---|---|
| Login returns "Epic sadface" | `business_rule` | `business_error` | No | `is_business_outcome: true` |
| "Add to cart" button CSS not found | `locator_not_found` | `hard_failure` | Yes | Locator fallback exhausted |
| Page load takes >30s | `timeout` | `hard_failure` | Yes | `context.DeadlineExceeded` |
| Network 404 on navigation | `navigation_error` | `hard_failure` | No | Playwright navigation error |
| Inventory page assertion fails | `assertion_failed` | `hard_failure` | Yes | `is_business_outcome: false` |
| LLM emits structurally invalid JSON | `planner_error` | `hard_failure` | No | JSON unmarshal failure |
| Product not in stock | `business_rule` | `business_error` | No | `is_business_outcome: true` |
| CAPTCHA detected | *(none)* | `escalated` | N/A | `human_escalate` policy |
| Engine stopped mid-run | *(none)* | `partial` | N/A | `context.Done()` at checkpoint |

---

## 8. Safety Pipeline

The safety pipeline runs **before** any step is executed. It has four layers applied in sequence, from cheapest to most expensive.

### 8.1 Layer 1 — Keyword Pre-filter (Coarse, ~microseconds, free)

A fast coarse filter. Not the final arbiter — its only job is to flag steps as "suspicious" so they are routed to deeper analysis. Most steps never proceed past this layer.

```go
var criticalKeywords = []string{
    "delete", "remove account", "purchase", "place order",
    "confirm payment", "submit order", "deactivate", "close account",
    "transfer", "withdraw",
}

func (s *SafetyScanner) keywordScan(step Step) bool {
    text := strings.ToLower(step.Description + " " + actionValue(step))
    for _, kw := range criticalKeywords {
        if strings.Contains(text, kw) {
            return true // suspicious — escalate to Layer 2
        }
    }
    return false // safe — skip remaining layers
}
```

> **Why keywords first?** The vast majority of steps (scrolls, reads, navigation) are obviously safe. Routing them all through the LLM-as-Judge would add 100–500ms per step. Keywords provide a near-zero-cost early exit for the common case.

### 8.2 Layer 2 — Structural Risk (ActionKind Tiers, ~nanoseconds, free)

Some action types are structurally safer than others. This layer **downgrades** false positives from Layer 1.

```go
var actionRiskTier = map[ActionKind]RiskLevel{
    ActionKindExtract:  RiskLow,
    ActionKindScroll:   RiskLow,
    ActionKindHover:    RiskLow,
    ActionKindNavigate: RiskMedium,
    ActionKindFill:     RiskMedium,
    ActionKindSelect:   RiskMedium,
    ActionKindClick:    RiskMedium, // could be anything — context needed
    ActionKindKeyPress: RiskMedium,
    // Note: no ActionKind is inherently RiskCritical.
    // Critical classification requires context from Layer 3.
}
```

If a keyword-flagged step has `ActionKind = extract`, it cannot be critical — downgrade and skip Layer 3.

### 8.3 Layer 3 — LLM-as-Judge (Context-Aware, ~100–500ms, costs tokens)

A **second, dedicated LLM call** with a completely different system prompt. This model does not generate plans — it only evaluates risk. It receives the full `PageContext` (screenshot + accessibility tree) so it understands what page the step is executing on.

```go
type SafetyVerdict struct {
    StepID      string    `json:"step_id"`
    Risk        RiskLevel `json:"risk"`
    Reasoning   string    `json:"reasoning"`   // e.g., "This submits a payment form"
    Reversible  bool      `json:"reversible"`  // can this be undone?
    Confidence  float64   `json:"confidence"`  // 0.0 – 1.0
}

// Safety judge system prompt (excerpt):
// "You are a safety auditor, not a planner. Do NOT generate plans.
//  For each step, assess the REAL-WORLD consequence of executing it.
//  Evaluate: could this cause financial loss, data deletion, or an
//  irreversible state change? Is the action reversible?
//  A 'click' on a checkout page is critical. A 'click' on a filter
//  button is low risk. Context is everything."
```

This is what catches *"proceed to checkout"* (no critical keywords) as a high-risk action.

### 8.4 Layer 4 — Capability-Level Allowlist (Hard Ceiling)

A human-defined policy attached to each `Capability`. The engine enforces it regardless of what the LLM judge says. No LLM can override this.

```go
type SafetyPolicy struct {
    MaxRiskAllowed    RiskLevel `json:"max_risk_allowed"`    // e.g., "medium"
    RequireApprovalAt RiskLevel `json:"require_approval_at"` // e.g., "high"
    AllowedDomains    []string  `json:"allowed_domains"`     // must match target URL
    MaxMonetaryValue  *float64  `json:"max_monetary_value,omitempty"` // nil = no cap
}
```

`MaxMonetaryValue` is particularly important: if the engine extracts a cart total that exceeds the cap, it hard-stops before the purchase step regardless of risk classification.

### 8.5 Layer 5 — Critical Gate (Human Approval)

Any step that survives all layers with `RiskCritical` or `RequiresApproval: true` causes the engine to pause and emit an escalation. Execution cannot resume without a human `POST /runs/:id/resume`.

```go
if step.RequiresApproval || step.Risk == RiskCritical {
    e.run.Status = RunStatusEscalated
    e.run.Escalation = &Escalation{
        StepID:    step.ID,
        Message:   "⚠️ " + step.OnError.EscalationMessage,
        Screenshot: e.takeScreenshot(step.ID + "_pre_critical"),
        ResumeURL: "/runs/" + e.run.RunID + "/resume",
    }
    return // do NOT execute the step
}
```

### 8.6 Dry Run Mode

Before any live run, the API supports a `dry_run: true` mode. The engine resolves all locators and takes screenshots but executes no `action` steps. This lets a human review exactly what is about to happen before approving a live run.

---

## 9. API Design

### 9.1 Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/runs` | Start a plan + execute cycle |
| `GET` | `/runs/:id` | Poll run status and results |
| `POST` | `/runs/:id/resume` | Human resolves escalation |
| `GET` | `/capabilities/:id` | Inspect a saved capability |

### 9.2 `POST /runs` — Start a Run

Returns `202 Accepted` immediately. The run executes asynchronously.

**Request:**
```json
{
  "instruction": "Log in and add Sauce Labs Backpack to cart",
  "target": {
    "url": "https://www.saucedemo.com",
    "application": "saucedemo"
  },
  "params": {
    "username": "standard_user",
    "password": "secret_sauce",
    "product":  "Sauce Labs Backpack"
  },
  "options": {
    "timeout_seconds": 120,
    "dry_run": false,
    "save_capability": true
  }
}
```

**Response (202):**
```json
{
  "run_id": "run_abc123",
  "status": "running",
  "capability_id": "cap_saucedemo_add_to_cart_v1",
  "poll_url": "/runs/run_abc123"
}
```

### 9.3 `GET /runs/:id` — Poll Results

**Success:**
```json
{
  "run_id": "run_abc123",
  "status": "success",
  "output": { "cart_count": "1" },
  "evidence": {
    "screenshots": [
      "/evidence/run_abc123/step_004_after_login.png",
      "/evidence/run_abc123/step_009_cart_confirmed.png"
    ],
    "steps_passed": 9,
    "steps_total": 9,
    "duration_ms": 8420
  }
}
```

**Business Error:**
```json
{
  "run_id": "run_abc123",
  "status": "business_error",
  "error": {
    "category": "business_rule",
    "step_id": "step_005_err",
    "message": "Login rejected: 'Sorry, this user has been locked out.'"
  },
  "evidence": {
    "screenshots": ["/evidence/run_abc123/step_005_locked_out.png"],
    "steps_passed": 4,
    "steps_total": 9
  }
}
```

**Escalated:**
```json
{
  "run_id": "run_abc123",
  "status": "escalated",
  "escalation": {
    "step_id": "step_007",
    "message": "⚠️ Critical: About to place order for $29.99. Human approval required.",
    "screenshot": "/evidence/run_abc123/step_007_pre_critical.png",
    "resume_url": "/runs/run_abc123/resume"
  }
}
```

### 9.4 `POST /runs/:id/resume` — Human Resolution

```json
{
  "resolution": "approved",
  "resume_from_step": "step_007",
  "notes": "Confirmed: correct product, correct price"
}
```

`resolution` values: `"approved"` | `"completed_manually"` | `"skip"` | `"abort"`

### 9.5 Concurrency Model

```go
type RunManager struct {
    mu   sync.RWMutex
    runs map[string]*RunState
}

func (m *RunManager) Start(req RunRequest) string {
    runID := newRunID()
    state := &RunState{ID: runID, Status: RunStatusRunning}
    
    m.mu.Lock()
    m.runs[runID] = state
    m.mu.Unlock()
    
    go func() {
        run, err := executeRun(req, state) // writes to state via mutex
        _ = err
        m.mu.Lock()
        m.runs[runID] = &RunState{Run: run}
        m.mu.Unlock()
    }()
    
    return runID
}
```

---

## 10. Project Structure

### Dependency Rule

> **The `domain` package has zero external dependencies. Everything else depends on it. Nothing imports "inward" past it.**

This means all business logic (error classification, branch evaluation, risk assessment) can be unit-tested without Playwright, without an LLM API key, and without a browser.

### Layout

```
computer-use/
├── main.go                   # CLI flags, dependency wiring
│
├── domain/                   # ← PURE GO. No playwright, no HTTP, no LLM.
│   ├── capability.go         # Capability, Step, Locator, Action, Assertion, Branch
│   ├── run.go                # Run, StepResult, ErrorClass, RunStatus
│   ├── safety.go             # SafetyPolicy, RiskLevel, SafetyVerdict
│   └── errors.go             # BusinessError, LocatorError, PlannerError (sentinels)
│
├── planner/                  # depends on: domain, LLM client
│   ├── planner.go            # Plan(instruction, PageContext) → Capability
│   ├── validator.go          # Validate(Capability, page) → []ValidationError
│   ├── fix.go                # Fix(Capability, []ValidationError) → Capability
│   └── prompts.go            # system prompt templates (no logic)
│
├── scraper/                  # depends on: playwright
│   └── scraper.go            # ExtractContext(page) → PageContext
│
├── safety/                   # depends on: domain, LLM client (for judge)
│   ├── scanner.go            # ScanCapability(cap, pageCtx) → []SafetyVerdict
│   ├── keyword.go            # coarse pre-filter
│   ├── structural.go         # ActionKind risk tiers
│   └── judge.go              # LLM-as-Judge call
│
├── replay/                   # depends on: domain, playwright
│   ├── engine.go             # Run(ctx, Capability, params) → Run
│   ├── dispatch.go           # switch on StepType
│   ├── locator.go            # fallback chain resolution
│   └── assert.go             # Assertion evaluation + BusinessError
│
├── store/                    # depends on: domain
│   └── store.go              # Save/Load Capability and Run as JSON
│
└── api/                      # depends on: all above
    ├── server.go             # HTTP mux, RunManager
    └── handlers.go           # StartRun, GetRun, ResumeRun, GetCapability
```

### Dependency Graph

```
        main
         │
         ├──► api ──────────────────────────┐
         │     │                            │
         ├──► planner ◄── scraper           │
         │     │                            │
         ├──► safety                        │
         │     │                            │
         ├──► replay                        │
         │     │                            │
         └──► store                         │
               │                            │
               └──────► domain ◄────────────┘
                        (no deps)
```

---

## 11. Problems & Solutions

| Problem | Why it's Hard | Our Solution |
|---|---|---|
| Raw HTML overwhelms LLM context | 50k+ tokens per page | Pruned accessibility tree: ~500 tokens, semantically richer |
| Selectors break on UI updates | `#btn-123` changes to `#btn-456` | Locator fallback chain: test-id → role → label → text → css |
| LLM hallucinates selectors | No live DOM access during generation | Self-healing loop: generate → validate against live page → fix |
| Business errors retried as failures | No semantic distinction | `is_business_outcome` flag routes to `business_error` status, never retried |
| LLM produces destructive plans | No guardrails on LLM output | 5-layer safety pipeline: keywords → structural → LLM-judge → allowlist → critical gate |
| Keywords miss context | "proceed to checkout" has no keywords | LLM-as-Judge layer with full page context |
| Keywords over-flag safe steps | "remove filter" ≠ "delete account" | Keywords are a coarse pre-filter only; structural + judge layers refine |
| Destructive actions mid-run | Engine can't predict DOM changes at plan time | Pre-execution page context check before RiskHigh steps |
| No audit trail | Can't investigate failures | `Run` record with per-step screenshots, actual values, error classification |
| Can't reuse a recorded plan | Hardcoded values in steps | `ParamDef` + `{{param_name}}` interpolation — capabilities are parameterized functions |
| Long runs fail partway through | Network blip, session expiry | `checkpoint` steps + `RunStatus: partial` + `/resume` endpoint |
| Fully autonomous system | CAPTCHA, price surprises, policy concerns | `human_escalate` error policy + `RequiresApproval` flag + `/resume` endpoint |
| Can't test without a browser | Playwright required for all tests | `domain` package has zero deps — all core logic unit-testable in isolation |

---

## 12. Design Decisions Summary

### Decision 1: Capability-first, not script-first
We emit a structured `Capability` JSON, not a Playwright script. This makes the plan auditable, versionable, parameterizable, and executable by any future engine — not just Playwright.

### Decision 2: `is_business_outcome` as the error taxonomy pivot
A single boolean on `Assertion` is all that's needed to separate clean business stops from technical failures. This simplicity is intentional — it maps directly to what a human would say: "did we fail, or did we hit an expected wall?"

### Decision 3: Locator fallback chain, not single selector
Resilience to UI changes is the primary operational concern of any automation system. The fallback chain means a Playwright update or a CSS refactor silently degrades to the next strategy rather than breaking the capability.

### Decision 4: Tiered safety pipeline
Starting with keyword pre-filtering and escalating to LLM-as-Judge only for suspicious steps keeps latency and cost low for the 95% of steps that are obviously safe, while providing context-aware protection for the 5% that matter.

### Decision 5: `domain` package with zero external dependencies
Core business logic — error classification, risk assessment, branch evaluation — is framework-agnostic. This enables fast unit tests, easy refactoring, and future portability to a different browser automation library.

### Decision 6: Async API with `202 Accepted`
Runs are long-lived (10s – 2min). A synchronous HTTP response would require long-polling or SSE at the HTTP layer. A `202 + poll` pattern is the simplest correct model for long async work.
