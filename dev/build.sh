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
# The version is the nearest tag, plus the commits since it: 0.1.0-4-g1a2b3c4
VERSION="$(git describe --tags --always --dirty --match 'v[0-9]*' 2>/dev/null | sed 's/^v//')"
dev/go.sh build -trimpath -ldflags "-s -w -X rowsmith/internal/api.Version=${VERSION:-dev}" -o /src/.bin/rowsmith ./cmd/rowsmith
echo "built .bin/rowsmith ${VERSION:-dev}"
