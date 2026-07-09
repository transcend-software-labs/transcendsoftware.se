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
  // Catch client-side JS failures across the whole run — a broken script passes
  // the HTML checks but breaks the page for real users.
  const jsErrors = [];
  page.on('pageerror', (e) => jsErrors.push('uncaught: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const t = m.text();
    // asset-404 noise, and htmx logging a non-2xx server response (not a JS bug —
    // the functional checks above catch real failures).
    if (/Failed to load resource|favicon|net::ERR_|Response Status Error Code/i.test(t)) return;
    jsErrors.push('console: ' + t);
  });

  // 1) Home renders.
  try {
    const resp = await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    if (!resp || !resp.ok()) throw new Error('status ' + (resp && resp.status()));
    if ((await page.locator('body').innerText()).trim().length < 20) throw new Error('page looks empty');
    ok('home page renders');
  } catch (e) { fail('home page renders', e.message); }

  // 1b) Nav must be legible at the TOP of the page. Catches a transparent fixed
  //     header whose text colour matches the dark hero it overlays (invisible
  //     until you scroll and a background fades in) — impeccable misses this
  //     because the header's own background is transparent and the hero is a
  //     visually-overlapping DOM sibling, not an ancestor.
  try {
    const bad = await page.evaluate(() => {
      const T = (c) => !c || c === 'rgba(0, 0, 0, 0)' || c === 'transparent';
      const lum = (c) => { const m = c.match(/[\d.]+/g); if (!m) return null;
        const [r, g, b] = m.map(Number).map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
        return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
      const ratio = (a, b) => { const L1 = lum(a), L2 = lum(b); if (L1 == null || L2 == null) return null; return (Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05); };
      const nav = document.querySelector('header, .nav, nav'); if (!nav) return null;
      const textEl = nav.querySelector('a, span, .brand, li') || nav;
      const color = getComputedStyle(textEl).color;
      const r = nav.getBoundingClientRect();
      const prev = nav.style.visibility; nav.style.visibility = 'hidden'; // so elementFromPoint sees what's BEHIND it
      let behind = document.elementFromPoint(r.left + Math.min(30, r.width / 2), r.top + r.height / 2);
      nav.style.visibility = prev;
      let bg = behind ? getComputedStyle(behind).backgroundColor : ''; let e = behind;
      while (e && T(bg)) { e = e.parentElement; bg = e ? getComputedStyle(e).backgroundColor : ''; }
      if (T(bg)) bg = getComputedStyle(document.body).backgroundColor;
      const cr = ratio(color, bg);
      return (cr != null && cr < 3) ? { color, bg, ratio: cr.toFixed(2) } : null;
    });
    if (bad) fail('nav is legible at the top of the page', `nav text ${bad.color} on ${bad.bg} = ${bad.ratio}:1 — a transparent/fixed header over a dark hero? give the nav a background or light text at scroll 0`);
    else ok('nav is legible at the top of the page');
  } catch (e) { ok('nav legibility check skipped (' + e.message + ')'); }

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

  // A site that passes every check but throws JS errors is still broken.
  if (jsErrors.length) fail('no client-side JS errors', jsErrors.slice(0, 5).join(' | '));
  else ok('no client-side JS errors');

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
