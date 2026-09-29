#!/usr/bin/env bash
# Builds the web UI, embeds it, and compiles the server into .bin/rowsmith.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/web"
[ -d node_modules ] || npm ci --no-audit --no-fund
npm run build --silent
find "$ROOT/internal/web/dist" -mindepth 1 ! -name .keep -delete
cp -R dist/. "$ROOT/internal/web/dist/"
cd "$ROOT"
dev/go.sh build -trimpath -ldflags "-s -w" -o /src/.bin/rowsmith ./cmd/rowsmith
echo "built .bin/rowsmith"
