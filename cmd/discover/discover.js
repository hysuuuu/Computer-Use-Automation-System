#!/usr/bin/env node
/**
 * cmd/discover/discover.js
 *
 * Interactive LLM-driven discovery agent.
 *
 * Usage:
 *   node cmd/discover/discover.js --url <target-url> --goal "<natural language goal>"
 *
 * How it works:
 *   1. Opens a real Chromium browser via Playwright.
 *   2. Navigates to the target URL.
 *   3. Extracts the live accessibility tree + current URL.
 *   4. Prints a structured observation block to stdout.
 *   5. Waits for the LLM operator to type a JSON action on stdin.
 *   6. Executes the action against the live page.
 *   7. Repeats from step 3 until action is {"action": "done", "capability": {...}}.
 *   8. Saves the final Capability JSON → evidence/cap_<id>.json
 *   9. Saves the full session transcript → evidence/discovery_transcript.md
 *
 * Supported actions (LLM types these as single-line JSON on stdin):
 *
 *   {"action": "navigate",   "url": "https://..."}
 *   {"action": "click",      "selector": "[data-test='login-button']"}
 *   {"action": "fill",       "selector": "[data-test='username']", "value": "standard_user"}
 *   {"action": "select",     "selector": "select#foo",             "value": "option-value"}
 *   {"action": "key_press",  "selector": "[data-test='search']",   "key": "Enter"}
 *   {"action": "screenshot", "path": "evidence/<name>.png"}
 *   {"action": "wait_text",  "text": "Inventory"}
 *   {"action": "observe"}    -- re-dump the accessibility tree without acting
 *   {"action": "done", "capability": { ... full Capability JSON ... }}
 */

const { chromium } = require('playwright');
const readline = require('readline');
const fs = require('fs');
const path = require('path');

// ── CLI args ──────────────────────────────────────────────────────────────────
const args = process.argv.slice(2);
const urlIdx  = args.indexOf('--url');
const goalIdx = args.indexOf('--goal');
const headedIdx = args.indexOf('--headed');

const TARGET_URL = urlIdx  >= 0 ? args[urlIdx + 1]  : 'https://www.saucedemo.com';
const GOAL       = goalIdx >= 0 ? args[goalIdx + 1] : 'Complete a checkout on saucedemo';
const HEADED     = headedIdx >= 0; // pass --headed to watch the browser live

const EVIDENCE_DIR = path.join(__dirname, '..', '..', 'evidence');
fs.mkdirSync(EVIDENCE_DIR, { recursive: true });

// ── Session transcript ────────────────────────────────────────────────────────
let transcript = [];
let stepCount  = 0;

function logStep(role, content) {
  stepCount++;
  const entry = { step: stepCount, role, content, ts: new Date().toISOString() };
  transcript.push(entry);
  return entry;
}

// ── Accessibility tree extraction ─────────────────────────────────────────────
/**
 * Returns a pruned, flat list of interactive elements from the page's
 * accessibility tree. This is what we show the LLM instead of raw HTML.
 */
async function extractA11yTree(page) {
  const snapshot = await page.accessibility.snapshot({ interestingOnly: true });
  const elements = [];

  function walk(node, depth) {
    if (!node) return;
    const interactiveRoles = [
      'button','link','textbox','checkbox','radio','combobox',
      'listbox','option','menuitem','tab','searchbox','spinbutton',
      'slider','switch','treeitem','gridcell',
    ];
    const isInteractive = interactiveRoles.includes(node.role) || node.focusable;
    if (isInteractive && node.name) {
      elements.push({
        role:     node.role,
        name:     node.name,
        value:    node.value,
        disabled: node.disabled,
        checked:  node.checked,
        // test-id is not in the a11y snapshot; we need to query for it separately
      });
    }
    if (node.children) node.children.forEach(c => walk(c, depth + 1));
  }
  walk(snapshot, 0);

  // Also grab test-ids for interactive elements (huge value for stable selectors)
  const testIds = await page.evaluate(() => {
    const els = document.querySelectorAll('[data-test],[data-testid],[data-test-id]');
    return Array.from(els).map(el => ({
      tag:    el.tagName.toLowerCase(),
      testId: el.getAttribute('data-test') || el.getAttribute('data-testid') || el.getAttribute('data-test-id'),
      type:   el.getAttribute('type'),
      text:   el.textContent.trim().slice(0, 60),
    }));
  });

  return { elements, testIds };
}

