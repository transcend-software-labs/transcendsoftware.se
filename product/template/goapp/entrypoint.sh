#!/bin/sh
# Run the app under Litestream when backup storage is configured, else run it
# directly. Litestream restores the database from the replica first if the local
# file is missing (fresh machine or a lost volume), then streams the WAL
# continuously (retaining 30 days of history) — so the SQLite data survives
# volume or host loss, not just the redeploys the volume already covers, and can
# be restored to any point in the last month. Backup env is set per app by the Forge
# orchestrator; with none set this is a no-op and the app runs exactly as before.
set -e
DB="${DATA_DIR:-/data}/app.db"

if [ -n "$LITESTREAM_BUCKET" ]; then
  # Render the config from the environment ourselves — litestream 0.3 does not
  # reliably expand ${VARS} inside the YAML, so the shell does it here.
  CFG=/tmp/litestream.yml
  cat > "$CFG" <<EOF
dbs:
  - path: ${DB}
    replicas:
      - type: s3
        bucket: ${LITESTREAM_BUCKET}
        path: ${LITESTREAM_PATH}
        endpoint: ${LITESTREAM_ENDPOINT}
        region: ${LITESTREAM_REGION:-auto}
        access-key-id: ${LITESTREAM_ACCESS_KEY_ID}
        secret-access-key: ${LITESTREAM_SECRET_ACCESS_KEY}
        force-path-style: true
        # Continuous WAL streaming (litestream's ~1s default) plus a daily full
        # snapshot, keeping 30 days of history so we can restore to any point in
        # the last month — not just recover the latest state after a volume loss.
        snapshot-interval: 24h
        retention: 720h
EOF
  echo "litestream: restoring ${DB} if a replica exists…"
  litestream restore -config "$CFG" -if-db-not-exists -if-replica-exists "$DB" || true
  exec litestream replicate -config "$CFG" -exec app
fi

exec app
