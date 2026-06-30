# Build sandbox image

The per-task runtime for Transcend Forge builds. A fresh Fly Machine (Firecracker
microVM) boots from this image, runs the agent build + verification, then is
destroyed. The toolchain is baked in so **nothing installs at runtime**.

## Baked in
- Node + npm/pnpm, **Chromium + Playwright** (headless verification), Go, git
  (the Playwright base provides Node, Chromium, and all system libraries)
- **opencode** — the agent engine the orchestrator drives over HTTP

## Deliberately NOT included
- Deploy credentials and `flyctl`. The orchestrator performs the deploy *outside*
  the sandbox, so the untrusted environment never holds a real token.

## Per-task env (injected at Machine create)
| Env             | Purpose                                              |
|-----------------|------------------------------------------------------|
| `OPENCODE_PORT` | opencode server port (default 4096)                  |
| `SPEC_URL`      | URL to fetch the operating spec (`AGENTS.md`) fresh  |
| `REPO_URL`      | repo to work in for reiterations; empty = greenfield |
| `GIT_TOKEN`     | short-lived, repo-scoped clone token                 |

The image holds the **toolchain**; per-task data (plan, repo, spec, scoped env)
is injected at boot. The operating spec is fetched at runtime so editing the
agent's brain never requires an image rebuild.

## Build & push (to your Fly registry)

The app `transcend-forge-sandbox` already exists (Transcend Software org). It
hosts the image only — it runs no service. Build on Fly's remote builder (no
local 1.5 GB pull), from this directory:

```sh
fly deploy --build-only --push --remote-only --image-label $(date +%Y%m%d)
```

Or build locally:
```sh
fly auth docker
TAG=$(date +%Y%m%d)
docker build -t registry.fly.io/transcend-forge-sandbox:$TAG .
docker push registry.fly.io/transcend-forge-sandbox:$TAG
```

Then set `FLY_SANDBOX_APP=transcend-forge-sandbox` and
`FLY_SANDBOX_IMAGE=registry.fly.io/transcend-forge-sandbox:$TAG`. `fly.SpawnSandbox` creates
Machines in `FLY_SANDBOX_APP` from `FLY_SANDBOX_IMAGE`.

Or use the Make targets from `product/`: `make sandbox-build && make sandbox-push`.

## Project dependencies
The generated site's own dependencies still `npm install` per build — inherent,
since each site has its own `package.json`. If that becomes a bottleneck, mount a
pnpm store on a Fly volume to cache across builds.

## Version pinning
Pin the Playwright tag, Go version, pnpm, and opencode for reproducibility, and
bump them deliberately. The base image is large (~1.5 GB), so cold start is
dominated by the image pull — Fly caches per host; pre-pull or keep a warm pool
if you need snappier starts.
