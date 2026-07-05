# Transcend Forge Go starter

The starting point for every app Transcend Forge builds: **one Go binary**
serving frontend and backend — server-rendered HTML, embedded templates/assets,
SQLite persistence, and working auth.

Included out of the box:

- Signup / login / logout (bcrypt, DB-backed sessions with hashed tokens, CSRF)
- First account created = site owner (`is_admin`)
- Public contact form → messages inbox on the owner's `/app` dashboard
- Embedded, numbered SQL migrations
- `/healthz`, graceful shutdown, Dockerfile + fly.toml (auto-stop, 1 machine)

Run locally:

    make run      # http://localhost:8080, data in ./data
    make test

Build agents: read [`AGENTS.md`](AGENTS.md) before changing anything.

Deploy (one machine — SQLite):

    fly deploy --remote-only --ha=false --app "$FLY_APP" --access-token "$FLY_DEPLOY_TOKEN"

Previews run without a volume (`/data` on the machine's rootfs: survives
stop/start, resets on redeploys). At go-live, create a volume and add the
`[mounts]` section shown in `fly.toml`.
