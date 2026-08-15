# Discovery Run Transcript

**Goal:** Log into saucedemo.com, add a specific item to the cart (parameterized), and reach the final checkout confirmation.
**Date:** August 15, 2026
**Agent:** Antigravity (LLM Planner)
**Method:** Manual observation of Playwright accessibility tree via local script (`discovery.js`).

## Step 1: Navigate & Observe
- **Action:** `page.goto('https://www.saucedemo.com')`
- **Observation:** Extracted Accessibility Tree (see `evidence/discovery_logs.txt`).
- **Decision:** The page contains a login form. I see elements with `test-id="username"`, `test-id="password"`, and `test-id="login-button"`. I will map these to parameters `{{username}}` and `{{password}}`.

## Step 2: Login & Observe
- **Action:** Fill username/password, click login button.
- **Observation:** Landed on inventory page (`step2_inventory.png`).
- **Decision:** The inventory page contains multiple items. The goal is to add a specific item. I noticed the "Add to cart" buttons use `test-id` attributes like `add-to-cart-sauce-labs-backpack`. I will parameterize this as `add-to-cart-{{item_name}}`. Next, I see the cart link at the top right (class `.shopping_cart_link`). I will instruct the replay engine to click the add-to-cart button, then the cart link.

## Step 3: Cart Page & Observe
- **Action:** Clicked Add to Cart, then clicked `.shopping_cart_link`.
- **Observation:** Landed on the cart page (`step3_cart.png`).
- **Decision:** I see the checkout button has `test-id="checkout"`. The replay capability should click this.

## Step 4: Checkout Info & Observe
- **Action:** Clicked Checkout.
- **Observation:** Reached the "Checkout: Your Information" page (`step4_checkout_review.png`).
- **Decision:** The form has `test-id` fields for `firstName`, `lastName`, and `postalCode`. I will parameterize these. The continue button has `test-id="continue"`. 

## Step 5: Checkout Review & Safety Guardrail
- **Action:** Clicked Continue.
- **Observation:** Reached the "Checkout: Overview" page with a "Finish" button (`test-id="finish"`).
- **Decision:** Clicking Finish finalizes a purchase. According to the Safety Policy, this is a **HIGH RISK** action that mutates external state. In the `cap_checkout.json`, I will mark this step with `risk: high` and `requires_approval: true`, ensuring the replay engine halts and escalates to a human before execution.

## Step 6: Error Handling Discovery (Business Error)
- **Action:** Ran a parallel test with a wrong password.
- **Observation:** Reached the login page with an error message: "Epic sadface: Username and password do not match any user in this service" (`error_login.png`).
- **Decision:** This is a legitimate business outcome, not a technical crash. I will add a `branch` step in the capability to check for this text. If it appears, the capability will branch to an `assert` step marked with `is_business_outcome: true`, signaling to the calling AI agent that the credentials were bad.

## Output
The observations were compiled into the final parameterized artifact: `cap_checkout.json`.
