// Design-quality audit of the RUNNING site — run this before deploying, after
// the app is up (./scripts/serve.sh) and smoke.js/flow.js pass.
//
//   node scripts/audit.js [baseURL]     default http://localhost:8080
//
// WHY this and not `impeccable detect <source dirs>`: the detector must see the
// REAL assembled page. Defects like a section rule overriding a button's text
// color (making it invisible), or faded low-contrast footer text, exist ONLY in
// the rendered DOM — never in any single template file. So this crawls the
// running site, inlines its CSS for impeccable, then drives desktop + mobile
// Chromium for layout, keyboard, metadata and image-delivery checks.
//
// It writes the raw findings to /tmp/forge-audit-findings.json too (for tooling).

const { execSync } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { chromium } = require('playwright');

const BASE = (process.argv[2] || 'http://localhost:8080').replace(/\/$/, '');
const MAX_ROUTES = 12;
const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 900 },
  { name: 'mobile', width: 390, height: 844 },
];
const PAGE_IMAGE_BUDGET = 2_500_000;
const HERO_IMAGE_BUDGET = 500_000;
const CONTENT_IMAGE_BUDGET = 350_000;
const LOGO_IMAGE_BUDGET = 100_000;
const STARTER_FAVICON_SHA256 = 'cc89472f7ac89404405941713fc4fdd3539f6eb9d60395b0c705b0b6fcb2b646';

// components.css is LOCKED — structure lives there so padding, footer insets,
// nav visibility and button readability can't regress per project. Restyle via
// tokens.css + app.css. (Maintainers: template-push refuses to ship if this
// constant doesn't match the file — update both together, deliberately.)
const COMPONENTS_SHA256 = '88cf8d3514ac78c1717ada8f24e2f47e41095d4a672744c6e10c0250e6f01ad1';

async function get(url) {
  try { const r = await fetch(url); return r.ok ? await r.text() : ''; } catch { return ''; }
}

async function getBytes(url) {
  try {
    const r = await fetch(url);
    return r.ok ? Buffer.from(await r.arrayBuffer()) : null;
  } catch { return null; }
}

