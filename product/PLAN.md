# Transcend Forge — plan to completion

Reviewed 2026-07-05 on branch `rasmus-ai-product`, after a code review of the
orchestrator, builder, sandbox, Fly client, web layer, and the live deployment
(`transcend-forge.fly.dev`, release v3).

## Status update (2026-07-05, end of session)

Phase 1 **done**, Phase 2 **done** except three items that need Rasmus (DNS
repoint, a Resend key, credential rotation). Postgres + Tigris provisioned and
live. The core M2 promise — brief → verified preview → a real change that edits
the same site — was proven end-to-end through prod (see the reiteration line in
Phase 1). What's left for a paying stranger is Phase 3 (handover, payments,
ToS) plus those three Rasmus items.

## Verdict: right path, no pivot — but "worked once E2E" ≠ "sellable"

The architecture is sound and proven: Go + HTMX + Postgres-ready store, opencode
as the agent engine (model-agnostic via env), per-build Fly Machine sandboxes,
the trusted/untrusted split (orchestrator holds real credentials, sandboxes get
only scoped/presigned ones), tool-less safety gate, live SSE build streaming.
One real customer flow has run end-to-end: brief → intake questions → plan →
gate → sandboxed build → agent-run `fly deploy` → live preview site.

Nothing about that needs to change. What stands between this demo and a first
real customer is **completion work**, and the review found five gaps that are
not on any list yet — two of them design-level. They are the top of this plan.

## 1. What we're building (unchanged)

Non-technical customers describe a website at `app.transcendsoftware.se`; an
agent plans it, asks PO-level questions, builds it in an isolated sandbox,
deploys a live preview; the customer gets 2 rounds of changes; Rasmus personally
reviews and guarantees the result. Greenfield **websites** only (small → large,
DB + object storage fine) — not apps. EU hosting (Fly, arn) as a selling point.

**Non-goals for now:** app-style products, template system (joint session with
Rasmus pending), multi-tenancy/teams, public API, custom-domain automation,
multi-region, replacing opencode.

## 2. Where it stands

**Working, verified live:** full pipeline incl. Kimi intake/plan/gate, sandbox
spawning (IPv6 6PN, readiness polling), opencode driving, agent-run deploys,
live log streaming (SSE → dashboard), crash recovery (startup reaper +
heartbeats), escalation queue at `/admin`, asset upload UI, CSRF, dev mode
with fakes for everything.

**Deployed:** product at `transcend-forge.fly.dev` (v3). Sandbox image
`transcend-forge-sandbox:20260630-4`.

**Key numbers:** build ≈ 15 min with Kimi k2.7-code; 1 initial build + 2
reiterations per project (`MaxIterations = 3`).

## 3. What the review found (must change)

### 3.1 Reiterations don't actually work — P1, design gap
The "2 changes" promise is core to the offer, and it's currently broken by
design: `RepoURL` is plumbed everywhere (project, builder request, entrypoint
clone) but **nothing ever sets it** — the sandbox never pushes code anywhere,
and `builder.Build` just echoes `req.RepoURL` back. A reiteration therefore
spawns a **fresh, empty workspace** and instructs the agent to "apply this
change to the existing site" — there is no existing site. The agent would
rebuild something different from scratch; "make the hero bigger" yields a new
website.

**Fix (recommended):** workspace snapshots via object storage — at the end of
every successful build the sandbox tars `/workspace` and uploads it to a
**presigned PUT** URL (no storage creds in the sandbox, same pattern as asset
downloads); the next iteration's entrypoint restores from a presigned GET.
No new integrations. GitHub mirroring under `transcend-software-labs` stays a
Phase-4 nice-to-have for the code-review story, not the mechanism.

### 3.2 "Preview ready" is asserted, never verified — P1, design gap
`builder.go` constructs the preview URL (`https://forge-<id>.fly.dev`) and
marks the project preview_ready **without ever checking it**. If the agent's
`fly deploy` failed politely, the customer gets a dead link labeled "Preview
ready". Verification is literally the brand (the auto-verification positioning,
"I personally guarantee every result").

