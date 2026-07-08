// Declarative browser-flow runner — the companion to smoke.js for testing the
// plan's SITE-SPECIFIC flows (a booking, a custom form) WITHOUT writing or
// debugging your own Playwright code. You describe the steps in a small JSON
// file; this runs them in a real Chromium and prints PASS/FAIL. Use this instead
// of hand-rolling a Playwright script.
//
//   node scripts/flow.js <baseURL> <steps.json>
//
// steps.json is a JSON array of step objects, run in order. Supported steps:
//   { "signupOwner": "owner@test.local" }            first account → owner/admin
//   { "signup": "member@test.local" }                a normal member account
//   { "login":  "member@test.local" }                log in an existing account
//   { "goto":   "/member/book" }                      navigate to a path
//   { "click":  "a[href='/member/book']" }            click a selector
//   { "fill":   { "input[name='date']": "2026-08-01", "select[name='space']": "Hot desk" } }
//   { "expect": "Booking confirmed" }                 body text must appear
//   { "expectUrl": "/member/bookings" }               URL path must contain this
//   { "expectFirstClick": "a[href='/admin']", "url": "/admin", "css": "admin.css" }
//        click a link and assert it reached `url` on the FIRST click and (opt) that
//        a stylesheet matching `css` loaded — catches hx-boost "does nothing" /
//        "unstyled admin" bugs on a custom nav.
//
// Password for signup/login steps defaults to "flowpass123" (override with a
// "password" field on the step). Playwright is global + NODE_PATH is preset, so
// require('playwright') works from anywhere — just run it.

const { chromium } = require('playwright');

const BASE = (process.argv[2] || 'http://localhost:8080').replace(/\/$/, '');
const STEPS_FILE = process.argv[3];
if (!STEPS_FILE) { console.error('usage: node scripts/flow.js <baseURL> <steps.json>'); process.exit(2); }
const steps = JSON.parse(require('fs').readFileSync(STEPS_FILE, 'utf8'));

const results = [];
const pass = (n) => results.push({ n, ok: true });
const fail = (n, d) => results.push({ n, ok: false, d });

async function auth(page, path, email, password) {
  await page.goto(BASE + path, { waitUntil: 'domcontentloaded' });
  await page.fill('input[type="email"]', email);
  await page.fill('input[type="password"]', password || 'flowpass123');
  await Promise.all([
    page.waitForURL((u) => !u.pathname.startsWith(path), { timeout: 15000 }),
    page.click('button[type="submit"]'),
  ]);
}

async function run(page, step, i) {
  const label = 'step ' + (i + 1) + ': ' + Object.keys(step)[0];
  try {
    if (step.signupOwner || step.signup) await auth(page, '/signup', step.signupOwner || step.signup, step.password);
    else if (step.login) await auth(page, '/login', step.login, step.password);
    else if (step.goto) { const r = await page.goto(BASE + step.goto, { waitUntil: 'load' }); if (!r || !r.ok()) throw new Error('status ' + (r && r.status())); }
    else if (step.click) { await page.click(step.click); await page.waitForLoadState('load'); }
    else if (step.fill) { for (const [sel, val] of Object.entries(step.fill)) await page.fill(sel, String(val)); }
    else if (step.expect) { const t = await page.locator('body').innerText(); if (!t.includes(step.expect)) throw new Error('page did not contain: ' + step.expect); }
    else if (step.expectUrl) { if (!page.url().includes(step.expectUrl)) throw new Error('url is ' + page.url() + ', wanted ' + step.expectUrl); }
    else if (step.expectFirstClick) {
      await page.click(step.expectFirstClick);
      await page.waitForLoadState('load');
      if (step.url && !page.url().includes(step.url)) throw new Error('first click did not reach ' + step.url + ' (got ' + page.url() + ')');
      if (step.css && !(await page.evaluate((c) => !!document.querySelector(`link[rel="stylesheet"][href*="${c}"]`), step.css)))
        throw new Error(step.css + ' not loaded after the click (unstyled — hx-boost="false" needed?)');
    } else throw new Error('unknown step: ' + JSON.stringify(step));
    pass(label);
  } catch (e) { fail(label, e.message); }
}

async function main() {
  const browser = await chromium.launch();
  const page = await (await browser.newContext()).newPage();
  page.setDefaultTimeout(15000);
  for (let i = 0; i < steps.length; i++) {
    await run(page, steps[i], i);
    if (!results[results.length - 1].ok) break; // stop at the first failure — later steps depend on it
  }
  await browser.close();

  let failed = 0;
  console.log('\n=== flow: ' + BASE + ' (' + STEPS_FILE + ') ===');
  for (const r of results) { console.log((r.ok ? 'PASS ' : 'FAIL ') + r.n + (r.ok ? '' : '  →  ' + r.d)); if (!r.ok) failed++; }
  console.log('=== ' + (failed ? failed + ' FAILED — fix before deploying' : 'flow passed') + ' ===\n');
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error('flow runner crashed:', e); process.exit(1); });
