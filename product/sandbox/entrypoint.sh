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

# 4) Configure opencode: permissions + model provider.
#
#    Permissions — auto-approve EVERY tool action. This sandbox is a throwaway,
#    network-restricted microVM: the sandbox itself is the security boundary, and
#    no human is present to answer opencode's interactive approval prompts. Any
#    prompt that defaults to `ask` (notably `external_directory`, hit when the
#    agent writes outside /workspace — e.g. DATA_DIR=/tmp/appdata for a local
#    smoke test) deadlocks the whole build until the deadline reaper kills it.
#    So every permission is forced to `allow`. Without this, builds hang.
#
#    Provider — when an OpenAI-compatible model is set (e.g. Moonshot/Kimi) add a
#    provider block; otherwise opencode falls back to Anthropic via env.
mkdir -p /root/.config/opencode
perm='"permission": { "edit": "allow", "bash": "allow", "webfetch": "allow", "external_directory": "allow" }'
if [ "${IMPL_PROVIDER:-}" = "anthropic" ] && [ -n "${ANTHROPIC_MODEL:-}" ]; then
  # Explicit Anthropic model (per-build model experiment). opencode 1.17.18
  # predates the newest Claude models' thinking API: given a reasoning budget it
  # emits the deprecated thinking:{type:"enabled",budgetTokens:N} shape, which
  # Sonnet 5 / Fable 5 / Opus 4.8 reject with a 400 ("use thinking.type.adaptive
  # and output_config.effort"). So we send NO thinking option and let the model
  # use its own (adaptive) default — the impl runs the chosen model, just without
  # a forced budget. Impl-effort control returns once opencode speaks
  # output_config.effort; the planner effort (our own client) is already exact.
  # ANTHROPIC_EFFORT is still passed in but intentionally unused here for now.
  log "configuring opencode: anthropic provider, model ${ANTHROPIC_MODEL} (model-default thinking; auto-approve all tools)"
  cat > /root/.config/opencode/opencode.json <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  ${perm},
  "provider": {
    "anthropic": {
      "models": { "${ANTHROPIC_MODEL}": {} }
    }
  },
  "model": "anthropic/${ANTHROPIC_MODEL}"
}
JSON
elif [ -n "${LLM_API_KEY:-}" ]; then
  base="${LLM_BASE_URL:-https://api.moonshot.ai/v1}"
  model="${LLM_MODEL:-kimi-k2.7-code}"
  # Optional reasoning effort for the OpenAI-compatible model.
  ropts="{}"
  if [ -n "${LLM_EFFORT:-}" ]; then
    ropts="{ \"options\": { \"reasoningEffort\": \"${LLM_EFFORT}\" } }"
  fi
  case "$base" in
  *api.openai.com*)
    # Direct OpenAI: use opencode's NATIVE openai provider, not the generic
    # openai-compatible shim — GPT-5.x needs the params/API the native provider
    # speaks (max_completion_tokens, Responses API). It reads OPENAI_API_KEY.
    # Declare the model EXPLICITLY on the provider: opencode otherwise resolves
    # it against its bundled models.dev snapshot, and a model newer than the
    # installed opencode (e.g. gpt-5.6-* the day after release) hard-fails the
    # session with unknown-model. The explicit entry passes any id through.
    export OPENAI_API_KEY="${LLM_API_KEY}"
    log "configuring opencode: native openai provider, model ${model} (auto-approve all tools)"
    cat > /root/.config/opencode/opencode.json <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  ${perm},
  "provider": {
    "openai": {
      "models": { "${model}": ${ropts} }
    }
  },
  "model": "openai/${model}"
}
JSON
    ;;
  *)
    log "configuring opencode for ${base} model ${model} (auto-approve all tools)"
    cat > /root/.config/opencode/opencode.json <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  ${perm},
  "provider": {
    "moonshot": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Moonshot",
      "options": { "baseURL": "${base}", "apiKey": "{env:LLM_API_KEY}" },
      "models": { "${model}": ${ropts} }
    }
  },
  "model": "moonshot/${model}"
}
JSON
    ;;
  esac
else
  log "configuring opencode (default provider, auto-approve all tools)"
  cat > /root/.config/opencode/opencode.json <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  ${perm}
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
