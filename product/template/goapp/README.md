# Transcend Forge Go starter

The starting point for every app Transcend Forge builds: **one Go binary**
serving frontend and backend — server-rendered HTML, embedded templates/assets,
SQLite persistence, and working auth.

Included out of the box:

- Signup / login / logout (bcrypt, DB-backed sessions with hashed tokens, CSRF)
- First account created = site owner (`is_admin`)
- Public contact form → messages inbox on the owner's `/app` dashboard
- Embedded, numbered SQL migrations
- Cached static assets (ETag + versioned URLs) and native CSS view transitions
  — navigation is plain links and forms, no client-side routing layer
- Fluid type and spacing, accessible mobile navigation, focus states, 44px touch
  targets, reusable composition primitives, and a discreet owner-login path
- Automatic WebP conversion and responsive image variants for PNG/JPEG assets
- Rendered desktop/mobile quality gates for accessibility, metadata, layout,
  image semantics and payload budgets, plus a visual design-director review
- One typed path for client JS: `web/src/app.ts` (strict TypeScript, compiled
  by `make js` via esbuild's Go API — empty by default)
- `/healthz`, graceful shutdown, Dockerfile + fly.toml (auto-stop, 1 machine)

Run locally:

    make run      # http://localhost:8080, data in ./data (images + app.ts first)
    make test     # optimizes images, builds/type-checks app.ts, then go test

Build agents: read [`AGENTS.md`](AGENTS.md) before changing anything.

Deploy (one machine — SQLite):

    fly deploy --remote-only --ha=false --app "$FLY_APP" --access-token "$FLY_DEPLOY_TOKEN"

Previews run without a volume (`/data` on the machine's rootfs: survives
stop/start, resets on redeploys). At go-live, create a volume and add the
`[mounts]` section shown in `fly.toml`.
