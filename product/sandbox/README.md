# Build sandbox image

The per-task runtime for Transcend Forge builds. A fresh Fly Machine (Firecracker
microVM) boots from this image, runs the agent build + verification, then is
destroyed. The toolchain is baked in so **nothing installs at runtime**.

## Baked in
- Node + npm/pnpm, **Chromium + Playwright** (headless verification), Go, git
  (the Playwright base provides Node, Chromium, and all system libraries)
- **opencode** — the agent engine the orchestrator drives over HTTP
- **flyctl** — the agent runs `fly deploy` itself using the deploy token the
  orchestrator injects (`FLY_APP`/`FLY_DEPLOY_TOKEN`; org-scoped for now — see
  `internal/fly` for the per-app hardening TODO)
- **Warm Go caches for the starter template** (`/opt/forge-template`): the
  template's module + build caches are compiled into the image, so the agent's
  `go build` / `go test` finish in seconds instead of minutes (modernc SQLite
  alone takes minutes of CPU to compile cold). Building + testing the template
  also gates the image on a working starter.

## Trust model, in short
The microVM is the isolation boundary. Inside per task: the LLM API key and the
deploy token. Never inside: the Fly org API token and storage credentials —
assets, snapshots, and the template travel via short-lived presigned URLs.

## Per-task env (injected at Machine create)
See the header of `entrypoint.sh` for the authoritative list (ports, asset
manifest, LLM provider config, deploy env).

## Build & push (to your Fly registry)

From `product/` (stages the template into the build context, then builds on
Fly's remote builder — native amd64, no local pull):

```sh
make sandbox-build SANDBOX_TAG=$(date +%Y%m%d)
```

Then update the product's secret:
`FLY_SANDBOX_IMAGE=registry.fly.io/transcend-forge-sandbox:<tag>`.
`fly.SpawnSandbox` creates Machines in `FLY_SANDBOX_APP` from that image.

## Project dependencies
Template-based builds start from warmed Go caches (above). If the agent adds a
new Go dependency it downloads just that one. JS-based sites still `npm install`
per build — if that becomes a bottleneck, mount a pnpm store on a Fly volume.

## Version pinning
Pin the Playwright tag, Go version, pnpm, and opencode for reproducibility, and
bump them deliberately. The base image is large (~1.5 GB), so cold start is
dominated by the image pull — Fly caches per host; pre-pull or keep a warm pool
if you need snappier starts.
