# Transcend Forge — plan to completion

Reviewed 2026-07-05 on branch `rasmus-ai-product`, after a code review of the
orchestrator, builder, sandbox, Fly client, web layer, and the live deployment
(`transcend-forge.fly.dev`, release v3).

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
- **M3 — sellable:** a stranger can pay, get a site, and have it handed over.
  (Phase 3)

### Phase 1 — make it real (M2 blockers)
- [ ] Provision Fly Managed Postgres (arn) + run migrations + set
      `DATABASE_URL` — *needs OK, paid (§5.1)* (S)
- [x] Sessions out of process memory: store-backed (memory in dev, Postgres in
      prod once provisioned), cookie token stored as SHA-256 hash, expired
      sessions swept on login; full SQL surface validated against local
      Postgres (migration 0003) (S)
- [ ] Provision Tigris bucket + set `STORAGE_*` — fixes prod asset uploads —
      *needs OK, usage-priced (§5.2)* (S)
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
- [x] Reiteration test in dev fakes: snapshot key persisted after build 1,
      restore exec verified before build 2 (orchestrator + builder tests).
      The live run proving "change X" edits the *same* site is **blocked on
      Tigris** (§5.2) — presigned snapshot URLs need a real bucket (S)

### Phase 2 — safe to expose (M2 blockers)
- [x] Quotas: ≤3 projects/day/user (env `MAX_PROJECTS_PER_DAY`), one in-flight
      pipeline per user, global concurrent-build cap (env
      `MAX_CONCURRENT_BUILDS`, reiterations included), 4k char caps on
      brief/answers/change requests (S)
- [ ] Preview lifecycle (M):
  - [x] generated `fly.toml` gets `auto_stop_machines` + `min_machines_running
        = 0` (BuildSystemPrompt) — previews cost ~nothing when idle
  - [ ] reaper destroys preview apps N days after reject/fail/abandon
  - [ ] admin destroy button
- [ ] Per-app deploy tokens *or* explicitly accepted org-token risk — decision
      §5.3 (M)
- [ ] **Rotate the burned credentials:** the Kimi key and Fly tokens were
      pasted in chat and must be treated as compromised — mint fresh ones,
      update Fly secrets, revoke old (S)
- [ ] Email (Resend or SMTP): escalated → Rasmus, build failed → Rasmus,
      preview ready → customer (M)
- [x] Escalated project page auto-updates after admin approval (slow 15s poll
      while held, fast 2s while building) (S)
- [ ] `app.transcendsoftware.se`: DNS CNAME + `fly certs add` (BASE_URL is
      already set) (S)
- [ ] Failure-rate visibility: at minimum, email-on-failure covers it; add a
      daily "builds started/succeeded/avg duration" line to `/admin` (S)

### Phase 3 — sellable (M3)
- [ ] Handover flow: customer **Accept** on preview → Rasmus review gate in
      `/admin` (the personal guarantee, now enforced by the state machine) →
      `delivered`; site stays on `forge-*` under the Transcend org; custom
      domains as a manual service initially (M)
- [ ] Payments at the accept-or-build step — trigger decision §5.4;
      implementation stays deferred until Rasmus says go (M)
- [ ] Pricing + ToS + privacy pages (GDPR/EU angle is the brand; mostly
      Rasmus's words) (S–M)
- [ ] Production build model — decision §5.5; env-only switch (S)
- [ ] Email verification at signup (before taking money) (S)
- [ ] Retry-once on transient LLM/API failures in intake/plan/gate (S)

### Phase 4 — better product (post-M3; several are joint sessions)
- [ ] Project template/scaffold — **explicitly waiting to build together with
      Rasmus**; biggest lever on build time (~15 → ~5 min) and consistency
- [ ] GitHub mirroring under `transcend-software-labs` (the code-review story)
- [ ] Screenshot verification into the admin review queue
- [ ] Custom-domain automation
- [ ] Scale product app past 1 machine (unblocked by Postgres)
- [ ] Per-build cost tracking (machine-seconds + tokens) surfaced in `/admin`

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
