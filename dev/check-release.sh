#!/usr/bin/env bash
# Checks a release archive on the machine it was built for: unpacks it, checks
# the version, starts the server on a scratch data directory and waits until
# it answers.
#   dev/check-release.sh dist/rowsmith_0.1.0_linux_amd64.tar.gz 0.1.0
set -euo pipefail
ARCHIVE="${1:?usage: dev/check-release.sh ARCHIVE VERSION}"
VERSION="${2:?usage: dev/check-release.sh ARCHIVE VERSION}"
WORK="$(mktemp -d)"
PID=
trap 'kill "$PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT

# fail explains what went wrong, in GitHub Actions also as an annotation on the run.
fail() {
  printf '%s\n' "$1" >&2
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    local msg="${1//'%'/'%25'}"
    msg="${msg//$'\r'/}"
    echo "::error title=$(basename "$ARCHIVE")::${msg//$'\n'/'%0A'}"
  fi
  exit 1
}

case "$ARCHIVE" in
  *.zip)
    if command -v unzip >/dev/null; then
      unzip -q "$ARCHIVE" -d "$WORK" || fail "unzip failed"
    else # Windows without unzip
      7z x -y -o"$(cygpath -w "$WORK")" "$(cygpath -w "$ARCHIVE")" >/dev/null || fail "7z failed"
    fi ;;
  *) tar -xzf "$ARCHIVE" -C "$WORK" || fail "tar failed" ;;
esac
BIN="$(find "$WORK" -name rowsmith -o -name rowsmith.exe | head -n1)"
[ -n "$BIN" ] || fail "no rowsmith binary in the archive: $(cd "$WORK" && find . | head -n 20)"

got="$("$BIN" --version 2>&1 | tr -d '\r')" || fail "rowsmith --version failed: $got"
[ "$got" = "rowsmith $VERSION" ] || fail "version: got \"$got\", want \"rowsmith $VERSION\""
echo "version ok: $got"

ROWSMITH_DATA_DIR="$WORK/data" ROWSMITH_ADDR=127.0.0.1:18765 "$BIN" serve >"$WORK/log" 2>&1 &
PID=$!
for _ in $(seq 90); do
  if boot="$(curl -sf http://127.0.0.1:18765/api/bootstrap)"; then
    case "$boot" in
      *"\"version\":\"$VERSION\""*) echo "server ok: $boot"; exit 0 ;;
      *) fail "server reports another version: $boot" ;;
    esac
  fi
  kill -0 "$PID" 2>/dev/null || fail "the server stopped: $(tail -n 30 "$WORK/log")"
  sleep 1
done
fail "the server didn't answer within 90 seconds: $(tail -n 30 "$WORK/log")"