// Replace every linked stylesheet with its fetched contents so file-mode
// impeccable sees the real cascade (it does not fetch remote/relative CSS itself).
async function inlineCss(html) {
  const links = [...html.matchAll(/<link[^>]*rel=["']?stylesheet["']?[^>]*>/gi)];
  for (const tag of links) {
    const href = (tag[0].match(/href=["']([^"']+)["']/i) || [])[1];
    if (!href) continue;
    const u = href.startsWith('http') ? href : BASE + (href.startsWith('/') ? href : '/' + href);
    const css = await get(u);
    if (css) html = html.replace(tag[0], '<style>\n' + css + '\n</style>');
  }
  return html;
}

function routeFrom(href) {
  try {
    const u = new URL(href, BASE + '/');
    if (u.origin !== new URL(BASE).origin) return '';
    const p = u.pathname.replace(/\/+$/, '') || '/';
    if (/\.(css|js|png|jpe?g|svg|ico|webp|avif|gif|xml|txt)$/i.test(p)) return '';
    if (/^\/(static|logout|admin|app)(\/|$)/.test(p)) return '';
    return p;
  } catch { return ''; }
}

function linkedRoutes(html) {
  const out = [];
  for (const m of html.matchAll(/href=["']([^"']+)["']/gi)) {
    const p = routeFrom(m[1]);
    if (p) out.push(p);
  }
  return out;
}

async function discoverRoutes(home) {
  const routes = new Set(['/']);
  const queue = ['/'];
  const sitemap = await get(BASE + '/sitemap.xml');
  for (const m of sitemap.matchAll(/<loc>(.*?)<\/loc>/gi)) {
    const p = routeFrom(m[1]);
    if (p && !routes.has(p)) { routes.add(p); queue.push(p); }
  }
  const cached = new Map([['/', home]]);
  while (queue.length && routes.size <= MAX_ROUTES) {
    const route = queue.shift();
    const html = cached.get(route) || await get(BASE + route);
    cached.set(route, html);
    for (const p of linkedRoutes(html)) {
      if (routes.size >= MAX_ROUTES) break;
      if (!routes.has(p)) { routes.add(p); queue.push(p); }
    }
  }
  return { routes: [...routes], cached };
}

function customFinding(antipattern, name, severity, description, snippet, route = '/') {
  return { antipattern, name, severity, description, file: route, line: 0, snippet };
}

async function responseBytes(url, cache) {
  if (cache.has(url)) return cache.get(url);
  let info = { bytes: 0, type: '' };
  try {
    const r = await fetch(url, { signal: AbortSignal.timeout(12000) });
    const declared = Number(r.headers.get('content-length') || 0);
    info = { bytes: declared || (await r.arrayBuffer()).byteLength, type: r.headers.get('content-type') || '' };
  } catch {}
  cache.set(url, info);
  return info;
}

async function runtimeAudit(routes) {
  const findings = [];
  const bytesCache = new Map();
  const browser = await chromium.launch();
  try {
    for (const viewport of VIEWPORTS) {
      const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height } });
      const page = await context.newPage();
      page.setDefaultTimeout(12000);
      for (const route of routes) {
        await page.goto(BASE + route, { waitUntil: 'networkidle', timeout: 20000 });
        const state = await page.evaluate(({ vh }) => {
          const visible = (el) => { const s = getComputedStyle(el), r = el.getBoundingClientRect(); return s.display !== 'none' && s.visibility !== 'hidden' && r.width > 0 && r.height > 0; };
          const firstLabelTextRect = (label) => {
            const walker = document.createTreeWalker(label, NodeFilter.SHOW_TEXT);
            let node;
            while ((node = walker.nextNode())) {
              if (!node.textContent.trim()) continue;
              const parent = node.parentElement;
              if (parent?.closest('input, textarea, select, option, .field-hint, .field-error, .hint, .req, .required-mark, .required-indicator, [data-required-marker]')) continue;
              const range = document.createRange();
              range.selectNodeContents(node);
              const rect = [...range.getClientRects()].find((r) => r.width > 0 && r.height > 0);
              if (rect) return { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
            }
            return null;
          };
          const formAlignment = [];
          const markers = [...document.querySelectorAll('label :is(.req, .required-mark, .required-indicator, [data-required-marker]), label [aria-hidden="true"]')]
            .filter((el) => /^(\*|∗)$/.test(el.textContent.trim()) && visible(el));
          for (const marker of markers) {
            const label = marker.closest('label');
            const text = label && firstLabelTextRect(label);
            if (!text) continue;
            const r = marker.getBoundingClientRect();
            const delta = Math.abs((text.y + text.height / 2) - (r.y + Math.min(r.height, text.height) / 2));
            if (delta > Math.max(6, text.height * .45)) {
              formAlignment.push({ kind: 'required marker', name: (label.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 70), delta: Math.round(delta) });
            }
          }
          for (const control of document.querySelectorAll('input[type="checkbox"], input[type="radio"]')) {
            if (!visible(control) || control.classList.contains('nav-toggle')) continue;
            const label = [...control.labels].find(visible);
            const text = label && firstLabelTextRect(label);
            if (!text) continue;
            const r = control.getBoundingClientRect();
            const delta = Math.abs((text.y + text.height / 2) - (r.y + Math.min(r.height, text.height) / 2));
            if (delta > Math.max(7, text.height * .55)) {
              formAlignment.push({ kind: control.type, name: (label.textContent || control.name || '').trim().replace(/\s+/g, ' ').slice(0, 70), delta: Math.round(delta) });
            }
          }
          const images = [...document.images].filter(visible).map((img) => {
            const r = img.getBoundingClientRect();
            const near = img.closest('article, li, figure, section, header');
            return {
              src: img.currentSrc || img.src, alt: img.getAttribute('alt'),
              widthAttr: img.getAttribute('width'), heightAttr: img.getAttribute('height'),
              loading: img.getAttribute('loading') || 'auto', priority: img.getAttribute('fetchpriority') || '',
              top: Math.round(r.top + scrollY), renderedWidth: Math.round(r.width), naturalWidth: img.naturalWidth,
              logo: !!img.closest('header') || /logo|logotyp|brand|mark/i.test(`${img.src} ${img.alt || ''} ${img.className}`),
              context: (near?.innerText || '').trim().replace(/\s+/g, ' ').slice(0, 100),
            };
          });
          const smallTargets = [...document.querySelectorAll('button, a.btn, input:not([type=hidden]):not(.nav-toggle), select, textarea, .navlinks a, .nav-burger')]
            .filter(visible).map((el) => {
              const choiceLabel = el.matches('input[type="checkbox"], input[type="radio"]') ? [...el.labels].find(visible) : null;
              const r = (choiceLabel || el).getBoundingClientRect();
              return { tag: el.tagName, text: ((choiceLabel?.textContent) || el.textContent || el.getAttribute('aria-label') || el.getAttribute('name') || '').trim().slice(0, 50), w: Math.round(r.width), h: Math.round(r.height) };
            })
            .filter((x) => x.w < 44 || x.h < 44);
          const canonical = document.querySelector('link[rel=canonical]')?.href || '';
          const ogURL = document.querySelector('meta[property="og:url"]')?.content || '';
          let jsonldURL = '';
          try { jsonldURL = JSON.parse(document.querySelector('script[type="application/ld+json"]')?.textContent || '{}').url || ''; } catch {}
          return {
            lang: document.documentElement.lang, description: document.querySelector('meta[name=description]')?.content || '',
            theme: document.querySelector('meta[name=theme-color]')?.content || '',
            colorScheme: document.querySelector('meta[name=color-scheme]')?.content || '',
            favicon: document.querySelector('link[rel~="icon"]')?.href || '', canonical, ogURL, jsonldURL,
            canonicalHost: canonical ? new URL(canonical).host : '', locationHost: location.host,
            h1: document.querySelectorAll('main h1').length,
            skip: document.querySelector('a[href="#main-content"]') !== null,
            overflow: Math.max(document.body.scrollWidth, document.documentElement.scrollWidth) - innerWidth,
            pageHeight: document.documentElement.scrollHeight, images, smallTargets, formAlignment,
            hasBurger: document.querySelector('.nav-burger') !== null,
            toggleFocusable: (() => { const el = document.querySelector('.nav-toggle'); return !el || (!el.hidden && el.tabIndex >= 0 && getComputedStyle(el).display !== 'none'); })(),
          };
        }, { vh: viewport.height });

        const scope = `${route} at ${viewport.width}px`;
        if (viewport.name === 'desktop') {
          if (!state.lang) findings.push(customFinding('missing-language', 'Missing page language', 'error', 'Set the public Language option so assistive technology knows the page language.', scope, route));
          if (!state.description && !/^\/(login|signup)$/.test(route)) findings.push(customFinding('missing-description', 'Missing page description', 'error', 'Every public page needs a specific meta description.', scope, route));
          if (!state.theme || !state.colorScheme) findings.push(customFinding('missing-browser-theme', 'Missing browser theme metadata', 'warning', 'Set theme-color and color-scheme to match the project palette.', scope, route));
          if (!state.favicon) findings.push(customFinding('missing-favicon', 'Missing favicon', 'warning', 'Ship a project-specific favicon.', scope, route));
          if (!state.canonical) findings.push(customFinding('missing-canonical', 'Missing canonical URL', 'error', 'Every public page needs an absolute canonical URL.', scope, route));
          if (state.canonical && state.canonicalHost !== state.locationHost) findings.push(customFinding('canonical-host-mismatch', 'Canonical host does not match the public site', 'error', 'Canonical, Open Graph, sitemap and JSON-LD must use the branded preview or live custom domain.', `${state.canonicalHost} instead of ${state.locationHost}`, route));
          for (const [kind, value] of [['Open Graph URL', state.ogURL], ['JSON-LD URL', state.jsonldURL]]) {
            if (value && new URL(value, BASE).host !== state.locationHost) findings.push(customFinding('metadata-host-mismatch', `${kind} does not match the public site`, 'error', 'Public metadata must use the branded preview or live custom domain.', `${value}`, route));
          }
          if (state.h1 !== 1) findings.push(customFinding('h1-count', 'Page needs exactly one H1', 'error', 'Give each page exactly one primary heading.', `${scope}: found ${state.h1}`, route));
          if (!state.skip) findings.push(customFinding('missing-skip-link', 'Missing skip link', 'error', 'Keep the starter skip-to-content link when redesigning the header.', scope, route));
        }
        if (state.overflow > 2) findings.push(customFinding('horizontal-overflow', 'Horizontal overflow', 'error', 'The page is wider than the viewport; fix the overflowing element.', `${scope}: ${state.overflow}px overflow`, route));
        for (const issue of state.formAlignment.slice(0, 12)) {
          findings.push(customFinding('form-label-alignment', 'Form label parts are on different rows', 'error', 'Keep required markers inline with their field name, and keep each checkbox/radio control aligned with its label text. Use the locked field-label and choice-row primitives.', `${scope}: ${issue.kind} “${issue.name}” is displaced by ${issue.delta}px`, route));
        }
        if (viewport.name === 'desktop' && route === '/' && state.pageHeight > viewport.height * 6) findings.push(customFinding('overlong-home', 'Homepage is excessively long', 'warning', 'Feature a focused selection and link to full detail pages instead of reproducing a catalogue on the home page.', `${state.pageHeight}px tall at ${viewport.height}px viewport`, route));
        if (viewport.name === 'mobile') {
          for (const t of state.smallTargets.slice(0, 8)) findings.push(customFinding('small-touch-target', 'Touch target below 44px', 'error', 'Increase the interactive target to at least 44×44px on mobile.', `${scope}: ${t.tag} “${t.text}” is ${t.w}×${t.h}`, route));
          if (state.hasBurger && !state.toggleFocusable) findings.push(customFinding('keyboard-mobile-nav', 'Mobile menu is not keyboard reachable', 'error', 'Keep the nav toggle focusable and expose a visible focus state.', scope, route));
          if (state.hasBurger && state.toggleFocusable) {
            try {
              await page.focus('.nav-toggle'); await page.keyboard.press('Space');
              const opened = await page.evaluate(() => { const t = document.querySelector('.nav-toggle'), n = document.querySelector('#site-nav'); return !!t?.checked && n && getComputedStyle(n).display !== 'none'; });
              if (!opened) findings.push(customFinding('keyboard-mobile-nav', 'Mobile menu does not open from the keyboard', 'error', 'Space on the focused menu control must reveal the navigation.', scope, route));
            } catch { findings.push(customFinding('keyboard-mobile-nav', 'Mobile menu keyboard test failed', 'error', 'Verify the menu can be focused and opened with Space.', scope, route)); }
          }
        }

        let pageBytes = 0;
        for (const img of state.images) {
          if (!img.src || img.src.startsWith('data:')) continue;
          const info = await responseBytes(img.src, bytesCache); pageBytes += info.bytes;
          const above = img.top < viewport.height * 1.2;
          const limit = img.logo ? LOGO_IMAGE_BUDGET : above ? HERO_IMAGE_BUDGET : CONTENT_IMAGE_BUDGET;
          if (!img.widthAttr || !img.heightAttr) findings.push(customFinding('image-dimensions', 'Image lacks intrinsic dimensions', 'error', 'Add width and height attributes to prevent layout shift.', `${scope}: ${img.src}`, route));
          if (!above && img.loading !== 'lazy') findings.push(customFinding('below-fold-eager-image', 'Below-fold image loads eagerly', 'warning', 'Set loading="lazy" on images below the first viewport.', `${scope}: ${img.context || img.src}`, route));
          if (above && img.loading === 'lazy') findings.push(customFinding('lazy-lcp-image', 'Above-fold image is lazy-loaded', 'error', 'Load the hero/LCP image eagerly and use fetchpriority="high" where appropriate.', `${scope}: ${img.context || img.src}`, route));
          if (info.bytes > limit) findings.push(customFinding('oversized-image', 'Image exceeds its delivery budget', 'error', 'Use the automatic WebP variants or a smaller logo/source image.', `${scope}: ${Math.round(info.bytes / 1024)}KB (limit ${Math.round(limit / 1024)}KB) — ${img.src}`, route));
          if (info.bytes > 100_000 && !/(image\/(webp|avif|svg\+xml))|\.(webp|avif|svg)(\?|$)/i.test(`${info.type} ${img.src}`)) findings.push(customFinding('legacy-image-format', 'Large image is not delivered in a modern format', 'error', 'Run scripts/optimize-images.js and reference the asset helper/WebP output.', `${scope}: ${img.src}`, route));
          if (img.renderedWidth > 0 && img.naturalWidth > img.renderedWidth * 3 && info.bytes > 150_000) findings.push(customFinding('oversized-image-dimensions', 'Image has far more pixels than it displays', 'warning', 'Use assetSrcSet with an honest sizes value.', `${scope}: ${img.naturalWidth}px source rendered at ${img.renderedWidth}px`, route));
        }
        if (pageBytes > PAGE_IMAGE_BUDGET) findings.push(customFinding('page-image-budget', 'Page image payload exceeds 2.5MB', 'error', 'Reduce image count/size and use responsive WebP delivery.', `${scope}: ${Math.round(pageBytes / 1024)}KB`, route));
      }
      await context.close();
    }
  } finally { await browser.close(); }
  return findings;
}

async function main() {
  const home = await get(BASE + '/');
  if (!home) { console.error('audit: site not reachable at ' + BASE + ' — is ./scripts/serve.sh running?'); process.exit(2); }

  // Integrity gate: the locked structural stylesheet must ship unmodified.
  const comp = await get(BASE + '/static/components.css');
  const compSha = crypto.createHash('sha256').update(comp).digest('hex');
  if (compSha !== COMPONENTS_SHA256) {
    console.error('\n=== design audit: FAIL — components.css was modified (it is LOCKED) ===');
    console.error('Revert components.css to the starter version. Restyle by setting');
    console.error('variables in tokens.css and writing project CSS in app.css instead.');
    process.exit(1);
  }

  // Crawl sitemap + links recursively so a page does not escape review merely
  // because the home page forgot to link it.
  const discovered = await discoverRoutes(home);
  const routes = discovered.routes;
  let starterFavicon = false;

  // The starter icon is intentionally neutral. A finished customer site must
  // replace it so browser tabs, bookmarks and saved shortcuts carry the brand.
  const faviconHref = (home.match(/<link[^>]*rel=["'][^"']*icon[^"']*["'][^>]*href=["']([^"']+)["']/i)
    || home.match(/<link[^>]*href=["']([^"']+)["'][^>]*rel=["'][^"']*icon[^"']*["']/i) || [])[1];
  if (faviconHref) {
    const faviconURL = new URL(faviconHref, BASE + '/').href;
    const faviconBytes = await getBytes(faviconURL);
    if (faviconBytes && crypto.createHash('sha256').update(faviconBytes).digest('hex') === STARTER_FAVICON_SHA256) {
      starterFavicon = true;
    }
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'forge-audit-'));
  const pages = [];
  let n = 0;
  for (const r of routes) {
    const html = discovered.cached.get(r) || await get(BASE + r);
    if (!html) continue;
    pages.push(html);
    fs.writeFileSync(path.join(dir, 'page' + n + '.html'), await inlineCss(html));
    n++;
  }
  console.log('rendered ' + n + ' page(s) from ' + BASE + ' → running impeccable…');

  // Redirect impeccable's JSON to a file rather than capturing stdout: it exits 2
  // when it finds issues, and Node truncates a non-zero-exit child's captured
  // stdout — the file is always written in full.
  const rawFile = path.join(os.tmpdir(), 'forge-audit-raw.json');
  try { execSync('impeccable detect --json ' + JSON.stringify(dir) + ' > ' + JSON.stringify(rawFile), { stdio: 'ignore' }); }
  catch { /* exit 2 = findings; the file is still complete */ }
  const out = fs.existsSync(rawFile) ? fs.readFileSync(rawFile, 'utf8') : '[]';

  let findings;
  try { findings = JSON.parse(out); }
  catch { console.error('audit: impeccable output was not JSON:\n' + out.slice(0, 200)); process.exit(1); }

  if (starterFavicon) {
    findings.push(customFinding('starter-favicon', 'Starter favicon is still in use', 'error', 'Replace static/favicon.svg with a simple project-specific brand mark.', faviconHref));
  }

  const publicHost = new URL(BASE).host;
  const sitemap = await get(BASE + '/sitemap.xml');
  for (const m of sitemap.matchAll(/<loc>(.*?)<\/loc>/gi)) {
    try { if (new URL(m[1]).host !== publicHost) findings.push(customFinding('sitemap-host-mismatch', 'Sitemap uses a non-public host', 'error', 'Generate sitemap URLs from the branded preview or live custom domain.', m[1])); } catch {}
  }
  const robots = await get(BASE + '/robots.txt');
  const robotSitemap = (robots.match(/^Sitemap:\s*(\S+)/mi) || [])[1];
  try { if (robotSitemap && new URL(robotSitemap).host !== publicHost) findings.push(customFinding('robots-host-mismatch', 'robots.txt uses a non-public sitemap host', 'error', 'Point robots.txt at the public sitemap URL.', robotSitemap)); } catch {}

  try { findings.push(...await runtimeAudit(routes)); }
  catch (e) { findings.push(customFinding('browser-audit-failed', 'Browser quality audit failed', 'error', 'The responsive quality gate must run before deploy.', e.message)); }

  // Orphaned auth pages: the starter always serves /login and /signup. If a
  // route serves a real page (200, not a redirect) but no crawled page links
  // to it, visitors can never reach it — a recurring nav-rebuild mistake.
  const linked = new Set();
  for (const html of pages)
    for (const m of html.matchAll(/href=["'](\/[a-z0-9/_-]*)["']/gi)) linked.add(m[1]);
  for (const route of ['/login', '/signup']) {
    let status = 0;
    try { status = (await fetch(BASE + route, { redirect: 'manual' })).status; } catch {}
    if (status === 200 && !linked.has(route)) {
      findings.push({
        antipattern: 'orphaned-auth-page', name: 'Orphaned auth page', severity: 'error',
        description: route + ' serves a page but no page links to it.',
        file: 'internal/web/templates/layout.html', line: 0,
        snippet: route + ' exists but is unreachable — keep the discreet owner-login link in the footer; /login must link to /signup.',
      });
    }
  }
  try { fs.writeFileSync('/tmp/forge-audit-findings.json', JSON.stringify(findings)); } catch {}

  if (!findings.length) { console.log('\n=== design audit: clean ✓ ==='); process.exit(0); }

  // Group by type; show the worst offenders with their measured values.
  const by = {};
  for (const f of findings) (by[f.antipattern] = by[f.antipattern] || []).push(f);
  console.log('\n=== design audit: ' + findings.length + ' finding(s) across ' + n + ' page(s) — FIX before deploy ===');
  for (const [type, list] of Object.entries(by)) {
    console.log('\n' + type + ' (' + list.length + '):');
    for (const f of list.slice(0, 6)) console.log('  [' + f.severity + '] ' + (f.snippet || f.name || '').slice(0, 90));
  }
  console.log('\nThese are on the RENDERED pages. Common cause: a section/link rule (e.g. .section-dark a)\noverriding a .btn color, or opacity making text too faint. Fix the CSS and re-run.');
  process.exit(1);
}

main().catch((e) => { console.error('audit crashed:', e.message); process.exit(1); });
