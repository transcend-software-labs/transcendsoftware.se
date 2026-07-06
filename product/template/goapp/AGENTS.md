# Build agent conventions — Transcend Forge Go starter

You are extending a working, production-ready Go application. **Do not scaffold
a new project.** Read this file, then the code, then make the smallest change
that implements the plan.

## What this app already does

- One Go binary serves everything: server-rendered HTML (`html/template`),
  static assets, and all backend logic. Templates, CSS and migrations are
  **embedded** — the binary is the entire app.
- SQLite persistence in `$DATA_DIR` (default `data/`, `/data` in the container).
- Auth: signup/login/logout, bcrypt, DB-backed sessions (hashed tokens), CSRF
  helper. The **first account created is the site owner** (`is_admin`).
- Public contact form on `/` → stored in `messages` → shown to the owner on `/app`.
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
- Keep: semantic HTML, accessibility (contrast, focus states, labels), and the
  responsive behavior. Beauty never trumps usability.

## Rules

- Keep `/healthz` working — the platform health check depends on it.
- Keep auth, CSRF and the session model intact; extend, don't weaken.
- Stdlib only unless the plan clearly needs more; no JS frameworks by default.
- Validate and length-cap all user input (see `maxFieldLen`).
- Run `make test` (or `go test ./...`) and `go vet ./...` before deploying;
  fix what they find.

## Build, test, deploy

    make run        # local dev on :8080
    make test       # tests
    fly deploy --remote-only --ha=false --app "$FLY_APP" --access-token "$FLY_DEPLOY_TOKEN"

`--ha=false` is required: the app uses SQLite on a single machine — two
machines would mean two diverging databases. The Dockerfile and fly.toml in
this repo are already correct; don't rewrite them unless the plan demands it.
