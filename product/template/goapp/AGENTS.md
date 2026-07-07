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
Design section with the customer's chosen direction — implement *that*:

- Restyle `static/app.css` completely: palette, typography, spacing, layout.
  Replace the CSS variables and go far beyond them if the direction calls for
  it. Nothing about the starter's look should survive unless it happens to fit.
- Redesign the landing page structure freely (hero, sections, imagery).
- **Do NOT touch the site admin's look**: `static/admin.css`, `admin_layout.html`,
  or the `admin*.html` templates are Forge-provided and intentionally styled
  separately from the public site — leave them exactly as they are. Your
  restyling of `app.css` only affects the public pages.
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
- Run `make test` (or `go test ./...`) and `go vet ./...` before deploying;
  fix what they find.

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
