// Forge browser smoke test — run this INSTEAD of writing your own Playwright
// script. It drives a real Chromium through the flows that silently break most
// often (auth that "does nothing" on first click, the /admin area loading
// unstyled) and prints a PASS/FAIL report. Exit code 0 = all good, 1 = a real
// bug you must fix before deploying.
//
//   node scripts/smoke.js [baseURL] [ownerEmail] [ownerPassword]
//   defaults: http://localhost:8080  owner@test.local  ownerpass123
//
// Playwright is preinstalled globally and NODE_PATH is preset, so this runs
// as-is — do not edit it or hunt for modules. It adapts: sites without accounts
// simply skip the auth checks.

const { chromium } = require('playwright');

const BASE = (process.argv[2] || 'http://localhost:8080').replace(/\/$/, '');
const EMAIL = process.argv[3] || 'owner@test.local';
const PASSWORD = process.argv[4] || 'ownerpass123';

const results = [];
const ok = (name) => results.push({ name, pass: true });
const fail = (name, detail) => results.push({ name, pass: false, detail });

async function main() {
  const browser = await chromium.launch();
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);

  // 1) Home renders.
  try {
    const resp = await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    if (!resp || !resp.ok()) throw new Error('status ' + (resp && resp.status()));
    if ((await page.locator('body').innerText()).trim().length < 20) throw new Error('page looks empty');
    ok('home page renders');
  } catch (e) { fail('home page renders', e.message); }

  // Detect whether this site has accounts (a real /login form).
  let hasAuth = false;
  try {
    await page.goto(BASE + '/login', { waitUntil: 'domcontentloaded' });
    hasAuth = await page.locator('input[type="password"]').count() > 0;
  } catch { /* no /login */ }

  if (!hasAuth) {
    ok('no accounts on this site — auth checks skipped');
  } else {
    // 2) Sign up the owner (first account = owner/admin).
    try {
      await page.goto(BASE + '/signup', { waitUntil: 'domcontentloaded' });
      await page.fill('input[type="email"]', EMAIL);
      await page.fill('input[type="password"]', PASSWORD);
      await Promise.all([
        page.waitForURL((u) => !u.pathname.startsWith('/signup'), { timeout: 15000 }),
        page.click('button[type="submit"], button:has-text("Create"), button:has-text("Sign up")'),
      ]);
      ok('signup lands logged in (owner account created)');
    } catch (e) { fail('signup lands logged in', e.message + ' — still on ' + page.url()); }

    // 3) Log out.
    try {
      const logout = page.locator('form[action="/logout"] button, a[href="/logout"], button:has-text("Log out")').first();
      if (await logout.count()) { await logout.click(); await page.waitForLoadState('domcontentloaded'); }
      ok('logout works');
    } catch (e) { fail('logout works', e.message); }

    // 4) Log in — must succeed on the FIRST click (the hx-boost "login does
    //    nothing" trap). Assert we actually leave /login.
    try {
      await page.goto(BASE + '/login', { waitUntil: 'domcontentloaded' });
      await page.fill('input[type="email"]', EMAIL);
      await page.fill('input[type="password"]', PASSWORD);
      await Promise.all([
        page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 15000 }),
        page.click('button[type="submit"], button:has-text("Log in"), button:has-text("Sign in")'),
      ]);
      ok('login works on the first click');
    } catch (e) { fail('login works on the first click', 'first click did not navigate away from /login (hx-boost trap?) — ' + e.message); }

    // 5) The hx-boost cross-stylesheet bug: the public site uses app.css and
    //    /admin uses admin.css. Reproduce the real transition — stand on a
    //    PUBLIC page (app.css) and CLICK the "Site admin" link. If it's a boosted
    //    link it swaps only the body and keeps app.css, so /admin renders
    //    unstyled until a reload. Assert admin.css is actually referenced after.
    try {
      await page.goto(BASE + '/', { waitUntil: 'load' }); // public page, still logged in
      const adminLink = page.locator('a[href="/admin"]').first();
      if (await adminLink.count()) {
        await adminLink.click();
        await page.waitForLoadState('load'); // wait for the head's stylesheets
        if (!page.url().includes('/admin')) throw new Error('nav did not reach /admin');
        const hasAdminCss = await page.evaluate(() =>
          !!document.querySelector('link[rel="stylesheet"][href*="admin.css"]'));
        if (!hasAdminCss) throw new Error('admin.css not loaded after clicking "Site admin" from a public page (unstyled until reload — set hx-boost="false" on that link)');
        ok('admin is styled when reached via the nav link');
      } else {
        ok('no admin link for this account — admin styling check skipped');
      }
    } catch (e) { fail('admin is styled when reached via the nav link', e.message); }
  }

  await browser.close();

  // Report.
  let failed = 0;
  console.log('\n=== browser smoke test: ' + BASE + ' ===');
  for (const r of results) {
    console.log((r.pass ? 'PASS ' : 'FAIL ') + r.name + (r.pass ? '' : '  →  ' + r.detail));
    if (!r.pass) failed++;
  }
  console.log('=== ' + (failed ? failed + ' FAILED — fix before deploying' : 'all passed') + ' ===\n');
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error('smoke test crashed:', e); process.exit(1); });
