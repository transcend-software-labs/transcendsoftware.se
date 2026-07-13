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
    internal/web/static/        tokens.css (design vars) + components.css (locked) + app.css (theme)

## How to make common changes

- **New page:** add `templates/<name>.html` (use `{{template "head" .}}` /
  `{{template "foot" .}}`), a handler in `handlers.go`, a route in `Handler()`.
- **New table/column:** add `internal/db/migrations/000N_<what>.sql` (next
  number). Never edit an already-numbered file.
- **Authed page:** wrap the handler with `s.requireUser(...)`.
- **Any authenticated POST:** include the hidden `csrf_token` input (see the
  logout form) and call `s.checkCSRF(r)` first.
- **Content/branding:** replace "Your Site" in `layout.html`, the landing page,
  and the `static/tokens.css` variables. Write real, complete copy in the customer's
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

- **The CSS is a three-file system — know which file does what:**
  - `static/tokens.css` — the design surface. Set the plan's palette (as the
    semantic color tokens, incl. `--accent-ink` = readable text ON the accent),
    the display+body type pairing, radius, and the spacing/rhythm feel. Setting
    tokens alone re-skins every component. Nothing of the starter's neutral
    look should survive.
  - `static/components.css` — **LOCKED, do not edit** (the design audit
    verifies its hash and fails the build if it changed). It is structure only:
    nav + hamburger, container/section rhythm, footer insets, forms, readable
    buttons. It has no look of its own — everything visual comes from tokens.
  - `static/app.css` — yours, freeform. Hero art direction, section bands,
    cards, imagery treatment, the signature element. Compose with
    `.section`/`.container` so insets stay consistent, and take every
    padding/margin/gap from the `--space-*` scale.
- Buttons: restyle variants via custom properties only —
  `.btn-secondary { --btn-bg: …; --btn-ink: …; }`. Never raw `color:` rules on
  buttons; the component pins button ink so section link-colors can't make
  button text invisible (a bug that shipped once — never again).
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
- Keep semantic HTML and the responsive behavior. The **Interface quality floor**
  below (keyboard/focus, forms, contrast, every-state, motion) is the
  non-negotiable bar under every design — beauty never trumps usability.
- **The site's language drives more than copy**: set `<html lang="…">` in
  `layout.html` to the site's language, and pick typefaces that actually cover
  its script — many display faces are Latin-only, so for Cyrillic, Greek, etc.
  verify the display font has the glyphs (or use a well-covered face / system
  stack for headings). Tofu or silent fallback in headings is a shipped bug.
- **Record the chosen direction in `DESIGN.md`** (palette, type, spacing, voice)
  before you build the UI, then build to it.

### Avoid the AI-generated look (these read as "made by a bot")

- **No purple/violet gradients or cyan-on-dark** — the single biggest AI tell.
  Choose a distinctive, intentional palette that fits the business.
- **Don't default to Inter/Roboto** — pick type with character that suits the brand.
- No bounce/elastic/overshoot easing; use calm, natural motion (or none).
- No nested cards, side-tab accent borders, or dark drop-glows.
- Never gray text on a colored background (contrast + it looks cheap).
- Generous, CONSISTENT padding — every padding/margin/gap you write comes from
  the `--space-*` scale in tokens.css, applied evenly. The locked components
  already give sections and the footer the shared horizontal inset
  (`--container-pad`) and symmetric vertical rhythm (`--section-pad-y`) — keep
  any CSS you add on that same system; content must never sit flush against a
  container or viewport edge.
- Tap targets ≥ 44px; comfortable line length (~45–75 chars); never skip
  heading levels (h1 → h2 → h3).
- **Mobile nav is a hamburger, always.** Any site with more than ~2 nav links
  MUST collapse them into an accessible expandable menu on phones (a row of
  cramped links, or links that wrap under the logo, reads as unfinished — the
  most common tell). The starter ships a working CSS-only pattern in
  `layout.html` + `components.css` (`.nav` / `.nav-toggle` / `.nav-burger` / `.navlinks`,
  toggled below 720px): reuse it for the public nav — restyle it, but keep the
  collapse behavior. It needs no JavaScript and survives hx-boost swaps. Always
  check the nav at a 375px width before deploying.
- **Login must stay reachable.** The starter serves /login and /signup on every
  site, and the starter nav's `{{if .User}}` block links them. When you redesign
  the header, KEEP that block (restyled however you like) — or link /login from
  the footer. A site whose auth pages exist but are linked from nowhere fails
  the audit (`orphaned-auth-page`): the owner literally cannot find their own
  login. If the site takes orders/bookings, "Log in" belongs in the nav.

### Interface quality floor — every page clears these

