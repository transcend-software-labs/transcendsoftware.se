# Transcend Forge — product

The customer-facing application for `app.transcendsoftware.se`: a landing page,
login, and a dashboard where a customer describes a website and an autonomous
agent plans, builds, and deploys it — with every result reviewed and guaranteed
by a human (Rasmus) before go-live.

This is a separate deploy target from the marketing site (`transcendsoftware.se`,
the Astro project at the repo root) but lives in the same monorepo.

## Status

**Deployed and proven end-to-end** at `transcend-forge.fly.dev`: a real customer
flow has run brief → intake questions → plan → safety gate → sandboxed build →
agent-run `fly deploy` → live, verified preview on `forge-<projectId>.fly.dev`.
In dev mode everything runs locally with fakes, so the whole experience works
without any secrets.

See [`PLAN.md`](PLAN.md) for the review verdict, the plan to completion, and
the open decisions.

What works today:

- Landing page + email/password auth (bcrypt, cookie sessions, **CSRF-protected** forms)
- Start a project → orchestrator runs **intake (clarifying questions) → plan →
  safety gate → build**
- **Live build streaming**: the dashboard shows the agent's tool activity as it
  happens (opencode SSE `/event` → broker → HTMX SSE)
- **Deploy verification**: the preview URL is smoke-checked (HTTP 200 +
  non-empty body) before a project is marked preview-ready — never asserted
- **Workspace snapshots**: reiterations restore the previous build's
  `/workspace` (presigned URLs, orchestrator-driven via Machines exec), so
  changes edit the same site instead of rebuilding from scratch
- **Starter template** ([`template/goapp`](template/goapp)): first builds seed
  the workspace with a single-binary Go app (server-rendered FE+BE, SQLite,
  auth, contact-form inbox) that the agent extends per `AGENTS.md` — enabled
  via `TEMPLATE_KEY`; customer apps deploy with `--ha=false` (one machine)
- Two reiterations per project (1 initial build + 2 changes); a failed change
  falls back to the still-live previous preview and consumes no credit
- Safety gate rejects abuse and **escalates** ambiguous requests to an
  operator/admin review queue (`/admin`, gated by `ADMIN_EMAIL`)
- Crash recovery: interrupted builds are reaped on startup; heartbeats + logs
  persisted per iteration
- In-memory store (dev) **and** Postgres (Postgres not yet provisioned in prod
  — see PLAN.md §3.3)
- Health check (`/healthz`), graceful shutdown, single static binary

Real build mode (`FLY_API_TOKEN` + `FLY_SANDBOX_APP`/`FLY_SANDBOX_IMAGE` set):
the orchestrator spawns a per-task Fly Machine from the sandbox image, injects
env (the LLM key for opencode, plus `FLY_APP`/`FLY_DEPLOY_TOKEN` so the agent
can deploy), waits for opencode to accept connections, and drives it at its
private address. **The orchestrator must run on the Fly private (6PN) network**
to reach the sandbox.

Not yet built (by design): the payment gate. Known hardening TODO: the deploy
token in the sandbox is org-scoped; per-app minting is blocked on a
token-authority decision (PLAN.md §5.3).

## Run it

Dev mode — no database, no API key, no opencode, no Fly:

```sh
make run            # http://localhost:8080
```

Against Postgres:

```sh
make db-up          # start local Postgres in Docker
make db-migrate     # apply all migrations/ in order (idempotent)
make run-pg
```

Asset uploads against local MinIO (S3-compatible, like Tigris in prod):

```sh
docker compose up -d minio   # MinIO API :9000, console :9001
STORAGE_ENDPOINT=localhost:9000 STORAGE_ACCESS_KEY=forge \
  STORAGE_SECRET_KEY=forge-secret STORAGE_BUCKET=forge-assets make run
```

Test / vet:

```sh
make test
make vet
```

## Configuration

Everything is environment-driven. **With nothing set, the app is fully in dev
mode.** Each variable independently switches one piece from fake to real:

