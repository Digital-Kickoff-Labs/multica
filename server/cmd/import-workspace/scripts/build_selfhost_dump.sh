#!/bin/sh
# Builds a restorable dump of the cloud workspace shaped on the self-host schema.
# Usage: build_selfhost_dump.sh <export-archive-dir> <selfhost-pg-dump.dmp> <out-dir>
set -eu

ARCHIVE=$1
SELFHOST_DUMP=$2
OUT=$3
PG_BIN=${PG_BIN:-/opt/homebrew/opt/libpq/bin}
PG_ADMIN_URL=${PG_ADMIN_URL:-postgres://multica@localhost:55432/postgres}
IMPORTER=${IMPORTER:?set IMPORTER to the import-workspace binary}
WEB_URL=${WEB_URL:?set WEB_URL, e.g. https://multica.example.com}
API_URL=${API_URL:?set API_URL, e.g. https://multica-api.example.com}
BUILD_DB=selfhost_build

TARGET_URL=$(printf '%s' "$PG_ADMIN_URL" | sed "s#/[^/]*\$#/$BUILD_DB#")
mkdir -p "$OUT/uploads"
chmod 700 "$OUT"

"$PG_BIN/psql" "$PG_ADMIN_URL" -qc "DROP DATABASE IF EXISTS $BUILD_DB" -c "CREATE DATABASE $BUILD_DB"
"$PG_BIN/pg_restore" -d "$TARGET_URL" --no-owner --no-acl --schema-only --exit-on-error "$SELFHOST_DUMP"
"$PG_BIN/pg_restore" -d "$TARGET_URL" --no-owner --no-acl --data-only -t schema_migrations --exit-on-error "$SELFHOST_DUMP"

DATABASE_URL=$TARGET_URL LOCAL_UPLOAD_DIR=$(cd "$OUT/uploads" && pwd) LOCAL_UPLOAD_BASE_URL= \
  "$IMPORTER" -archive "$ARCHIVE" -repair-dangling \
  -rewrite "https://multica.ai/api/=$API_URL/api/" \
  -rewrite "https://multica.ai=$WEB_URL" | tee "$OUT/import.log"

"$PG_BIN/pg_dump" -d "$TARGET_URL" -Fc --no-owner --no-acl -f "$OUT/multica-selfhost.dump"
tar -C "$OUT/uploads" -czf "$OUT/multica-uploads.tgz" .
echo "Ready: $OUT/multica-selfhost.dump and $OUT/multica-uploads.tgz"