The distinctive look is per-project; THIS is the non-negotiable bar under all of
them (adapted from Vercel's web interface guidelines). A beautiful page that
fails these is not done. Walk this list before you deploy.

- **Keyboard & focus.** Everything interactive is reachable and operable by
  keyboard (Tab, Enter, Space). Every focusable element shows a visible
  `:focus-visible` ring — never `outline: none` without a clear replacement.
  Use real elements: a link is `<a>`, a button is `<button>` (never a clickable
  `<div>`). Hit targets ≥ 24px on desktop, ≥ 44px on mobile; put
  `touch-action: manipulation` on buttons/links to kill the double-tap zoom
  delay. Never disable browser zoom; never block paste in a field.
- **Forms are the money paths** (order, booking, contact, login). Every field
  has a real `<label>` tied to it (clicking the label focuses the input). Set
  the right `type` and `inputmode` (`email`, `tel`, `numeric`, …), an
  `autocomplete` value, and `spellcheck="false"` on emails/codes/usernames.
  Mobile inputs render at **≥ 16px** font (smaller makes iOS auto-zoom the
  page). Enter submits a single-field form. Keep the submit button ENABLED
  until submission starts — don't pre-disable it on "invalid"; instead show the
  error next to the offending field and move focus to the first one. Placeholders
  are example values ending with "…" (`e.g. you@example.com`), not instructions.
- **Copy & typography.** Curly quotes (“ ” ‘ ’) and the ellipsis character (…),
  never straight quotes or `...`. Each page sets an accurate, specific
  `<title>`. Put `font-variant-numeric: tabular-nums` on any run of numbers
  (prices, quantities, tables) so they align. Body copy stays a comfortable
  measure (~45–75 characters).
- **Never signal with color alone.** A status (available / sold out, open /
  closed, error) always pairs the color with text or an icon. Icon-only buttons
  carry an `aria-label`; purely decorative icons/SVGs get `aria-hidden="true"`.
- **Design every state, not just the happy path.** Empty (no items yet), loading,
  error, and long-content states all get a deliberate design. No dead ends —
  every screen offers a next step or a way back. The layout must survive both a
  3-character and a 500-character value without breaking (test a long name / a
  long note).
- **Visual finish.** Minimum AA contrast everywhere (never gray text on a colored
  panel). Interaction states (`:hover`, `:active`, `:focus`) INCREASE contrast
  vs the resting state. Nested corners: a child's `border-radius` ≤ its parent's.
  Tint borders and shadows toward the surface's own hue rather than pure black.
  Set `<meta name="theme-color">` and `color-scheme` on `<html>` so the browser
  chrome matches the page.
- **Motion.** Honor `prefers-reduced-motion` — wrap non-essential animation in
  `@media (prefers-reduced-motion: no-preference)` (or disable it under
  `reduce`). Animate only `transform` and `opacity`; never `transition: all`.
  Motion responds to user action — no autoplaying loops.

## Rules

- Keep `/healthz` working — the platform health check depends on it.
- Keep auth, CSRF and the session model intact; extend, don't weaken.
- **Data:** model domain data as proper typed tables (a migration per change).
  Plain rowid tables only — never `WITHOUT ROWID` (the admin and hooks key on
  rowid). Just INSERT the data — that is your whole job for it.
- **NEVER build an owner/staff admin, dashboard, CRUD, data-list or record-edit
  UI.** The built-in `/admin` already lists, filters, shows, deletes and exports
  EVERY table by introspection. Writing your own `admin_*.html` pages to manage
  clients, inquiries, orders, bookings or "statuses" is off-limits, wasted work,
  and the #1 build-time sink (you then have to hand-test a whole CRUD). If the
  plan says the owner "manages", "reviews" or "updates" data, that already happens
  in `/admin` — do not build it. (Customer-FACING read pages — e.g. a member
  seeing their own booking — are fine; the ban is on owner/staff management UIs.)
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

- **Do NOT hand-test flows with raw `curl` logins/POSTs, cookie jars, or
  `sqlite3`/`python3` DB pokes.** That manual loop (log in by hand, POST a form,
  dump the DB to see what landed, repeat) is a massive time sink and is banned.
  smoke.js and flow.js already drive real auth + forms in a browser and tell you
  what broke — use them. If a flow feels too gnarly to express as a flow.js file,
  that is a signal the feature is over-built (see the admin rule above), not a
  reason to hand-roll curl. Trust the tools; fix the app, not the test harness.

- **Design audit — required before deploy.** With the app still running, audit the
  RENDERED site for contrast/quality defects:

      node scripts/audit.js

  It crawls your running pages, renders each, and runs the impeccable detector on
  the REAL assembled HTML — so it catches defects that live only in the composed
  page and never in a template file (a section rule like `.section-dark a`
  overriding a `.btn` colour so a button is invisible until hover; opacity making
  footer text too faint; low-contrast text). Auditing the template SOURCE misses
  all of these. Fix what it flags in the CSS and re-run until it prints `clean`.
  Do NOT edit `audit.js`.

- **Visual design review — see your own work before deploy.** audit.js is a
  linter; this is a pair of eyes. With the app still running:

      node scripts/design-review.js

  It screenshots your pages and a design director (a vision model) critiques the
  real rendered look — hierarchy, balance, whether it reads as intentionally
  designed or generic — judgments a linter can't make. If it says `POLISH`, apply
  the concrete fixes it lists (CSS/templates) and run it again; do at most two
  polish passes, then stop. If it prints that it's skipping (no vision model),
  just rely on audit.js. Do NOT edit `design-review.js`.

  **The local render is what ships.** Fly serves the same baked-in static files
  and the app seeds its own DB, so localhost is faithful to production — there's
  no separate "review it once it's live". The one thing that makes the review
  real: wire the customer's actual assets in first. Their uploaded and
  AI-generated images are already downloaded in `/workspace/assets/`; put the
  real logo and photos in place, then review — a review of a page still showing
  placeholders is worthless.

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
