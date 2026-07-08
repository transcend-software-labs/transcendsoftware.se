# Build agent conventions — Transcend Forge Go starter

You are extending a working, production-ready Go application. **Do not scaffold
a new project.** Read this file, then the code, then make the smallest change
that implements the plan.

## What this app already does

- One Go binary serves everything: server-rendered HTML (`html/template`),
  static assets, and all backend logic. Templates, CSS and migrations are
  **embedded** — the binary is the entire app.
- SQLite persistence in `$DATA_DIR` (default `data/`, `/data` in the container),
  on a durable volume, with Litestream streaming continuous backups to object
  storage when configured (see `entrypoint.sh` — don't change the entrypoint).
- Auth: signup/login/logout, bcrypt, DB-backed sessions (hashed tokens), CSRF
  helper. The **first account created is the site owner** (`is_admin`); when
  `OWNER_EMAIL` is set, that first account is reserved for that address.
- **Site admin at `/admin`** (owner-only): renders EVERY table in the database
  by introspection — browse, row detail, delete, CSV export. New tables appear
  there automatically; there is nothing to wire up.
- **Notification hooks**: in `/admin`, the owner can turn on "email me when a
  row is added" for any table (a trigger feeds `_outbox`, a background
  dispatcher sends). Works for every table automatically — you don't build
  notifications; just store the data.
- Public contact form on `/` → stored in `messages` → readable in `/admin`.
- `/healthz` for platform health checks. Graceful shutdown.

## Layout

    main.go                     config, wiring, server lifecycle
    internal/db/                Open (SQLite) + embedded migrations runner
    internal/db/migrations/     numbered .sql files, applied in order
    internal/auth/              passwords, users, sessions, cookies
    internal/web/server.go      routes, render, currentUser, requireUser, CSRF
    internal/web/handlers.go    page + form handlers
    internal/web/templates/     html/template pages ("head"/"foot" layout)
    internal/web/static/        app.css

## How to make common changes

- **New page:** add `templates/<name>.html` (use `{{template "head" .}}` /
  `{{template "foot" .}}`), a handler in `handlers.go`, a route in `Handler()`.
- **New table/column:** add `internal/db/migrations/000N_<what>.sql` (next
  number). Never edit an already-numbered file.
- **Authed page:** wrap the handler with `s.requireUser(...)`.
- **Any authenticated POST:** include the hidden `csrf_token` input (see the
  logout form) and call `s.checkCSRF(r)` first.
- **Content/branding:** replace "Your Site" in `layout.html`, the landing page,
  and `static/app.css` variables. Write real, complete copy in the customer's
  language (Swedish unless the brief says otherwise).

## Design — decided per project, not by this template

This starter provides **boilerplate, not a look**. Its neutral styling exists
only so the app works before you touch it. Every project's plan carries a
Design section with the customer's chosen direction — implement *that*.

**Use the `frontend-design` skill** (in `.opencode/skills/`) as your design
playbook before and while building the UI — it covers grounding the look in the
subject, picking a distinctive palette + type, spending your boldness in one
signature element, and avoiding the templated defaults. The bullets below are
Forge-specific rules layered on top of it.

- Restyle `static/app.css` completely: palette, typography, spacing, layout.
  Replace the CSS variables and go far beyond them if the direction calls for
  it. Nothing about the starter's look should survive unless it happens to fit.
- Redesign the landing page structure freely (hero, sections, imagery).
- **Do NOT touch the site admin's look**: `static/admin.css`, `admin_layout.html`,
  or the `admin*.html` templates are Forge-provided and intentionally styled
  separately from the public site — leave them exactly as they are. Your
  restyling of `app.css` only affects the public pages.
- **Crossing into `/admin` must be a native link, not a boosted one.** Because
  `/admin` is served with `admin.css` and the public site with `app.css`, an
  hx-boost link between them swaps only the `<body>` and keeps the old `<head>`,
  so the destination loads with the wrong stylesheet (unstyled) until a manual
  reload. The starter sets `hx-boost="false"` on the nav's "Site admin" link and
  on admin's "View site" link for exactly this reason — keep it there, and add
  it to any link you introduce that crosses the public-site ↔ `/admin` boundary.
- Keep: semantic HTML, accessibility (contrast, focus states, labels), and the
  responsive behavior. Beauty never trumps usability.
- **Record the chosen direction in `DESIGN.md`** (palette, type, spacing, voice)
  before you build the UI, then build to it.

### Avoid the AI-generated look (these read as "made by a bot")

- **No purple/violet gradients or cyan-on-dark** — the single biggest AI tell.
  Choose a distinctive, intentional palette that fits the business.
- **Don't default to Inter/Roboto** — pick type with character that suits the brand.
- No bounce/elastic/overshoot easing; use calm, natural motion (or none).
- No nested cards, side-tab accent borders, or dark drop-glows.
- Never gray text on a colored background (contrast + it looks cheap).
- Generous padding and whitespace — cramped layouts feel unfinished.
- Tap targets ≥ 44px; comfortable line length (~45–75 chars); never skip
  heading levels (h1 → h2 → h3).
