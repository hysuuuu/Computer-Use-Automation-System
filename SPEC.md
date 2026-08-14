# SPEC: Computer-Use Automation System

## Problem Statement

Financial institutions rely heavily on legacy back-office applications that lack APIs. AI agents need to operate these applications to accomplish tasks, but traditional browser automation is brittle (breaks on minor UI changes). On the other hand, fully autonomous LLM-driven execution is slow, non-deterministic, and prone to hallucinating destructive actions (like accidentally executing a $10,000 transfer). The company needs a reliable way to turn an LLM's initial "discovery" of a workflow into a safe, fast, and deterministic capability.

## Solution

A backend integration layer that uses an LLM to discover how to accomplish a task *once*, and then records that discovery as a structured, versioned, reusable `Capability` artifact. This artifact can then be deterministically replayed without the LLM in the decision loop. 

The system includes a resilient locator fallback chain to survive UI drift, a robust error taxonomy to distinguish business errors from technical failures, and a multi-layered safety pipeline with human-in-the-loop escalation to gate critical actions.

## User Stories

1. As an AI Agent, I want to submit a natural language goal and target URL, so that the system can generate a reusable automation capability.
2. As a Replay Engine, I want to execute a capability using a resilient locator fallback chain (`test-id` > `role` > `css`), so that minor UI changes don't break the automation.
3. As a Replay Engine, I want to evaluate conditional branches during replay, so that I can handle multiple expected page states (e.g., login vs CAPTCHA) without re-invoking the LLM.
4. As a Replay Engine, I want to distinguish between expected business outcomes and technical failures using the `is_business_outcome` flag, so that I don't infinitely retry legitimate errors (e.g., "User not found").
5. As a Safety Auditor, I want the system to scan planned actions for structural risk and keyword flags, so that destructive actions are paused before execution.
6. As a Safety Auditor, I want the system to enforce a capability-level allowlist (e.g., max monetary value, allowed domains), so that the agent cannot operate out of bounds.
7. As a Human Operator, I want the system to pause execution and request approval for critical actions, so that I can prevent unintended consequences.
8. As a Human Operator, I want to view a screenshot of the live session when an escalation occurs, so that I understand the context before approving or aborting.
9. As a Human Operator, I want to manually complete a blocked step in the live session and resume the automation via API, so that the run can succeed despite edge cases.
10. As a System Administrator, I want the execution run to emit a detailed audit log with screenshots and actual parameters used, so that I can debug failures.
11. As a System Administrator, I want sensitive parameters (e.g., passwords) to be automatically redacted from the audit log, so that PII is not leaked.

## Implementation Decisions

- **Architecture:** Clean Architecture. The `domain` package will have zero external dependencies, defining the core types (`Capability`, `Step`, `Locator`, `RunStatus`). The outer packages (`replay`, `planner`, `api`) will depend on the domain.
- **Capability Schema:** Defines the automation contract. Features parameter interpolation (`{{username}}`) to allow multi-tenant reuse.
- **Safety Pipeline:** A tiered approach: 
  1. Keyword pre-filter (fast/cheap). 
  2. Structural ActionKind check (e.g., `extract` is always low risk). 
  3. LLM-as-Judge for contextual risk. 
  4. Human Critical Gate.
- **Planner:** Uses a self-healing loop. It provides the LLM with a pruned accessibility tree rather than raw HTML to prevent context explosion and hallucinatory CSS selectors.
- **API Contracts:** Async API model. `POST /runs` returns `202 Accepted` immediately. Clients poll `GET /runs/:id` for status (`running`, `success`, `business_error`, `hard_failure`, `escalated`). `POST /runs/:id/resume` handles human intervention.
- **Secret Redaction:** Parameters marked with `type: "secret"` in the `ParamDef` will be explicitly redacted by the replay engine before the `Run` object is written to disk or returned via API.

## Testing Decisions

Good tests for this system will verify the behavior of the replay engine, error taxonomy, and safety pipeline without relying on a real browser wherever possible.

- **Seam 1: Domain / Replay Engine (Unit).** We will test the engine's dispatch loop, branch evaluation, and error policy application by mocking the browser interaction layer (`playwright.Page`). This is the highest internal seam and allows us to validate determinism and error taxonomy (e.g., ensuring `is_business_outcome` halts execution cleanly) extremely quickly.
- **Seam 2: Planner (Integration).** We will test the planner by providing static `PageContext` fixtures (fake accessibility trees) and verifying it generates a `Capability` that prioritizes `test-id` locators and applies the correct `RiskLevel`.
- **Seam 3: End-to-End API (E2E).** We will test the `POST /runs` and `POST /runs/:id/resume` endpoints against a local mock HTML server (with intentionally hostile legacy markup) to verify the full vertical slice (LLM stub -> Engine -> Playwright -> API). 

## Out of Scope

- A full real-time co-browsing operator console (a simple `/resume` API endpoint accepting a JSON payload is sufficient for the human-in-the-loop requirement).
- Desktop application automation (the design abstracts the surface, but the implementation explicitly targets a web app).
- Distributed multi-tenant execution infrastructure (e.g., Kafka queues, Kubernetes clusters). A local async go-routine manager is sufficient.

## Further Notes

- The target application for the E2E demo will be a local mock server or `saucedemo.com` to demonstrate a multi-step checkout flow with business errors.
- The `SPEC.md` format aligns with the final `REPORT.md` requirements for the assessment.