// ── Observation block printer ─────────────────────────────────────────────────
async function printObservation(page, label) {
  const url    = page.url();
  const title  = await page.title();
  const { elements, testIds } = await extractA11yTree(page);

  const block = [
    '',
    '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━',
    `📸  OBSERVATION${label ? ' — ' + label : ''}`,
    `    URL:   ${url}`,
    `    Title: ${title}`,
    '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━',
    '',
    '── Accessibility Tree (interactive elements) ──',
  ];

  elements.forEach(el => {
    let line = `  [${el.role}] "${el.name}"`;
    if (el.value)    line += ` value="${el.value}"`;
    if (el.disabled) line += ` (disabled)`;
    if (el.checked)  line += ` (checked)`;
    block.push(line);
  });

  if (testIds.length > 0) {
    block.push('');
    block.push('── data-test IDs on page ──');
    testIds.forEach(t => {
      block.push(`  <${t.tag}${t.type ? ` type="${t.type}"` : ''}> data-test="${t.testId}"  text: "${t.text}"`);
    });
  }

  block.push('');
  block.push('🎯  GOAL: ' + GOAL);
  block.push('');
  block.push('Type your next action as JSON and press Enter:');
  block.push('  e.g. {"action":"click","selector":"[data-test=\'login-button\']"}');
  block.push('  Type {"action":"done","capability":{...}} when finished.');
  block.push('');

  const text = block.join('\n');
  console.log(text);
  logStep('observation', { url, title, elements, testIds });
  return text;
}

// ── Action executor ───────────────────────────────────────────────────────────
async function executeAction(page, actionObj) {
  const { action } = actionObj;

  switch (action) {
    case 'navigate':
      await page.goto(actionObj.url, { waitUntil: 'networkidle' });
      console.log(`  ✅ Navigated to ${actionObj.url}`);
      break;

    case 'click':
      await page.click(actionObj.selector, { timeout: 10000 });
      await page.waitForLoadState('networkidle').catch(() => {});
      console.log(`  ✅ Clicked ${actionObj.selector}`);
      break;

    case 'fill':
      await page.fill(actionObj.selector, actionObj.value, { timeout: 5000 });
      console.log(`  ✅ Filled ${actionObj.selector} with "${actionObj.value}"`);
      break;

    case 'select':
      await page.selectOption(actionObj.selector, actionObj.value);
      console.log(`  ✅ Selected ${actionObj.value} in ${actionObj.selector}`);
      break;

    case 'key_press':
      if (actionObj.selector) await page.focus(actionObj.selector);
      await page.keyboard.press(actionObj.key);
      console.log(`  ✅ Key press: ${actionObj.key}`);
      break;

    case 'screenshot': {
      const screenshotPath = path.isAbsolute(actionObj.path)
        ? actionObj.path
        : path.join(__dirname, '..', '..', actionObj.path);
      fs.mkdirSync(path.dirname(screenshotPath), { recursive: true });
      await page.screenshot({ path: screenshotPath, fullPage: true });
      console.log(`  ✅ Screenshot saved → ${screenshotPath}`);
      break;
    }

    case 'wait_text':
      await page.waitForSelector(`text="${actionObj.text}"`, { timeout: 10000 });
      console.log(`  ✅ Text visible: "${actionObj.text}"`);
      break;

    case 'observe':
      // no-op — will re-observe after this
      break;

    default:
      console.warn(`  ⚠️  Unknown action: ${action}`);
  }
}

// ── Save outputs ──────────────────────────────────────────────────────────────
function saveCapability(capability) {
  const capId = capability.id || 'cap_discovered';
  const capPath = path.join(EVIDENCE_DIR, `${capId}.json`);
  fs.writeFileSync(capPath, JSON.stringify(capability, null, 2));
  console.log(`\n  ✅ Capability saved → ${capPath}`);
  return capPath;
}

