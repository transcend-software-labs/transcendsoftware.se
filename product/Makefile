DATABASE_URL ?= postgres://postgres:pw@localhost:5432/rasmusai?sslmode=disable
SANDBOX_TAG ?= $(shell date +%Y%m%d)
SANDBOX_IMAGE ?= registry.fly.io/transcend-forge-sandbox:$(SANDBOX_TAG)

.PHONY: run build test vet db-up db-down db-migrate sandbox-build template-push template-test

# Run in dev mode (in-memory store, fake planner/gate/builder — no secrets needed).
run:
	go run ./cmd/server

# Run against local Postgres (start it first: make db-up && make db-migrate).
run-pg:
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

test:
	go test ./...

vet:
	go vet ./...

db-up:
	docker compose up -d db

db-down:
	docker compose down

# Manual escape hatch only — the server applies migrations itself at startup
# (embedded, tracked in schema_migrations). Files are idempotent, so this is
# safe to run alongside; it just doesn't record versions.
db-migrate:
	for f in migrations/*.sql; do psql -v ON_ERROR_STOP=1 "$(DATABASE_URL)" -f $$f || exit 1; done

# Build + push the per-task build sandbox image on Fly's remote builder (native
# amd64). Stages the starter template into the build context so its Go caches
# are warmed in the image. See sandbox/README.md.
sandbox-build:
	rm -rf sandbox/.template && mkdir -p sandbox/.template
	cp -R template/goapp sandbox/.template/goapp
	rm -rf sandbox/.template/goapp/data sandbox/.template/goapp/bin
	cd sandbox && fly deploy --build-only --push --remote-only --image-label $(SANDBOX_TAG)

# Upload the starter template tarball that seeds first builds (TEMPLATE_KEY).
template-push:
	go run ./cmd/templatepush -dir template/goapp -key templates/goapp.tgz

template-test:
	cd template/goapp && go vet ./... && go test ./...

# --- Model experiments -------------------------------------------------------
# Switch which models plan + build, env-only (no code change, app restarts
# automatically). The OPENCODE_GO_API_KEY secret backs both Zen endpoints, so
# no new keys are needed. Verify what's live: make model-status
model-grok:
	fly secrets set -a transcend-forge \
	  LLM_BASE_URL=https://opencode.ai/zen/v1 \
	  LLM_MODEL=grok-4.5 \
	  PLANNER_LLM_BASE_URL=https://opencode.ai/zen/v1 \
	  PLANNER_LLM_MODEL=grok-4.5

# Back to the standard wiring: kimi-k2.7-code impl + glm-5.2 planning on zen/go/v1.
model-default:
	fly secrets unset -a transcend-forge \
	  LLM_BASE_URL LLM_MODEL PLANNER_LLM_BASE_URL PLANNER_LLM_MODEL

# Show the live model wiring from the app's startup log.
model-status:
	fly logs -a transcend-forge --no-tail 2>/dev/null | grep "llm: model config" | tail -1
