#!/usr/bin/env bash
# Run the Go toolchain inside Docker (the host has no Go install).
# Usage: dev/go.sh build ./...   |   NET=rowsmith-dev dev/go.sh test ./...
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
exec docker run --rm -i \
  ${NET:+--network "$NET"} \
  -v "$ROOT":/src -w /src \
  -v rowsmith-gomod:/go/pkg/mod \
  -v rowsmith-gocache:/root/.cache/go-build \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  ${GO_ENV:-} \
  golang:1.27-bookworm go "$@"
