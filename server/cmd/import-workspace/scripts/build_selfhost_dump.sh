#!/bin/sh
# Merges a `multica workspace export` archive into a snapshot of the target
# self-host database and packages the result for restore. See docs/selfhost-migration.md.
#
# Usage: build_selfhost_dump.sh <export-archive-dir> <selfhost.dump> <out-dir> [import-workspace flags...]
#
# Environment:
#   PG_MODE         docker (throwaway pgvector/pgvector:pg17 container) or local; default: docker when available
#   PG_ADMIN_URL    local mode: URL of a PostgreSQL 17 server role allowed to create databases
#   PG_BIN          local mode: directory holding psql, pg_restore and pg_dump (default: from PATH)
#   PG_IMAGE        docker mode image (default: pgvector/pgvector:pg17)
#   IMPORTER        import-workspace binary (default: built from this checkout with go)
#   SOURCE_APP_URL  app URL of the source server (default: https://multica.ai)
#   WEB_URL         app URL of the self-host, e.g. https://multica.example.com; rewrites issue links when set
set -eu

if [ $# -lt 3 ]; then
  sed -n '5p' "$0" >&2
  exit 2
fi
ARCHIVE=$(cd "$1" && pwd)
SELFHOST_DUMP=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
mkdir -p "$3"
OUT=$(cd "$3" && pwd)
shift 3

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SERVER_DIR=$(cd "$SCRIPT_DIR/../../.." && pwd)
BUILD_DB=multica_selfhost_build
SOURCE_APP_URL=${SOURCE_APP_URL:-https://multica.ai}
WEB_URL=${WEB_URL:-}

[ -f "$ARCHIVE/manifest.json" ] || { echo "$ARCHIVE has no manifest.json: finish the export first" >&2; exit 1; }
[ -f "$SELFHOST_DUMP" ] || { echo "self-host dump not found: $SELFHOST_DUMP" >&2; exit 1; }
if [ -d "$OUT/uploads" ] && [ -n "$(ls -A "$OUT/uploads")" ]; then
  echo "$OUT/uploads is not empty: use a fresh output directory" >&2
  exit 1
fi
mkdir -p "$OUT/uploads"
chmod 700 "$OUT"

if [ -z "${PG_MODE:-}" ]; then
  if [ -z "${PG_ADMIN_URL:-}" ] && command -v docker >/dev/null 2>&1; then PG_MODE=docker; else PG_MODE=local; fi
fi

case $PG_MODE in
docker)
  CONTAINER=multica-selfhost-build-$$
  trap 'docker rm -f "$CONTAINER" >/dev/null 2>&1 || true' EXIT INT TERM
  docker run -d --name "$CONTAINER" -e POSTGRES_PASSWORD=build -e POSTGRES_USER=postgres \
    -p 127.0.0.1::5432 "${PG_IMAGE:-pgvector/pgvector:pg17}" >/dev/null
  # The image's init server listens on the socket only; TCP answers once the real server is up.
  until docker exec "$CONTAINER" pg_isready -h 127.0.0.1 -U postgres -q 2>/dev/null; do sleep 1; done
  PORT=$(docker port "$CONTAINER" 5432/tcp | head -n1 | sed 's/.*://')
  pg() { tool=$1; shift; docker exec -i "$CONTAINER" "$tool" -U postgres "$@"; }
  ADMIN_DB=postgres
  BUILD_TARGET=$BUILD_DB
  TARGET_URL="postgres://postgres:build@127.0.0.1:$PORT/$BUILD_DB?sslmode=disable"
  ;;
local)
  PG_ADMIN_URL=${PG_ADMIN_URL:?set PG_ADMIN_URL (local mode) or install docker}
  pg() { tool=$1; shift; "${PG_BIN:+$PG_BIN/}$tool" "$@"; }
  ADMIN_DB=$PG_ADMIN_URL
  TARGET_URL=$(printf '%s' "$PG_ADMIN_URL" | sed -E "s#/[^/?]*(\?|\$)#/$BUILD_DB\1#")
  BUILD_TARGET=$TARGET_URL
  ;;
*)
  echo "PG_MODE must be docker or local" >&2
  exit 2
  ;;
esac

if [ -z "${IMPORTER:-}" ]; then
  IMPORTER=$OUT/import-workspace
  (cd "$SERVER_DIR" && go build -o "$IMPORTER" ./cmd/import-workspace)
fi

echo "==> Restoring the self-host snapshot into $BUILD_DB"
pg psql -d "$ADMIN_DB" -v ON_ERROR_STOP=1 -qc "DROP DATABASE IF EXISTS $BUILD_DB" -c "CREATE DATABASE $BUILD_DB"
pg pg_restore -d "$BUILD_TARGET" --no-owner --no-acl --exit-on-error <"$SELFHOST_DUMP"

echo "==> Importing $ARCHIVE"
set -- -archive "$ARCHIVE" -repair-dangling "$@"
if [ -n "$WEB_URL" ]; then
  set -- "$@" -rewrite "$SOURCE_APP_URL=$WEB_URL"
fi
if ! DATABASE_URL=$TARGET_URL LOCAL_UPLOAD_DIR=$OUT/uploads LOCAL_UPLOAD_BASE_URL= S3_BUCKET= \
  "$IMPORTER" "$@" >"$OUT/import.log" 2>&1; then
  cat "$OUT/import.log" >&2
  echo "import failed: nothing was packaged" >&2
  exit 1
fi
cat "$OUT/import.log"

echo "==> Checking the files written for the restore"
EXPECTED=$(sed -n 's/^files written: \([0-9]*\).*/\1/p' "$OUT/import.log")
PRESENT=$(find "$OUT/uploads" -type f ! -name '*.meta.json' | wc -l | tr -d ' ')
echo "files expected: ${EXPECTED:-0}, present: $PRESENT"
if [ "${EXPECTED:-0}" != "$PRESENT" ]; then
  echo "file count mismatch: do not restore this build" >&2
  exit 1
fi

echo "==> Packaging"
pg pg_dump -d "$BUILD_TARGET" -Fc --no-owner --no-acl >"$OUT/multica-selfhost.dump"
tar -C "$OUT/uploads" -czf "$OUT/multica-uploads.tgz" .
echo "Ready:"
echo "  $OUT/multica-selfhost.dump  (pg_restore --clean --if-exists into the self-host database)"
echo "  $OUT/multica-uploads.tgz    (tar -xzf ... -C \$LOCAL_UPLOAD_DIR; $PRESENT files)"
