# Fly deploy runbook

How a built site gets published. The **agent does not hold the Fly token** — it
prepares the artifact and requests deploy; the orchestrator (outside the
sandbox) runs the privileged step with a per-task-scoped token.

## What the agent produces

1. A working build that passes the verifier (build + tests green).
2. A `Dockerfile` (or static output) that runs the site.
3. A `fly.toml` with:
   - `primary_region` in the EU (`arn` Stockholm preferred, else `ams`/`cdg`).
   - the right internal port and a health check.
4. For data-backed sites: a note of the Postgres + Tigris (object storage)
   resources required, so the orchestrator can provision them.

## What the orchestrator does (privileged, outside the sandbox)

1. Validate the request against policy (EU region, plan size/cost caps, naming).
2. Provision resources if needed:
   - `fly postgres create --region arn` (EU)
   - `fly storage create` (Tigris, EU) for object storage
3. Deploy: `fly deploy` with a **per-task-scoped** token, app named
   `proj-<projectID>`.
4. Capture the preview URL (`https://proj-<id>.fly.dev`) and attach it to the
   project.

## Going live (after payment + Rasmus review)

1. Customer acquires a domain (`.se` via a registrar).
2. `fly certs add <domain>` and point DNS at the app.
3. Confirm TLS issued, then mark the project live.

> NOTE: the actual `fly deploy` is deliberately gated in code today
> (`fly.ErrDeployDisabled`) until real deploys are switched on.
