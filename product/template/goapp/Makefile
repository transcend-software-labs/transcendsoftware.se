.PHONY: run build test vet js

# Compile web/src/app.ts → internal/web/static/app.js (embedded in the binary),
# then type-check it strictly. tsc is preinstalled in the build sandbox; if it
# is missing locally the emit still happens and the typecheck is skipped loudly.
js:
	go run ./tools/buildjs
	@if command -v tsc >/dev/null 2>&1; then tsc -p .; \
	else echo "note: tsc not found — app.ts compiled but NOT type-checked (npm i -g typescript)"; fi

run: js
	go run .

build: js
	go build -o bin/app .

test: js
	go test ./...

vet:
	go vet ./...
