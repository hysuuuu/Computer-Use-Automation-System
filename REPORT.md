# REPORT: Computer-Use Automation System

## 1. Architecture

The system splits browser automation into two phases that never overlap: a discovery phase driven by an LLM, and a replay phase that runs without one.

During discovery, a planning agent (`planner/`) receives a natural language goal and a live accessibility tree snapshot from the page. It calls an LLM to generate a `Capability` artifact, a structured JSON document that records every step needed to complete the goal. Once the artifact is saved, the LLM is done. Replay never calls it again.

During replay, the engine (`replay/`) reads the saved `Capability`, resolves each step against the live browser, and writes an audit log (`Run`) on the way out. The engine has no generative component; it is a strict interpreter of a fixed instruction set.

The package dependency graph enforces this separation:

```
domain        (zero external deps)
  ↑
replay   store   stub
  ↑
api   cmd/replay   cmd/discover
```

`domain` owns all types. Every other package imports it but not each other. The `replay.Browser` interface is the only seam between the engine and Playwright. The stub browser (`stub/`) satisfies that interface with logged no-ops, so the full engine test suite runs in under 10ms with no browser installed.

The HTTP server (`api/`) is asynchronous. `POST /runs` returns `202 Accepted` immediately and launches a goroutine. Clients poll `GET /runs/:id`. When a step requires human approval, the goroutine parks on a channel and the run status becomes `escalated`. `POST /runs/:id/resume` sends to that channel to unblock it.

The trade-off I made is simplicity over scale. A goroutine-per-run model breaks down past a few hundred concurrent runs. The right fix is a job queue and worker pool, but that is infrastructure the spec explicitly asks us not to build yet. The `RunManager` interface is already the right abstraction to swap behind later.

## 2. Artifact schema

A `Capability` is the contract between the planner and the engine. Its schema reflects three design goals.

**Parameterization.** Steps use `{{username}}` interpolation so one recorded workflow can run for thousands of tenants without re-planning. Parameters carry a `type` field (`string`, `secret`, `number`), and the engine redacts any secret-typed parameter from the audit log before writing it to disk.

**Locator resilience.** Every step that touches the DOM carries a `Locator` with a `Primary` strategy and an ordered `Fallbacks` list. The priority order is `test-id > role > label > text > css > xpath`, from most to least stable. If the primary strategy fails, the engine tries each fallback in sequence before failing the step. This means a layout change or a CSS class rename does not necessarily break the automation.

**Branch steps.** A `Branch` step evaluates an assertion and jumps to a different step ID depending on the result. This lets one capability handle multiple expected page states (a CAPTCHA screen, a "session expired" redirect) without re-invoking the LLM.

The schema does not try to model every possible browser interaction. Steps have six types: `action`, `assert`, `branch`, `extract`, `wait`, and `checkpoint`. That is enough to cover real workflows without the engine becoming a general scripting language.

## 3. Determinism and error handling

Replay is deterministic in the sense that the same capability and the same parameters always produce the same sequence of browser interactions. The engine does not make decisions; it interprets the step list linearly and jumps on branch steps.

The error taxonomy separates three cases that a lot of automation systems conflate:

A `BusinessError` means an assertion with `is_business_outcome: true` failed. This is not a crash. "Wrong credentials" is a legitimate answer the calling agent needs to receive cleanly. The engine stops, marks the run `business_error`, and returns. No retry happens because retrying a business outcome is almost always wrong.

A hard failure means something unexpected went wrong (a locator not found after all fallbacks, a timeout, a navigation error). The engine applies the step's `on_error` policy, which can be `fail_fast`, `retry` with a configurable delay and max count, or `skip`.

An escalation is a human gate. A step marked `requires_approval: true` or with `on_error.strategy: human_escalate` parks the run and surfaces a resume URL. The human can approve by calling `POST /runs/:id/resume`, or abort.

For UI drift specifically, the fallback locator chain handles minor changes without any re-planning. A `test-id` attribute is the most stable identifier, but when a legacy app does not have them at all (which is the norm in financial software), the engine walks down to ARIA role plus accessible name, then text content, then CSS. If the page is restructured enough that all fallbacks fail, the engine escalates to a human rather than guessing.

