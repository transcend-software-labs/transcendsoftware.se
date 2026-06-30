#!/usr/bin/env bash
# Transcend Forge sandbox entrypoint — WIRING ONLY (never installs toolchains).
#
# Per-task inputs (env vars injected at Machine create):
#   OPENCODE_PORT  port for the opencode server          (default 4096)
#   SPEC_URL       URL to fetch the operating spec        (optional)
#   REPO_URL       git repo to work in for reiterations   (empty = greenfield)
#   GIT_TOKEN      short-lived, repo-scoped clone token   (optional)
#
# Real deploy credentials are intentionally NOT passed here. The orchestrator
# performs the deploy outside the sandbox, so a compromised build leaks nothing.

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

# 3) Start opencode; the orchestrator connects over Fly's private network.
#    (Flags may vary by opencode version — confirm against the pinned release.)
log "starting opencode on :${PORT}"
exec opencode serve --hostname 0.0.0.0 --port "${PORT}"