**Fix:** after the agent finishes, the orchestrator smoke-checks the preview
URL (HTTP 200 + non-trivial HTML body, with a retry window for machine start)
before `preview_ready`; otherwise the iteration fails with the log. Emit
"Verified live ✓" into the stream. Screenshot-into-admin-review is Phase 4.

### 3.3 Production data is all in-memory — P1, provisioning gap
Prod secrets include **no `DATABASE_URL` and no `STORAGE_*`**. Consequences,
verified live:
- Every deploy/crash deletes all users, projects, and sessions (sessions are
  a process-local map even with Postgres — needs its own fix).
- Customer photo uploads in prod presign to `memory://<key>` URLs that the
  sandbox's `curl` **cannot fetch** — uploads are silently non-functional.

The Postgres store + migrations and the S3/Tigris store already exist and are
tested; this is provisioning + wiring, not new code. Blocks everything else
(scaling past 1 machine, snapshots in 3.1, durable logs).

### 3.4 Cost & abuse leaks — P1 before anyone but Rasmus can use it
- **Preview apps never die:** every build creates a Fly app whose nginx machine
  runs 24/7 forever. Three `forge-*` apps are alive right now; the äppelmust
  demo still serves 200. The generated `fly.toml` (via `BuildSystemPrompt`)
  doesn't request auto-stop.
- **No quotas anywhere:** any signup can start unlimited builds → unlimited
  machine spend + LLM spend.
- **Org-wide deploy token in the sandbox:** a prompt-injected agent could
  deploy over (or scale/destroy) *any* app in the org. Per-app minting is
  blocked on a decision (§5.3) — the current API token can't mint sub-tokens.
- **One failed change bricks the project:** a failed reiteration marks the
  whole project `failed` (terminal) even though the previous preview is live
  and the customer had changes left. Must return to `preview_ready` + surface
  the failure.

### 3.5 The docs lie about the security model — P1, small but corrosive
`README.md`, `builder.go` (package comment + `Config` comment), and
`entrypoint.sh` all still claim "the deploy token stays out of the sandbox /
the orchestrator performs the deploy / app-scoped token". Since deploys were
ungated, an **org-scoped** `FLY_DEPLOY_TOKEN` *is* injected into every sandbox
and the *agent* deploys. `fly_http.go` now documents reality; the other three
must match. Wrong security docs are worse than none.