## 4. Heterogeneity and multi-tenant reuse

The capability artifact is already parameterized, so the same file runs across multiple tenants running the same application without modification. The caller supplies a different `username` and `password` at runtime. Tenant-specific variations (a custom logo on the login page, a different URL path prefix) would require only a separate capability file, not a code change.

For genuinely different surface types, the `replay.Browser` interface is the extension point. The current implementation backs it with a stub. A Playwright-backed browser is the obvious next step for web. For a desktop application, the same interface could be backed by a library like `robotgo` or Windows UI Automation, as long as it satisfies the same methods (`Click`, `Fill`, `TextVisible`, etc.). The engine and all its logic would remain unchanged.

The planner's `PageContext` type (an accessibility tree, not raw HTML) is deliberate here. Accessibility trees are available on desktop operating systems through OS-level APIs, so the discovery side can generalize the same way.

## 5. Escalation and handoff

The system detects "stuck" in two ways. First, any step whose `risk` meets or exceeds `SafetyPolicy.RequireApprovalAt` is automatically flagged `requires_approval: true` by the planner's safety pipeline. Second, a step can declare `on_error.strategy: human_escalate` explicitly, which fires when the step fails rather than before it runs.

When escalation triggers, the `RunManager` stores the `EscalationError` (including the step ID and message) in the run state, changes the status to `escalated`, and blocks the goroutine on a channel. The `GET /runs/:id` response includes the escalation context so the operator knows which step is waiting and why.

A human operator calls `POST /runs/:id/resume` with a `resolution` field (`approved` or `abort`). If approved, the blocked goroutine receives the signal and continues from the next step. If aborted, the run terminates as a hard failure.

The current handoff is API-only. A full operator console with a live browser view is out of scope (noted in the cuts section), but the API surface is real and functional.

## 6. Safety

The planner runs every generated capability through a four-tier pipeline before returning it.

Tier 1 is a keyword scan against a list of destructive terms ("delete", "transfer", "withdraw", "purge", and others). This runs before any structural analysis, costs nothing, and catches the most obvious dangerous plans immediately.

Tier 2 checks every `navigate` action and the target URL against the capability's `allowed_domains` list. A plan that tries to navigate outside the allowlist is rejected with a `SafetyError`.

Tier 3 enforces the `require_approval_at` risk ceiling. Any step whose `risk` field meets or exceeds the threshold has `requires_approval` set to `true` and its error policy upgraded to `human_escalate`. This is a mutation, not a rejection: the plan still runs, but a human must approve the high-risk steps before they execute.

Tier 4 sends a short review prompt to the LLM for any plan that contains high-risk steps. The LLM responds `APPROVE` or `REJECT: <reason>`. This tier fails open: if the LLM is unavailable, the pipeline continues rather than blocking legitimate work. That is a deliberate trade-off. The first three tiers handle the structural cases; the LLM judge adds contextual review but should not be the last line of defense.

The `SafetyPolicy` is operator-controlled. The planner injects it verbatim from the request and cannot lower the ceilings the operator set.

## 7. Cuts

**Real browser integration.** The `replay.Browser` interface is implemented by a stub that logs every call and returns success. The swap point for a real Playwright-backed browser is one file (`stub/browser.go`), but wiring up playwright-go driver installation reliably across environments takes time. It is the clearest next step.

**Live session takeover.** The spec mentions a human taking over the live browser session directly, not just approving via API. This would require streaming the browser viewport to the operator and forwarding mouse/keyboard events back. The escalation API is functional; the console is not built.

**Operator UI.** There is no web interface for browsing capabilities, inspecting run history, or managing approvals. The API supports all of it; a frontend does not exist yet.

**Planner self-healing loop.** The current planner calls the LLM once and either succeeds or fails. A production planner would run an observe-decide-act loop until the goal is confirmed complete, with a step budget to prevent runaway execution.

**Confidence scoring and approval gates.** Capabilities have no stability score. A capability that has replayed successfully 50 times should have a different approval status than one that has never been tested. The schema has a `version` field that could support this; the logic does not exist yet.
