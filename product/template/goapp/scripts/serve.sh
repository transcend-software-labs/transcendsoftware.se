#!/bin/bash
# Reliable (re)start of the app for local browser testing. Run this INSTEAD of
# hand-managing processes or ports — it is idempotent and does the whole
# lifecycle in one shot: kill any previous instance HARD (so the port frees
# immediately — no graceful-drain lag, no "address already in use"), wipe the
# throwaway data dir, build in the foreground (so compile errors surface here),
# start the app detached, and wait until it is healthy.
#
#   ./scripts/serve.sh                 # (re)start on :8080, owner@test.local
#
# NEVER debug ports/processes with ps/lsof/fuser/ss/netstat/kill — that is the
# #1 time sink. If a start ever fails, just run this script again; its SIGKILL
# frees the port every time. Read /tmp/forge-app.log if it won't come up.
set -u
PORT="${PORT:-8080}"
DATA=/tmp/forge-data
APP=/tmp/forge-app

pkill -9 -f "$APP" 2>/dev/null || true   # SIGKILL: throwaway test process, no graceful drain
sleep 0.5                                # let the kernel release the port
rm -rf "$DATA" && mkdir -p "$DATA"

if ! go build -o "$APP" .; then
  echo "BUILD FAILED — fix the compile error above, then re-run ./scripts/serve.sh"
  exit 1
fi

DATA_DIR="$DATA" PORT="$PORT" OWNER_EMAIL=owner@test.local setsid "$APP" >/tmp/forge-app.log 2>&1 </dev/null &

for i in $(seq 1 40); do
  if curl -sf -m 2 "http://localhost:$PORT/healthz" >/dev/null 2>&1; then
    echo "app ready on http://localhost:$PORT  (owner: owner@test.local / ownerpass123)"
    exit 0
  fi
  sleep 0.5
done
echo "app did not become healthy in 20s — last log lines:"
tail -20 /tmp/forge-app.log
exit 1
