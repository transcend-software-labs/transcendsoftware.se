#!/usr/bin/env bash
# Transcend Forge sandbox entrypoint — WIRING ONLY (never installs toolchains).
#
# Per-task inputs (env vars injected at Machine create):
#   OPENCODE_PORT   port for the opencode server           (default 4096)
#   SPEC_URL        URL to fetch the operating spec        (optional)
#   REPO_URL        git repo to clone (reserved for GitHub mirroring; unused)
#   GIT_TOKEN       short-lived, repo-scoped clone token   (optional)
#   ASSETS_MANIFEST JSON {filename: presigned-GET-url} of customer uploads
#   LLM_API_KEY / LLM_BASE_URL / LLM_MODEL   opencode's model provider
#   FLY_APP / FLY_DEPLOY_TOKEN   let the agent `fly deploy` the customer app.
#     The token is minted per build, scoped to FLY_APP alone — this sandbox
#     can deploy only its own app (see internal/fly).
#
# Storage is never credentialed here: assets arrive via presigned GET URLs, and
# workspace snapshots are restored/saved by the orchestrator over the Machines
# exec API with presigned URLs. A compromised build can spend LLM tokens and
# deploy within the org, but cannot touch storage or the Fly org API.

set -euo pipefail

PORT="${OPENCODE_PORT:-4096}"
WORKSPACE=/workspace
mkdir -p "$WORKSPACE"
cd "$WORKSPACE"

log() { echo "[sandbox] $*"; }

# 1) Bring in project source (reiteration) or start clean (greenfield).
if [ -n "${REPO_URL:-}" ]; then
  log "cloning ${REPO_URL}"
  if [ -n "${GIT_TOKEN:-}" ]; then
    git clone "https://x-access-token:${GIT_TOKEN}@${REPO_URL#https://}" repo
  else
    git clone "${REPO_URL}" repo
  fi
  cd repo
else
  log "greenfield build: empty workspace"
fi

# 2) Pull the operating spec fresh, so editing the agent's "brain" never
#    requires rebuilding this image.
if [ -n "${SPEC_URL:-}" ]; then
  log "fetching operating spec"
  curl -fsSL "${SPEC_URL}" -o AGENTS.md || log "WARN: could not fetch operating spec"
fi

# 3) Stage customer-uploaded assets from their presigned URLs (no creds here).
if [ -n "${ASSETS_MANIFEST:-}" ]; then
  log "staging uploaded assets"
  mkdir -p /workspace/assets
  echo "${ASSETS_MANIFEST}" | jq -r 'to_entries[] | "\(.key)\t\(.value)"' |
    while IFS="$(printf '\t')" read -r fname url; do
      if curl -fsSL "$url" -o "/workspace/assets/${fname}"; then
        log "  fetched ${fname}"
      else
        log "  WARN: failed to fetch ${fname}"
      fi
    done
fi

# 4) Configure opencode's model provider. If an OpenAI-compatible model is set
#    (e.g. Moonshot/Kimi), write a provider config; otherwise opencode uses its
#    default (Anthropic via ANTHROPIC_API_KEY).
if [ -n "${LLM_API_KEY:-}" ]; then
  base="${LLM_BASE_URL:-https://api.moonshot.ai/v1}"
  model="${LLM_MODEL:-kimi-k2.7-code}"
  log "configuring opencode for ${base} model ${model}"
  mkdir -p /root/.config/opencode
  cat > /root/.config/opencode/opencode.json <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  "provider": {
    "moonshot": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Moonshot",
      "options": { "baseURL": "${base}", "apiKey": "{env:LLM_API_KEY}" },
      "models": { "${model}": {} }
    }
  },
  "model": "moonshot/${model}"
}
JSON
fi

# 5) Warm the Go build cache in the background. The image ships precompiled
#    caches for the starter template, but a fresh machine's first `go build`
#    re-hashes all module sources to trust them (I/O-bound, ~1 min). Doing it
#    now — while opencode boots and the agent reads the plan — makes the
#    agent's own `go build`/`go test` effectively instant.
if [ -d /opt/forge-template ]; then
  (cd /opt/forge-template && go build ./... >/dev/null 2>&1 && log "go cache warmed") &
fi

# 6) Start opencode; the orchestrator connects over Fly's private network.
#    (Flags may vary by opencode version — confirm against the pinned release.)
# Bind :: (IPv6, dual-stack) so the orchestrator can reach it over Fly's private
# 6PN network, which is IPv6-only.
log "starting opencode on :${PORT}"
exec opencode serve --hostname :: --port "${PORT}"