function saveTranscript(goal, capPath) {
  const mdLines = [
    '# Discovery Run Transcript',
    '',
    `**Goal:** ${goal}`,
    `**Date:** ${new Date().toISOString()}`,
    `**Target:** ${TARGET_URL}`,
    `**Method:** Interactive LLM-driven agent loop (AGY CLI acting as LLM)`,
    `**Output Capability:** ${capPath ? path.basename(capPath) : 'none'}`,
    '',
    '---',
    '',
  ];

  transcript.forEach(entry => {
    if (entry.role === 'observation') {
      mdLines.push(`## Step ${entry.step} — Observation`);
      mdLines.push(`**URL:** \`${entry.content.url}\``);
      mdLines.push(`**Title:** ${entry.content.title}`);
      mdLines.push('');
      if (entry.content.testIds.length > 0) {
        mdLines.push('**data-test IDs found:**');
        entry.content.testIds.forEach(t => {
          mdLines.push(`- \`data-test="${t.testId}"\` — ${t.text}`);
        });
      }
      mdLines.push('');
    } else if (entry.role === 'action') {
      mdLines.push(`## Step ${entry.step} — Action`);
      mdLines.push('```json');
      mdLines.push(JSON.stringify(entry.content, null, 2));
      mdLines.push('```');
      mdLines.push('');
    } else if (entry.role === 'decision') {
      mdLines.push(`> **LLM Reasoning:** ${entry.content}`);
      mdLines.push('');
    }
  });

  const mdPath = path.join(EVIDENCE_DIR, 'discovery_transcript.md');
  fs.writeFileSync(mdPath, mdLines.join('\n'));
  console.log(`  ✅ Transcript saved → ${mdPath}`);
}

// ── Main loop ─────────────────────────────────────────────────────────────────
async function main() {
  console.log('\n🤖 Computer-Use Discovery Agent');
  console.log('================================');
  console.log(`Goal:   ${GOAL}`);
  console.log(`Target: ${TARGET_URL}`);
  console.log(`Mode:   ${HEADED ? 'headed (visible browser)' : 'headless'}`);
  console.log('');
  console.log('Starting browser...\n');

  const browser = await chromium.launch({ headless: !HEADED, slowMo: 100 });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page    = await context.newPage();

  // Navigate to the initial target
  await page.goto(TARGET_URL, { waitUntil: 'networkidle' });

  // Set up readline for stdin
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout, terminal: false });

  let capPath = null;
  let done    = false;

  // Initial observation
  await printObservation(page, 'Initial page');

  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;

    // Try to parse the LLM's JSON action
    let actionObj;
    try {
      actionObj = JSON.parse(trimmed);
    } catch (e) {
      // Maybe the LLM included a reasoning comment before the JSON — extract it
      const jsonStart = trimmed.indexOf('{');
      if (jsonStart >= 0) {
        const reasoning = trimmed.slice(0, jsonStart).trim();
        if (reasoning) {
          console.log(`  💭 Reasoning: ${reasoning}`);
          logStep('decision', reasoning);
        }
        try {
          actionObj = JSON.parse(trimmed.slice(jsonStart));
        } catch {
          console.error('  ❌ Could not parse JSON. Try again.');
          continue;
        }
      } else {
        // Treat the whole line as reasoning/comment, don't act
        console.log(`  💭 Noted: ${trimmed}`);
        logStep('decision', trimmed);
        continue;
      }
    }

    logStep('action', actionObj);

    // Handle "done" — save and exit
    if (actionObj.action === 'done') {
      if (!actionObj.capability) {
        console.error('  ❌ "done" action requires a "capability" field with the full Capability JSON.');
        continue;
      }
      capPath = saveCapability(actionObj.capability);
      done = true;
      break;
    }

    // Execute the action
    try {
      await executeAction(page, actionObj);
    } catch (err) {
      console.error(`  ❌ Action failed: ${err.message}`);
      console.log('  Showing current page state so you can retry:\n');
    }

    // Re-observe after every action (except screenshot which doesn't change page)
    if (actionObj.action !== 'screenshot') {
      await printObservation(page, `After ${actionObj.action}`);
    }
  }

  // Save transcript regardless of whether we completed
  saveTranscript(GOAL, capPath);

  if (!done) {
    console.log('\n⚠️  Session ended without completing the goal. Transcript saved.');
  } else {
    console.log('\n✅ Discovery complete!');
    console.log(`   Capability: ${capPath}`);
    console.log(`   Transcript: ${path.join(EVIDENCE_DIR, 'discovery_transcript.md')}`);
  }

  await browser.close();
  process.exit(0);
}

main().catch(err => {
  console.error('Fatal error:', err);
  process.exit(1);
});
