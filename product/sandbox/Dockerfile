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

# Pin to a current Playwright release (bundles Node 22 + Chromium + deps).
# Bump deliberately; do not track a floating tag in production. Note: impeccable
# is pinned to its 2.x line because this base ships Node 22 and impeccable 3.x
# requires Node >=24 (and upgrading Node here breaks the Playwright CLI).
FROM mcr.microsoft.com/playwright:v1.50.0-jammy

ARG GO_VERSION=1.25.0
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl ca-certificates jq \
 && rm -rf /var/lib/apt/lists/*

# pnpm via npm. (Corepack in the base image has stale signing keys and can't
# verify current pnpm releases.) Pin a version in production.
RUN npm i -g pnpm

# playwright CLI, pinned to the base image's browser version (v1.50.0) so the
# baked /ms-playwright browsers match. Used to screenshot the deployed site for
# Rasmus's review — the browsers are already present, so this is just the JS.
RUN npm i -g playwright@1.50.0

# impeccable — deterministic frontend-design detector (no LLM/API key). The
# agent runs `impeccable detect` on its output before deploying to catch
# AI-slop design tells. Pinned to the 2.x line (node >=18; the 3.x line needs
# node >=24, which this Playwright base doesn't ship).
RUN npm i -g impeccable@2.3.2

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
 && command -v opencode && command -v flyctl && command -v git \
 && command -v playwright && test -d /ms-playwright \
 && impeccable --version

# Warm the Go module + build caches with the starter template's dependency
# tree (staged into .template/ by `make sandbox-build`). modernc SQLite alone
# takes minutes of CPU to compile cold on a shared-cpu machine; with this
# layer the agent's `go build` / `go test` inside a build finish in seconds.
# Building + testing here also gates the image on a working template.
COPY .template/goapp /opt/forge-template
RUN cd /opt/forge-template && go mod download && go build ./... && go test ./...

WORKDIR /workspace
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The Fly Machine microVM is the isolation boundary, so running as root inside
# is acceptable. Credentials that DO enter per task: the LLM API key and an
# (interim, org-scoped) Fly deploy token — see entrypoint.sh for the full list
# and internal/fly for the per-app-token hardening TODO.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