- **Mobile nav is a hamburger, always.** Any site with more than ~2 nav links
  MUST collapse them into an accessible expandable menu on phones (a row of
  cramped links, or links that wrap under the logo, reads as unfinished — the
  most common tell). The starter ships a working CSS-only pattern in
  `layout.html` + `app.css` (`.nav` / `.nav-toggle` / `.nav-burger` / `.navlinks`,
  toggled below 720px): reuse it for the public nav — restyle it, but keep the
  collapse behavior. It needs no JavaScript and survives hx-boost swaps. Always
  check the nav at a 375px width before deploying.

## Rules

- Keep `/healthz` working — the platform health check depends on it.
- Keep auth, CSRF and the session model intact; extend, don't weaken.
- **Data:** model domain data as proper typed tables (a migration per change).
  Plain rowid tables only — never `WITHOUT ROWID` (the admin and hooks key on
  rowid). Do NOT build owner dashboards, data lists, or admin pages — `/admin`
  already renders every table; just insert the data.
- Don't name columns with `password`/`hash`/`token`/`secret` unless the value
  is genuinely secret — the admin masks such columns.
- Stdlib only unless the plan clearly needs more; no JS frameworks by default.
- Validate and length-cap all user input (see `maxFieldLen`).
- Run `go test ./...` and `go vet ./...` before deploying — code must compile
  and tests must pass. But note: the starter's `web_test.go` asserts the
  SCAFFOLD's exact behavior (specific pages, confirmation text, which tables
  `/admin` lists). When you deliberately change that behavior, those assertions
  are now WRONG — update or delete the obsolete ones to match what you actually
  built, in ONE pass. Do NOT loop re-editing tests to satisfy stale assertions,
  and do NOT weaken auth/CSRF/persistence coverage. Unit tests are a compile +
  invariant check; the browser smoke test above is the real functional gate —
  don't rabbit-hole here.
- **Test every user path in a real browser before deploying — required.** Unit
  tests and `curl`/health checks run no JavaScript, so they miss broken
  htmx/form/redirect flows (the #1 "I click the button and nothing happens"
  bug). To (re)start the app locally, run ONE command — do NOT improvise the
  process/port/data-dir lifecycle:

      ./scripts/serve.sh        # builds, (re)starts on :8080, waits for health

  Run it again any time you change code: it kills the old instance HARD, frees
  the port, wipes the throwaway data dir, rebuilds, restarts detached, and prints
  "app ready …" when up (owner account owner@test.local / ownerpass123). On a
  compile error it prints it — fix and re-run. Read /tmp/forge-app.log if it
  won't come up.

  **NEVER debug ports or processes** with `ps` / `lsof` / `fuser` / `ss` /
  `netstat` / `kill` — that hand-management is the single biggest time sink and
  is banned. If a start ever fails or ":8080 is busy", the ONLY correct response
  is to run `./scripts/serve.sh` again — its SIGKILL frees the port every time.
  Do not hunt for stray processes.

  Signing up with owner@test.local creates the first (owner/admin) account. Then
  run the PROVIDED smoke test (run it, don't rewrite it) — it walks signup /
  login / logout / admin and prints PASS/FAIL:

      node scripts/smoke.js http://localhost:8080 owner@test.local ownerpass123

  Every check must PASS before deploy; a FAIL is a real bug — **FIX the reported
  issue and RE-RUN `smoke.js`.** smoke.js already covers auth, admin styling and
  nav, so do NOT write your own scripts to re-verify those flows. (`scripts/` is
  test-only tooling — do not deploy it or edit `smoke.js`/`serve.sh`.)
  Once smoke.js is green, test the plan's SITE-SPECIFIC flow it can't know about
  (a booking, a custom form) with the PROVIDED declarative runner — do NOT
  hand-roll a Playwright script (that is the #1 time sink). Write a small steps
  JSON and run it:

      node scripts/flow.js http://localhost:8080 /tmp/flow.json

  Steps are declarative — `{"signupOwner":"owner@test.local"}`,
  `{"login":"..."}`, `{"goto":"/book"}`, `{"fill":{"select[name='space']":"Hot
  desk"}}`, `{"click":"button[type=submit]"}`, `{"expect":"Booking confirmed"}`,
  `{"expectUrl":"/bookings"}`. See the header of `scripts/flow.js` for the full
  list. It handles the browser, login, waits and assertions for you, so there is
  nothing to debug — just get the SELECTORS and expected text right. If a step
  FAILS, fix the app (not the flow file) and re-run. One flow file for the key
  path is enough — don't build a parallel Playwright harness.

## Build, test, deploy

    make run        # local dev on :8080
    make test       # tests
    fly deploy --remote-only --ha=false --app "$FLY_APP" --access-token "$FLY_DEPLOY_TOKEN"

`--ha=false` is required: the app uses SQLite on a single machine — two
machines would mean two diverging databases. The `[mounts]` block in fly.toml
provisions a durable volume at `/data` automatically on first deploy and keeps
the database across redeploys — **never remove it**, or customer data resets on
every deploy. The Dockerfile and fly.toml in this repo are already correct;
don't rewrite them unless the plan demands it.
