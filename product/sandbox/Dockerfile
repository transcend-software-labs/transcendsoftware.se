# Transcend Forge — build sandbox image.
#
# A per-task Fly Machine (Firecracker microVM) boots from this image, runs the
# agent build + verification, then is destroyed. Everything slow-to-install is
# baked in so there are NO toolchain installs at runtime — only the generated
# site's own dependencies install per build (inherent; that's unavoidable).
#
# The Playwright base ships Node + Chromium + all the system libraries, which is
# the genuinely hard part of getting headless-browser verification working.
#
# Build & push (see sandbox/README.md):
#   fly apps create transcend-forge-sandbox          # once
#   fly auth docker
#   docker build -t registry.fly.io/transcend-forge-sandbox:$(date +%Y%m%d) ./sandbox
#   docker push registry.fly.io/transcend-forge-sandbox:<tag>

# Pin to a current Playwright release (bundles Node 20 + Chromium + deps).
# Bump deliberately; do not track a floating tag in production.
FROM mcr.microsoft.com/playwright:v1.50.0-jammy

ARG GO_VERSION=1.25.0
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl ca-certificates jq \
 && rm -rf /var/lib/apt/lists/*

# pnpm via npm. (Corepack in the base image has stale signing keys and can't
# verify current pnpm releases.) Pin a version in production.
RUN npm i -g pnpm

# Go — only needed if the canonical stack builds Go services; drop this layer
# if your default stack is JS-only.
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
      | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:/root/.opencode/bin:${PATH}"

# opencode — the agent engine the orchestrator drives over HTTP.
# Pin a version in production rather than tracking the install script's latest.
RUN curl -fsSL https://opencode.ai/install | bash

# flyctl — the agent deploys the finished site to its per-customer app using an
# app-scoped token the orchestrator injects (the org token never enters here).
RUN curl -L https://fly.io/install.sh | FLYCTL_INSTALL=/usr/local sh

# Verify the toolchain is installed and on PATH — fails the build early if not.
RUN node --version && npm --version && pnpm --version && go version \
 && command -v opencode && command -v flyctl && command -v git && test -d /ms-playwright

WORKDIR /workspace
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The Fly Machine microVM is the isolation boundary, so running as root inside is
# acceptable — and real credentials are never injected here (the orchestrator
# performs the deploy outside the sandbox).
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
