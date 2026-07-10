// Design-quality audit of the RUNNING site — run this before deploying, after
// the app is up (./scripts/serve.sh) and smoke.js/flow.js pass.
//
//   node scripts/audit.js [baseURL]     default http://localhost:8080
//
// WHY this and not `impeccable detect <source dirs>`: the detector must see the
// REAL assembled page. Defects like a section rule overriding a button's text
// color (making it invisible), or faded low-contrast footer text, exist ONLY in
// the rendered DOM — never in any single template file. So this crawls the
// running site, renders each page to HTML, inlines its CSS, and runs impeccable
// in FILE mode on that. No browser needed. Exit 0 = clean, 1 = findings to fix.
//
// It writes the raw findings to /tmp/forge-audit-findings.json too (for tooling).

const { execSync } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const BASE = (process.argv[2] || 'http://localhost:8080').replace(/\/$/, '');

// components.css is LOCKED — structure lives there so padding, footer insets,
// nav visibility and button readability can't regress per project. Restyle via
// tokens.css + app.css. (Maintainers: template-push refuses to ship if this
// constant doesn't match the file — update both together, deliberately.)
const COMPONENTS_SHA256 = 'bbda67dcfaf9b3f9e587bbcbfe8dfbb06c73fe3f9a4e234631564856535681b4';

async function get(url) {
  try { const r = await fetch(url); return r.ok ? await r.text() : ''; } catch { return ''; }
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

  // Discover internal routes from the homepage's links (nav + body). Skip assets,
  // auth actions, and /admin (Forge-owned, styled separately).
  const routes = new Set(['/']);
  for (const m of home.matchAll(/href=["'](\/[a-z0-9/_-]*)["']/gi)) {
    const p = m[1];
    if (/\.(css|js|png|jpe?g|svg|ico|webp|gif)$/i.test(p)) continue;
    if (/^\/(static|logout|admin)(\/|$)/.test(p)) continue;
    routes.add(p);
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'forge-audit-'));
  let n = 0;
  for (const r of routes) {
    const html = await get(BASE + r);
    if (!html) continue;
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