### 3.6 Nobody is told anything — P2
No email exists: Rasmus isn't notified on escalation or failure (must poll
`/admin`), and customers aren't notified when a ~15-minute build finishes
(they'll have left). Also: the customer's page doesn't auto-update while
`escalated`, so an admin approval goes unseen until manual refresh.

## 4. The plan

Milestones:
- **M1 — demo that works: done.**
- **M2 — pilot:** one friendly real customer goes brief → preview → 2 changes
  with nothing lost, nothing unverified, and Rasmus notified. (Phases 1–2)
  **Phase 1 done; Phase 2 done except email + DNS + credential rotation.**
- **M3 — sellable:** a stranger can pay, get a site, and have it handed over.
  (Phase 3)

### Phase 1 — make it real (M2 blockers)
- [x] Provision Fly Managed Postgres + run migrations + set `DATABASE_URL`:
      cluster `transcend-forge-db` (Basic, $38/mo) in **fra** (arn wasn't
      available for MPG; Frankfurt keeps EU residency, ~20ms from Stockholm).
      pgx set to exec query mode for the PgBouncer pooler. Live-verified:
      prod signup persists to the cluster, no pooler errors (§5.1) (S)
- [x] Sessions out of process memory: store-backed (memory in dev, Postgres in
      prod once provisioned), cookie token stored as SHA-256 hash, expired
      sessions swept on login; full SQL surface validated against local
      Postgres (migration 0003) (S)
- [x] Provision Tigris bucket + set `STORAGE_*`: bucket
      `transcend-forge-assets`. Whole storage layer (Put, presigned GET **and**
      PUT) smoke-tested against real Tigris — fixes prod asset uploads and
      unblocks the live snapshot path. Prod boots `storage: s3-compatible`
      (§5.2) (S)
- [x] **Workspace snapshots** (3.1): restore + save are orchestrator-driven via
      the Fly Machines exec API (validated live) with presigned GET/PUT URLs —
      no storage creds and no agent reliance; builder's dead `RepoURL`
      threading removed (`REPO_URL` in the entrypoint stays, reserved for
      Phase-4 GitHub mirroring) (M)
- [x] **Deploy verification** (3.2): smoke-check before `preview_ready`;
      "Verified live ✓" stream line; failed check ⇒ failed iteration (S)
- [x] Failed reiteration returns project to `preview_ready` (previous preview
      stands), not terminal `failed` — incl. the startup-recovery path; a
      failed attempt does not consume the change credit (S)
- [x] Fix the lying docs (3.5): README + fly.go + builder.go + entrypoint.sh
      now state the real security model (org-scoped deploy token in the
      sandbox, presigned-only storage); also fixed `make db-migrate` to apply
      all migrations, not just 0001 (S)
- [x] Reiteration snapshot path proven at every level: dev-fake tests, a live
      storage round-trip, **and a full live 2-build E2E through prod** (Kanelbullen
      bakery, 2026-07-05). Build 1 → coherent Swedish Astro site, verified live.
      Reiteration ("add a catering section, keep everything else") → prod log
      showed "Restoring your site from the previous build…" then the agent
      *reading* index.astro/Layout/Dockerfile and editing them. Result: catering
      section added (+997 bytes, not a rewrite), both incidental hero phrases
      preserved **verbatim**, all original sections intact. The "2 changes"
      promise works for real. (M2 core validated) (S)

### Phase 2 — safe to expose (M2 blockers)
- [x] Quotas: ≤3 projects/day/user (env `MAX_PROJECTS_PER_DAY`), one in-flight
      pipeline per user, global concurrent-build cap (env
      `MAX_CONCURRENT_BUILDS`, reiterations included), 4k char caps on
      brief/answers/change requests (S)
- [x] Preview lifecycle (M):
  - [x] generated `fly.toml` gets `auto_stop_machines` + `min_machines_running
        = 0` (BuildSystemPrompt) — previews cost ~nothing when idle
  - [x] hourly reaper: destroys preview apps of failed projects, expires
        previews idle past `PREVIEW_TTL_DAYS` (default 14; customer sees
        "Preview expired"), and sweeps sandbox machines older than 2h
  - [x] admin "Live previews" section with a destroy button (verified E2E in
        dev: list → destroy → project expired)
  - [x] one-off cleanup 2026-07-05: destroyed the three zombie test apps
        (forge-appelmust-demo, forge-06e6dbdd2900, forge-4d012fcb1bba)
- [x] Per-app deploy tokens (2026-07-06, live): the sandbox gets a token scoped
      to its own customer app, minted per build (2h expiry) via Fly's
      `createLimitedAccessToken` — verified against the live API that such a
      token 200s on its app, 403s on another. Graceful fallback to a configured
      org-scoped token (logged) if the runtime token can't mint. §5.3 resolved
      (M)
- [~] **Rotate the burned credentials** (S):
  - [x] Fly tokens (2026-07-06): prod FLY_API_TOKEN + FLY_DEPLOY_TOKEN now a
        fresh named 6-month org token ("forge-api-20260706", expires
        2027-01-06) — prod no longer uses the chat-pasted token. **Rasmus:**
        revoke the old unnamed "Organization Token"s in `fly tokens list -o
        transcend-software` if nothing else (terraform? hermes?) uses them.
  - [ ] Kimi key (Moonshot console), Resend key, Postgres/Tigris creds — need
        Rasmus's accounts.
- [x] **Per-app sandbox tokens, properly** (2026-07-06): org tokens can't mint
      sub-tokens (only user sessions can), so the GraphQL mint could never work
      from prod. Replaced with **local macaroon attenuation** (superfly/
      macaroon): prod derives a one-app, TTL-bound deploy token from its own
      API token by pure computation, mirroring official deploy-token caveats
      (incl. builder/wg features for remote builds). Verified live (200 own
      app, 403 others). Ships with the next code deploy.
- [x] Email (Resend): escalated → Rasmus, build failed → Rasmus, preview ready
      → customer. Interface + log-only fake (dev) + Resend impl; wired at all
      three lifecycle points, best-effort after state is persisted; tested with
      a recording notifier. **Live in prod 2026-07-06** (`notify: resend`).
      Caveat: no verified sender domain yet, so `onboarding@resend.dev` only
      delivers to the Resend account owner (Rasmus) — operator notices work;
      **customer "preview ready" emails need `transcendsoftware.se` verified in
      Resend + `EMAIL_FROM` updated** (M)
- [x] Escalated project page auto-updates after admin approval (slow 15s poll
      while held, fast 2s while building) (S)
- [~] `app.transcendsoftware.se`: Fly cert **created** (pending validation).
      **Decision + action for Rasmus:** the subdomain currently resolves to
      Hetzner (194.9.94.85/86) — repointing moves it to the product. Set one of:
      - A `app` → 66.241.125.213 **and** AAAA `app` → 2a09:8280:1::13a:bc93:0,
        or simply CNAME `app` → transcend-forge.fly.dev.
      Then `fly certs check app.transcendsoftware.se`. BASE_URL already set (S)
- [x] Failure-rate visibility: `/admin` shows a last-24h row — builds,
      succeeded, failed, in-flight, avg build duration (verified rendering with
      a completed build). Email-on-failure still to come (S)

### Phase 3 — sellable (M3)
- [x] Handover flow (2026-07-06, live): customer **Accept** on preview →
      `accepted` state in Rasmus's `/admin` "Ready for delivery" queue (emails
      him) → **Approve & deliver** → `delivered` (customer emailed "reviewed and
      guaranteed"), or **Send back** with a note → `preview_ready` with
      remaining changes. Nothing reaches delivered without his approval — the
      personal guarantee is now enforced by the state machine. Site stays on
      `forge-*`; custom domains still a manual service. Verified E2E over HTTP +
      browser (M)
- [ ] Payments at the accept-or-build step — trigger decision §5.4;
      implementation stays deferred until Rasmus says go (M)
- [ ] Pricing + ToS + privacy pages (GDPR/EU angle is the brand; mostly
      Rasmus's words) (S–M)
- [ ] Production build model — decision §5.5; env-only switch (S)
- [ ] Email verification at signup (before taking money) (S)
- [x] Retry-once on transient LLM/API failures in intake/plan/gate: the
      OpenAI-compatible (Kimi) client retries network blips, 429s, 5xx and
      empty 200s once; permanent 4xx are not retried. Tested (S). _(Anthropic
      fallback client not yet covered — prod path is Kimi.)_

### Added along the way (2026-07-06, built + tested, awaiting credentials to activate)
- [x] **GitHub source mirror + CI deploy** (Rasmus's direction): each build
      mirrors the project source to a private repo under
      `transcend-software-labs` (one commit per build → reviewable diffs) and
      writes `.github/workflows/deploy.yml` (flyctl deploy on push to main) with
      an encrypted app-scoped `FLY_API_TOKEN` secret. `internal/github`
      (interface+fake+REST). Validated the full REST flow against real GitHub.
      **Activate:** `GITHUB_TOKEN` (needs `repo` + **`workflow`** scopes) +
      `GITHUB_ORG`. Not deployed yet. (~14 throwaway test repos to delete.)
- [x] **Login: magic link + Google (LinkedIn-ready)** (Rasmus's direction):
      passwordless email login (single-use 20-min link, migration 0008) +
      provider-generic OAuth2 (`internal/oauth`); email/password kept as a
      collapsed fallback; accounts link by email. Tested (magic-link E2E, oauth
      unit). **Activate:** `GOOGLE_CLIENT_ID/SECRET` (redirect
      `<BASE_URL>/auth/google/callback`); magic-link delivery to *customers*
      also needs the Resend domain verified. **Hold prod deploy** of this until
      Resend domain is verified, else the magic-link-first login page can't
      actually email customers.

### Added along the way
- [x] **Per-project design choice** (2026-07-06, Rasmus's direction): intake
      suggests 2-3 tailored design directions; the customer picks one or
      states their own; the choice flows into the plan (## Design) and the
      build agent, which restyles the design-neutral starter completely.
      Stored via migration 0004. Verified in dev UI, then **live in prod 2026-07-06** — Kimi produced 3 distinct tailored directions for a real brief.
- [x] **Migrations run at startup** (2026-07-06, Rasmus's direction): embedded
      in the binary, applied by `store.NewPostgres` in one advisory-locked
      transaction, tracked in `schema_migrations`. Kills deploy-ordering
      footguns. Validated against local Postgres in all three states: legacy
      schema without tracking table (prod's state — backfills idempotently),
      re-boot (0 applied), fresh empty DB (full bootstrap + working signup). **Live in prod 2026-07-06**: boot log applied 0001-0004 (0004 first-time), health green.

### Phase 4 — better product (post-M3; several are joint sessions)
- [x] Project template/scaffold — built 2026-07-05 at Rasmus's direction
      (`template/goapp`): one Go binary serving FE+BE (embedded templates/
      assets/migrations), SQLite, auth + sessions + CSRF, first-account-is-
      owner, contact-form → owner inbox; AGENTS.md carries the conventions.
      Seeded into first builds via `TEMPLATE_KEY` (snapshots win on
      reiterations). Sandbox image `20260705-2` precompiles its dep tree +
      warms the cache at boot (agent-side `go build` ≈ 35s / `go test` ≈ 15s
      vs 12+ min cold). Customer deploys now `--ha=false` (one machine — with
      SQLite, two machines would be two diverging databases); the 2-machine
      E2E app was scaled down live. First live template build ran 2026-07-05
      (Snickare Lindqvist, ~16 min): agent read AGENTS.md first, extended the
      template (localized it to Swedish incl. flash messages and the owner
      dashboard), kept tests green with go test/vet loops, deployed
      --ha=false → **1 machine**, contact form → owner inbox verified on the
      live site. **Rasmus: taste review of the template still open.**
- [ ] GitHub mirroring under `transcend-software-labs` (the code-review story)
- [x] Screenshot into the admin review queue (2026-07-06, live): after each
      build the sandbox **crawls the deployed site and screenshots every page**
      (full-page, same-origin links, one Chromium session; playwright baked
      into sandbox image 20260706-1), uploading each to a presigned PUT slot
      (up to 8). `/admin` shows all pages labeled by path in the delivery
      review cards, first as the previews thumbnail (migrations 0005→0007).
      Validated live against a real 6-page site
- [ ] Custom-domain automation
- [ ] Scale product app past 1 machine (unblocked by Postgres)
- [x] Per-build cost + timing in `/admin` (2026-07-06, live): each build records
      tokens (from the opencode session) + duration (migration 0006); admin
      shows a per-build table (project, when, duration, tokens, est. machine
      cost, status) + 24h totals. Cost = machine-time at configurable
      `SANDBOX_COST_PER_HOUR`. Feeds the Kimi-vs-Claude call with real numbers

## 5. Decisions needed from Rasmus

1. **Postgres now?** Recommend **yes** — nothing is real until state survives
   a deploy; smallest managed plan is fine at this stage.
2. **Tigris bucket now?** Recommend **yes** — usage-priced (cents), and asset
   uploads in prod are currently silently broken without it.
3. **Token model:** to mint per-app, short-lived deploy tokens, the product app
   must hold an org-privileged token (as a Fly secret; it never enters
   sandboxes — the trusted/untrusted split is exactly for this). Recommend
   **yes**. The alternative — keeping the org-wide deploy token in every
   sandbox — should be an explicit, documented acceptance, not a default.
4. **Payment trigger:** recommend **pay-to-build with a money-back guarantee**
   (aligns with the personal-guarantee brand, filters tire-kickers, and the
   2-changes budget stays clean). Build it in Phase 3, not now.
5. **Build model for paid customers:** Kimi is cheap but slow (~15 min) and
   mid; quality is the product. Recommend **Claude Sonnet for builds** (and
   optionally Opus for the plan step) once a customer is paying; keep Kimi for
   dev. Already env-switchable.
6. **Snapshot vs GitHub for iteration persistence:** recommend **snapshots
   now** (no new integration, same presigned-URL security model), GitHub as
   Phase-4 mirroring.

## 6. Explicitly not doing now

App-style products; the template (until the joint session); teams/orgs; public
API; custom-domain automation; multi-region; swapping opencode; payment
implementation (until §5.4 is decided). Scope stays: **first real customer,
end to end, with nothing fake in the path.**
