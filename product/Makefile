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
# Three providers, switchable env-only (no code change; the app restarts itself
# in seconds). Pick the models per role with IMPL= and PLANNER= (PLANNER
# defaults to IMPL). Examples:
#   make model-go     IMPL=kimi-k2.7-code PLANNER=glm-5.2   # OpenCode Zen Go tier
#   make model-go     IMPL=minimax-m3                        #   (open-weight models)
#   make model-zen    IMPL=grok-4.5                          # OpenCode Zen main (grok/gpt/claude/...)
#   make model-openai IMPL=gpt-5.6-sol                       # OpenAI direct (uses OPENAI_API_KEY)
#   make model-default                                       # standard: kimi + glm on Zen Go
# Mixed-provider combos: set LLM_* / PLANNER_LLM_* fly secrets by hand.
# Verify what is live: make model-status
FORGE_APP = transcend-forge
PLANNER ?= $(IMPL)

model-go:
	@test -n "$(IMPL)" || { echo "usage: make model-go IMPL=<model> [PLANNER=<model>]"; exit 1; }
	fly secrets unset -a $(FORGE_APP) LLM_BASE_URL PLANNER_LLM_BASE_URL 2>/dev/null || true
	fly secrets set -a $(FORGE_APP) LLM_MODEL=$(IMPL) PLANNER_LLM_MODEL=$(PLANNER)

model-zen:
	@test -n "$(IMPL)" || { echo "usage: make model-zen IMPL=<model> [PLANNER=<model>]"; exit 1; }
	fly secrets set -a $(FORGE_APP) \
	  LLM_BASE_URL=https://opencode.ai/zen/v1 LLM_MODEL=$(IMPL) \
	  PLANNER_LLM_BASE_URL=https://opencode.ai/zen/v1 PLANNER_LLM_MODEL=$(PLANNER)

model-openai:
	@test -n "$(IMPL)" || { echo "usage: make model-openai IMPL=<model> [PLANNER=<model>]"; exit 1; }
	fly secrets set -a $(FORGE_APP) \
	  LLM_BASE_URL=https://api.openai.com/v1 LLM_MODEL=$(IMPL) \
	  PLANNER_LLM_BASE_URL=https://api.openai.com/v1 PLANNER_LLM_MODEL=$(PLANNER)

# Back to the standard wiring: kimi-k2.7-code impl + glm-5.2 planning on zen/go/v1.
model-default:
	fly secrets unset -a $(FORGE_APP) \
	  LLM_BASE_URL LLM_MODEL PLANNER_LLM_BASE_URL PLANNER_LLM_MODEL

# Show the live model wiring from the app's startup log.
model-status:
	fly logs -a $(FORGE_APP) --no-tail 2>/dev/null | grep "llm: model config" | tail -1