| Env var               | Effect when set                                             |
|-----------------------|------------------------------------------------------------|
| `DATABASE_URL`        | use Postgres instead of the in-memory store                |
| `ADMIN_EMAIL`         | the account allowed into the operator review views (`/admin`) |
| `MAX_PROJECTS_PER_DAY` | per-user daily project cap (default 3)                    |
| `MAX_CONCURRENT_BUILDS` | global concurrent build cap (default 3)                  |
| `PREVIEW_TTL_DAYS`    | days an untouched preview app stays up before the reaper destroys it (default 14) |
| `TEMPLATE_KEY`        | object-storage key of the starter-app tarball seeding first builds (empty → greenfield); push with `make template-push` |
| `RESEND_API_KEY`      | send real email via Resend (else notifications are log-only) |
| `EMAIL_FROM`          | verified sender for outgoing email                         |
| `ANTHROPIC_API_KEY`   | use the real planner + safety gate (else a deterministic fake) |
| `ANTHROPIC_MODEL`     | override the model (default `claude-sonnet-4-6`)           |
| `LLM_API_KEY`         | OpenAI-compatible model for intake/plan/gate **and** the sandbox agent (takes precedence over Anthropic) |
| `LLM_BASE_URL` / `LLM_MODEL` | provider base URL (default Moonshot) and model (default `kimi-k2.7-code`) |
| `OPENCODE_URL`        | drive a fixed opencode server (else per-sandbox over 6PN, else simulated) |
| `FLY_API_TOKEN`       | real Fly Machines client (spawn sandbox, create per-customer app) |
| `FLY_ORG`             | Fly org slug for per-customer app creation                 |
| `FLY_DEPLOY_TOKEN`    | deploy-scoped token injected into the sandbox for `fly deploy` |
| `FLY_SANDBOX_APP`     | Fly app the per-task sandbox machines run under            |
| `FLY_SANDBOX_IMAGE`   | OCI image with opencode + toolchains                       |
| `STORAGE_ENDPOINT`    | S3-compatible object storage for asset uploads — `localhost:9000` (MinIO) or Tigris host; empty → in-memory dev store |
| `STORAGE_ACCESS_KEY` / `STORAGE_SECRET_KEY` | storage credentials (orchestrator only — never the sandbox) |
| `STORAGE_BUCKET` / `STORAGE_REGION` / `STORAGE_USE_SSL` | bucket (default `forge-assets`), region, TLS |
| `ADDR`                | listen address (default `:8080`)                           |
| `BASE_URL`            | public base URL                                            |
| `SECURE_COOKIE=true`  | mark the session cookie `Secure` (set behind HTTPS)        |

## Architecture

```
Browser ──► web (landing, auth, dashboard, /admin; HTMX, CSRF)
              │  start project
              ▼
          orchestrator ── intake ─► llm.Intake       (clarifying questions)
              │          ── plan ──► llm.Planner      (Anthropic | fake)
              │          ── gate ──► llm.SafetyGate   (tool-less; Anthropic | fake)
              │                          └─ escalate → /admin operator review
              │          ── build ─► builder.Sandbox
              │                          ├─ fly.Machines   spawn microVM / teardown
              │                          └─ opencode.Driver run the agent build
              ▼
            store (Postgres | in-memory)
```

The agent's operating spec, Fly deploy runbook, and intake playbook live in
[`agent/`](agent/) — the "Rasmus's decisions" the build sandbox mounts.

Packages (`internal/`):

- `project` — domain types + the lifecycle state machine
- `store` — `Store` interface, `Memory` (dev) and `Postgres` (pgx) impls
- `auth` — bcrypt + in-memory cookie sessions
- `llm` — `Planner` + `SafetyGate`, with the Anthropic client and a Fake; the
  operating spec ("Rasmus's decisions") lives in `PlannerSystemPrompt`
- `opencode` — driver to run a build via an opencode server (HTTP + Fake)
- `fly` — Fly client: spawn/destroy sandboxes, exec inside them, create per-customer apps
- `builder` — one build pass: spawn sandbox → restore snapshot → run agent → save snapshot → teardown
- `orchestrator` — async pipeline driving a project through its lifecycle
- `web` — HTTP handlers, templates (`templates/`), assets (`static/`)

## Trust model

The pipeline is built around a trusted/untrusted split:

- **Untrusted:** the customer's brief and the agent that acts on it. Each build
  runs in an isolated per-task sandbox (a Fly Machine microVM; in dev, a fake).
- **Trusted:** the orchestrator holds the real credentials (Fly org API token,
  storage keys). App creation, snapshot restore/save, and deploy verification
  all run on this side.
- **Inside the sandbox** (what a compromised build could leak): the LLM API key
  and a Fly **deploy token** — currently **org-scoped**, so a prompt-injected
  agent could deploy to any app in the org. Per-app minting is a documented
  hardening TODO blocked on a token-authority decision (PLAN.md §5.3).
- **Storage is never credentialed in the sandbox**: assets arrive and snapshots
  travel via short-lived presigned URLs only.
- The **safety gate is tool-less**: it only classifies, so a jailbreak of it
  yields a bad verdict, never an action.

Before exposing this publicly: quotas, preview-app lifecycle, credential
rotation, and the human review + payment gate — the ordered list lives in
[`PLAN.md`](PLAN.md).
