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
// Password for signup/login steps defaults to "ownerpass123" (matches smoke.js,
// so signupOwner logs in cleanly when smoke.js already created the owner in the
// same data dir). Override with a "password" field on the step. Playwright is
// global + NODE_PATH is preset, so
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
  const pw = password || 'ownerpass123';
  await page.fill('input[type="email"]', email);
  await page.fill('input[type="password"]', pw);
  const navigated = page.waitForURL((u) => !u.pathname.startsWith(path), { timeout: 8000 }).then(() => true, () => false);
  await page.click('button[type="submit"]');
  if (await navigated) { await settle(page); return; }
  // Signup for an account that already exists (409) — or any submit that didn't
  // navigate — falls back to logging in, so the flow is idempotent regardless of
  // whether smoke.js already created the owner.
  if (path === '/signup') return auth(page, '/login', email, pw);
  throw new Error('submit did not navigate away from ' + path);
}

// Best-effort settle: wait for the network to go quiet so JS-rendered content is
// present, but don't fail if it never idles (SSE / long-poll pages).
async function settle(page) {
  await page.waitForLoadState('networkidle', { timeout: 3000 }).catch(() => {});
}

// JS errors seen during the flow — uncaught exceptions + app console.error
// (asset-load noise filtered). A broken client script fails the flow.
const jsErrors = [];

// On a selector failure, dump the page's real form fields + clickables so the
// fix is obvious (correct field name / button text) — no manual curl-probing of
// the HTML to reverse-engineer selectors, which is a big time sink.
async function describePage(page) {
  try {
    return await page.evaluate(() => {
      const take = (arr, f) => arr.map(f).filter(Boolean).slice(0, 20);
      const fields = take([...document.querySelectorAll('input, select, textarea')], (e) => {
        const id = e.name ? 'name="' + e.name + '"' : e.id ? 'id="' + e.id + '"' : '';
        if (!id) return '';
        return e.tagName.toLowerCase() + (e.type ? '[type=' + e.type + ']' : '') + ' ' + id;
      });
      const clickables = take([...document.querySelectorAll('button, input[type=submit], a[href]')], (e) => {
        const txt = (e.innerText || e.value || '').trim().replace(/\s+/g, ' ').slice(0, 30);
        return txt ? e.tagName.toLowerCase() + ' "' + txt + '"' : '';
      });
      return { fields, clickables };
    });
  } catch { return null; }
}

async function run(page, step, i) {
  const label = 'step ' + (i + 1) + ': ' + Object.keys(step)[0];
  try {
    if (step.signupOwner || step.signup) await auth(page, '/signup', step.signupOwner || step.signup, step.password);
    else if (step.login) await auth(page, '/login', step.login, step.password);
    else if (step.goto) { const r = await page.goto(BASE + step.goto, { waitUntil: 'load' }); if (!r || !r.ok()) throw new Error('status ' + (r && r.status())); await settle(page); }
    else if (step.click) { await page.click(step.click); await page.waitForLoadState('load'); await settle(page); }
    else if (step.fill) { for (const [sel, val] of Object.entries(step.fill)) await page.fill(sel, String(val)); }
    else if (step.expect) { const t = await page.locator('body').innerText(); if (!t.includes(step.expect)) throw new Error('page did not contain: ' + step.expect); }
    else if (step.expectUrl) { if (!page.url().includes(step.expectUrl)) throw new Error('url is ' + page.url() + ', wanted ' + step.expectUrl); }
    else if (step.expectFirstClick) {
      await page.click(step.expectFirstClick);
      await page.waitForLoadState('load');
      await settle(page);
      if (step.url && !page.url().includes(step.url)) throw new Error('first click did not reach ' + step.url + ' (got ' + page.url() + ')');
      if (step.css && !(await page.evaluate((c) => !!document.querySelector(`link[rel="stylesheet"][href*="${c}"]`), step.css)))
        throw new Error(step.css + ' not loaded after the click (unstyled — hx-boost="false" needed?)');
    } else throw new Error('unknown step: ' + JSON.stringify(step));
    pass(label);
  } catch (e) {
    let detail = e.message;
    // Selector-based step failed → show what IS on the page so the fix is obvious.
    if (step.fill || step.click || step.expectFirstClick || step.signup || step.signupOwner || step.login) {
      const d = await describePage(page);
      if (d) detail += '\n      page ' + page.url() + ' has fields: [' +
        (d.fields.join(', ') || 'none') + ']  clickables: [' + (d.clickables.join(', ') || 'none') + ']';
    }
    fail(label, detail);
  }
}

async function main() {
  const browser = await chromium.launch();
  const page = await (await browser.newContext()).newPage();
  page.setDefaultTimeout(15000);
  // Capture client-side JS failures — a broken script often passes the HTML
  // checks but breaks the page for real users.
  page.on('pageerror', (e) => jsErrors.push('uncaught: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const t = m.text();
    // asset-404 noise, and htmx logging a non-2xx server response (that is not a
    // JS bug — the flow's expect/expectUrl steps catch functional failures).
    if (/Failed to load resource|favicon|net::ERR_|Response Status Error Code/i.test(t)) return;
    jsErrors.push('console: ' + t);
  });

  for (let i = 0; i < steps.length; i++) {
    await run(page, steps[i], i);
    if (!results[results.length - 1].ok) break; // stop at the first failure — later steps depend on it
  }
  if (jsErrors.length) fail('no JS console errors', jsErrors.slice(0, 5).join(' | '));
  else pass('no JS console errors');
  await browser.close();

  let failed = 0;
  console.log('\n=== flow: ' + BASE + ' (' + STEPS_FILE + ') ===');
  for (const r of results) { console.log((r.ok ? 'PASS ' : 'FAIL ') + r.n + (r.ok ? '' : '  →  ' + r.d)); if (!r.ok) failed++; }
  console.log('=== ' + (failed ? failed + ' FAILED — fix before deploying' : 'flow passed') + ' ===\n');
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error('flow runner crashed:', e); process.exit(1); });
