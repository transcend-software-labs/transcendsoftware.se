# Transcend Forge — product

The customer-facing application for `app.transcendsoftware.se`: a landing page,
login, and a dashboard where a customer describes a website and an autonomous
agent plans, builds, and deploys it — with every result reviewed and guaranteed
by a human (Rasmus) before go-live.

This is a separate deploy target from the marketing site (`transcendsoftware.se`,
the Astro project at the repo root) but lives in the same monorepo.

## Status

The full customer-facing flow is implemented and tested **except the actual Fly
deploy**, which is deliberately switched off (`fly.ErrDeployDisabled`). In dev
mode everything runs locally with fakes, so the whole experience works without
any secrets.

What works today:

- Landing page + email/password auth (bcrypt, cookie sessions, **CSRF-protected** forms)
- Start a project → orchestrator runs **intake (clarifying questions) → plan →
  safety gate → build**
- Clarifying-question step before building (the PO-level questions)
- Live status via HTMX polling; preview link attached on completion
- Two reiterations per project (1 initial build + 2 changes)
- Safety gate rejects abuse and **escalates** ambiguous requests to an
  operator/admin review queue (`/admin`, gated by `ADMIN_EMAIL`)
- In-memory store (dev) **and** Postgres (both validated end-to-end)
- Health check (`/healthz`), graceful shutdown, single static binary
- Deploy config for the product itself (`Dockerfile`, `fly.toml`) — not deployed

Real build mode (`FLY_API_TOKEN` + `FLY_SANDBOX_APP`/`FLY_SANDBOX_IMAGE` set):
the orchestrator spawns a per-task Fly Machine from the sandbox image, injects
env (incl. `ANTHROPIC_API_KEY` so opencode can call Claude — the one credential
that must be in the sandbox; the deploy token stays out), waits for it to start,
and drives opencode at its private address. The spawn/destroy path is validated
against the live Fly API (`internal/fly/integration_test.go`, gated behind
`FLY_SMOKE=1`). **The orchestrator must run on the Fly private (6PN) network** to
reach the sandbox.

Not yet built (by design): the payment gate and the real Fly **deploy** (still
`fly.ErrDeployDisabled`). Live opencode token-level streaming is a refinement
(the driver currently reports start + final result).

## Run it

Dev mode — no database, no API key, no opencode, no Fly:

```sh
make run            # http://localhost:8080
```

Against Postgres:

```sh
make db-up          # start local Postgres in Docker
make db-migrate     # apply migrations/0001_init.sql
make run-pg
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
| `ANTHROPIC_API_KEY`   | use the real planner + safety gate (else a deterministic fake) |
| `ANTHROPIC_MODEL`     | override the model (default `claude-sonnet-4-6`)           |
| `OPENCODE_URL`        | drive a real opencode server (else simulated build)        |
| `FLY_API_TOKEN`       | use the real Fly Machines client (deploy still gated)      |
| `FLY_SANDBOX_APP`     | Fly app the per-task sandbox machines run under            |
| `FLY_SANDBOX_IMAGE`   | OCI image with opencode + toolchains                       |
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
- `fly` — Fly Machines client: spawn/destroy sandboxes; deploy is gated
- `builder` — one build pass: spawn sandbox → run agent → deploy → teardown
- `orchestrator` — async pipeline driving a project through its lifecycle
- `web` — HTTP handlers, templates (`templates/`), assets (`static/`)

## Trust model

The pipeline is built around a trusted/untrusted split:

- **Untrusted:** the customer's brief and the agent that acts on it. Each build
  runs in an isolated per-task sandbox (a Fly Machine microVM; in dev, a fake).
- **Trusted:** the orchestrator and real credentials live outside the sandbox.
  The agent never holds the Fly/deploy token — it asks; the orchestrator
  performs the deploy after a policy check.
- The **safety gate is tool-less**: it only classifies, so a jailbreak of it
  yields a bad verdict, never an action.

Before exposing this publicly, also wire: real sandbox provisioning with an
egress allowlist, per-task scoped Fly tokens, and the human review + payment
gate. See the project notes for the full plan.
