#!/usr/bin/env node
/**
 * cmd/replay/playwright_bridge.js
 *
 * A thin stdin/stdout JSON bridge that lets the Go replay engine
 * control a real Playwright browser.
 *
 * Protocol:
 *   Go sends one JSON command per line on stdin.
 *   This script executes the command and writes one JSON result per line on stdout.
 *
 * Command format:  { "id": 1, "method": "navigate", "args": {...} }
 * Result format:   { "id": 1, "ok": true, "value": "..." }
 *                  { "id": 1, "ok": false, "error": "..." }
 *
 * Supported methods:
 *   navigate   { url }
 *   click      { selector }
 *   fill       { selector, value }
 *   select     { selector, value }
 *   check      { selector, checked }
 *   keypress   { selector, key }
 *   textvisible { text }  → value: bool
 *   urlcontains { substring } → value: bool
 *   elementexists { selector } → value: bool
 *   gettext    { selector }  → value: string
 *   screenshot { path }
 *   close      {}
 */

const { chromium } = require('playwright');
const readline = require('readline');

const CHROMIUM_PATH = process.env.CHROMIUM_PATH || '';
const HEADLESS      = process.env.HEADLESS !== 'false';

let browser, page;

async function init() {
  const launchOpts = { headless: HEADLESS, slowMo: 50 };
  if (CHROMIUM_PATH) launchOpts.executablePath = CHROMIUM_PATH;
  browser = await chromium.launch(launchOpts);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  page = await ctx.newPage();
}

async function handle(cmd) {
  const { method, args } = cmd;
  switch (method) {
    case 'navigate':
      await page.goto(args.url, { waitUntil: 'networkidle' });
      return {};
    case 'click':
      await page.locator(args.selector).click({ timeout: 10000 });
      await page.waitForLoadState('networkidle').catch(() => {});
      return {};
    case 'fill':
      await page.locator(args.selector).fill(args.value, { timeout: 5000 });
      return {};
    case 'select':
      await page.locator(args.selector).selectOption(args.value);
      return {};
    case 'check':
      if (args.checked) await page.locator(args.selector).check();
      else              await page.locator(args.selector).uncheck();
      return {};
    case 'keypress':
      if (args.selector) await page.locator(args.selector).focus();
      await page.keyboard.press(args.key);
      return {};
    case 'textvisible': {
      const count = await page.locator(`text=${args.text}`).count();
      return { value: count > 0 };
    }
    case 'urlcontains':
      return { value: page.url().includes(args.substring) };
    case 'elementexists': {
      const count = await page.locator(args.selector).count();
      return { value: count > 0 };
    }
    case 'gettext': {
      const text = await page.locator(args.selector).textContent({ timeout: 5000 });
      return { value: text || '' };
    }
    case 'screenshot':
      await page.screenshot({ path: args.path, fullPage: true });
      return { value: args.path };
    case 'close':
      await browser.close();
      process.exit(0);
    default:
      throw new Error(`Unknown method: ${method}`);
  }
}

async function main() {
  await init();

  const rl = readline.createInterface({ input: process.stdin, terminal: false });
  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let cmd;
    try { cmd = JSON.parse(trimmed); } catch { continue; }

    try {
      const result = await handle(cmd);
      process.stdout.write(JSON.stringify({ id: cmd.id, ok: true, ...result }) + '\n');
    } catch (err) {
      process.stdout.write(JSON.stringify({ id: cmd.id, ok: false, error: err.message }) + '\n');
    }
  }
  if (browser) await browser.close();
}

main().catch(err => {
  process.stderr.write('Bridge fatal error: ' + err.message + '\n');
  process.exit(1);
});
