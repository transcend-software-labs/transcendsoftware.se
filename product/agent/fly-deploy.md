# Fly deploy runbook

How the finished site gets published. **Split of privilege:**

- The **orchestrator** (trusted) creates the per-customer Fly app and mints a
  deploy token scoped to that one app. It does *not* put the org token anywhere
  near the sandbox.
- The **agent** (in the sandbox) runs the actual `fly deploy` for that one app,
  using the injected token. Blast radius if it leaks: that single throwaway app.

## What the orchestrator does (before the build)

1. `EnsureApp("forge-<projectId>")` — create the per-customer app.
2. Mint a deploy-scoped token and inject it, plus the app name, into the sandbox:
   - `FLY_APP=forge-<projectId>`
   - `FLY_DEPLOY_TOKEN=<deploy-scoped token>`

## What the agent does (end of the build)

Once the site builds and the verifier is green, publish it:

```sh
fly deploy --remote-only --app "$FLY_APP" --access-token "$FLY_DEPLOY_TOKEN"
```

The repo must contain a `Dockerfile` (or static output) and a `fly.toml` with:
- `primary_region` in the EU (`arn` Stockholm preferred, else `ams`/`cdg`)
- the right internal port and a health check

For data-backed sites, note the Postgres + Tigris resources needed so the
orchestrator can provision them.

The preview URL is deterministic: `https://<FLY_APP>.fly.dev`.

## Going live (after payment + Rasmus review)

1. Customer acquires a domain (`.se` via a registrar).
2. `fly certs add <domain>` and point DNS at the app.
3. Confirm TLS issued, then mark the project live.

## Hardening TODO

The injected token is currently a deploy-scoped token (limited to deploy
operations — no org admin or secret reads). Next step: mint it **scoped to
`FLY_APP` alone**, per task, and revoke after the build.
