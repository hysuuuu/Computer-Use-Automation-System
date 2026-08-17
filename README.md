# Computer-Use Automation System

A backend integration layer that lets an LLM discover how to operate a web application once, records that discovery as a versioned `Capability` artifact, and then replays the artifact deterministically without calling the LLM again.

The target application for the demo is [saucedemo.com](https://www.saucedemo.com), a public sandbox that simulates a retail storefront. The full demo runs a multi-step checkout flow, including a bad-credential scenario that exercises the business error path.

---

## How it works

**Discovery:** A planning agent opens a real browser, reads the accessibility tree at each step, and decides what to do next based on a natural language goal. The output is a `Capability` JSON file that describes every step, locator, and parameter needed to repeat the workflow. After this, the LLM is out of the picture.

**Replay:** The Go engine reads the saved `Capability`, executes each step against a browser, and writes a full audit log. Secret parameters (passwords) are redacted before the log is written to disk. High-risk steps pause execution and wait for a human to approve via API before continuing.

---

## Setup

**Requirements:**

- Go 1.23+
- Node.js 18+ and npm (for the discovery agent)
- Internet access to reach saucedemo.com

```bash
git clone <repo-url>
cd interface.ai

# Install Node dependencies (Playwright)
npm install
npx playwright install chromium

# Verify Go setup
export GO111MODULE=on
go test ./...
```

All 19 tests should pass in under a second. No API keys are needed to run the tests or the replay runner.

---

## Running the API Server

The API server can operate in two modes: **Stub Mode** (default) and **Real LLM Mode**.

### 1. Real LLM Mode (OpenAI / Gemini / Anthropic)

If you provide an `OPENAI_API_KEY`, the server will wire up the real `LLMClient` to dynamically generate capabilities using the OpenAI Chat Completions API.

```bash
export OPENAI_API_KEY="sk-..."
export OPENAI_MODEL="gpt-4o"  # optional, defaults to gpt-4o
export GO111MODULE=on
go run . --addr :8080 --data-dir ./data
```

_(Note: You can override the base URL via `OPENAI_BASE_URL` to use drop-in replacements like Groq or vLLM)._

### 2. Stub Mode (No LLM required)

If no API key is provided, the server falls back to a mock `stubLLMClient` that safely returns a pre-canned capability template. This lets you test the API surface locally without external dependencies.

```bash
export GO111MODULE=on
go run . --addr :8080 --data-dir ./data
```

Run a capability via the API:

```bash
# Start a run
curl -s -X POST http://localhost:8080/runs \
  -H 'Content-Type: application/json' \
  -d '{
    "capability_id": "cap_saucedemo_login_v1",
    "params": {"username": "standard_user", "password": "secret_sauce"}
  }' | jq .

# Poll status (replace with your run_id)
curl -s http://localhost:8080/runs/<run_id> | jq .
```

The server seeds a login capability on startup, so you can run this immediately after starting.

---

## Demo path: discovery then replay

This is the exact sequence to run the agent on a real goal and replay the resulting artifact.

### Step 1: discovery run

The discovery agent opens a real Chromium browser and prints the live accessibility tree after each action. You (or an LLM operator) type JSON commands on stdin to drive the session. When finished, type `{"action":"done","capability":{...}}` to save the artifact.

```bash
node cmd/discover/discover.js \
  --url https://www.saucedemo.com \
  --goal "Add Sauce Labs Backpack to cart and complete checkout" \
  --headed
```

The `--headed` flag makes the browser window visible so you can see what is happening. The script prints the accessibility tree at each step and waits for your input. A sample session looks like this:

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
OBSERVATION — Initial page
URL:   https://www.saucedemo.com
Title: Swag Labs
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

── data-test IDs on page ──
  <input type="text">   data-test="username"   text: ""
  <input type="password"> data-test="password"  text: ""
  <input type="submit">   data-test="login-button" text: "Login"

Type your next action as JSON and press Enter:
```

You respond with:

```json
{
  "action": "fill",
  "selector": "[data-test='username']",
  "value": "standard_user"
}
```

Continue until the goal is complete, then emit the final `done` action with the capability JSON. The script writes the artifact to `evidence/<capability-id>.json` and the full session transcript to `evidence/discovery_transcript.md`.

A pre-recorded capability for the checkout flow is already in `evidence/cap_checkout.json` if you want to skip straight to replay.

### Step 2: replay the artifact

```bash
export GO111MODULE=on
go build -o bin/replay-runner ./cmd/replay

# Successful run (pauses at the high-risk "Finish" step for human approval)
./bin/replay-runner \
  --capability evidence/cap_checkout.json \
  --params '{"username":"standard_user","password":"secret_sauce","item_name":"sauce-labs-backpack","first_name":"John","last_name":"Doe","zip":"12345"}' \
  --out evidence/replay_success.json

# Business error run (wrong password triggers the branch and halts cleanly)
./bin/replay-runner \
  --capability evidence/cap_checkout.json \
  --params '{"username":"standard_user","password":"wrong_password","item_name":"sauce-labs-backpack","first_name":"John","last_name":"Doe","zip":"12345"}' \
  --out evidence/replay_business_error.json
```

Exit codes: `0` = success, `2` = business error or escalation, `1` = hard failure.

The output JSON files in `evidence/` show the full step-by-step audit log for each run. The successful run ends with `status: "escalated"` because the "Finish Purchase" step is marked high-risk and requires human approval before the engine will click it. The business error run ends with `status: "business_error"` after the bad-password branch fires.

---

## Project layout

```
domain/        Core types (Capability, Step, Run). Zero external dependencies.
replay/        Deterministic execution engine. Depends only on domain.
planner/       LLM planning layer and four-tier safety pipeline.
api/           HTTP server (POST /runs, GET /runs/:id, POST /runs/:id/resume).
store/         JSON file persistence for capabilities and run logs.
stub/          No-op browser implementation used in tests and default server mode.
cmd/discover/  Node.js interactive discovery agent (Playwright + stdin).
cmd/replay/    Go CLI for running a capability and writing an audit log.
evidence/      Saved capability, discovery transcript, and replay audit logs.
```
