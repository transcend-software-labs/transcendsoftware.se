DATABASE_URL ?= postgres://postgres:pw@localhost:5432/rasmusai?sslmode=disable

.PHONY: run build test vet db-up db-down db-migrate

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

db-migrate:
	psql "$(DATABASE_URL)" -f migrations/0001_init.sql
