#!/usr/bin/env bash
# Checks a release archive on the machine it was built for: unpacks it, checks
# the version, starts the server on a scratch data directory and waits until
# it answers.
#   dev/check-release.sh dist/rowsmith_0.1.0_linux_amd64.tar.gz 0.1.0
set -euo pipefail
ARCHIVE="${1:?usage: dev/check-release.sh ARCHIVE VERSION}"
VERSION="${2:?usage: dev/check-release.sh ARCHIVE VERSION}"
WORK="$(mktemp -d)"
trap 'kill "$PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT
PID=

case "$ARCHIVE" in
  *.zip)
    if command -v unzip >/dev/null; then
      unzip -q "$ARCHIVE" -d "$WORK"
    else # Windows without unzip
      7z x -y -o"$(cygpath -w "$WORK")" "$(cygpath -w "$ARCHIVE")" >/dev/null
    fi ;;
  *) tar -xzf "$ARCHIVE" -C "$WORK" ;;
esac
BIN="$(find "$WORK" -name rowsmith -o -name rowsmith.exe | head -n1)"

got="$("$BIN" --version | tr -d '\r')"
[ "$got" = "rowsmith $VERSION" ] || { echo "version: got \"$got\", want \"rowsmith $VERSION\"" >&2; exit 1; }
echo "version ok: $got"

ROWSMITH_DATA_DIR="$WORK/data" ROWSMITH_ADDR=127.0.0.1:18765 "$BIN" serve >"$WORK/log" 2>&1 &
PID=$!
for _ in $(seq 60); do
  if boot="$(curl -sf http://127.0.0.1:18765/api/bootstrap)"; then
    case "$boot" in
      *"\"version\":\"$VERSION\""*) echo "server ok: $boot"; exit 0 ;;
      *) echo "server reports another version: $boot" >&2; exit 1 ;;
    esac
  fi
  sleep 1
done
echo "the server didn't answer within a minute:" >&2
cat "$WORK/log" >&2
exit 1
