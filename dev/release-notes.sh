#!/usr/bin/env bash
# Prints the notes for a GitHub release: the version's section of CHANGELOG.md,
# then how to install it. A release candidate (0.2.0-rc.1) gets the notes of
# the version it leads to (0.2.0).
#   dev/release-notes.sh 0.1.0
set -euo pipefail
VERSION="${1:?usage: dev/release-notes.sh VERSION}"
BASE="${VERSION%%-*}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO=abdulwaheedsyed/Rowsmith
IMAGE=ghcr.io/abdulwaheedsyed/rowsmith

# The section runs from "## 0.1.0 - 2026-10-20" to the next "## " heading.
heading="$(grep -m1 -E "^## ${BASE//./\\.}( |$)" "$ROOT/CHANGELOG.md" || true)"
[ -n "$heading" ] || { echo "CHANGELOG.md has no section for $BASE" >&2; exit 1; }
if [ "$BASE" = "$VERSION" ] && [[ "$heading" == *Unreleased* ]]; then
  echo "date the $BASE section of CHANGELOG.md before releasing it" >&2
  exit 1
fi
awk -v h="$heading" '$0 == h { on = 1; next } on && /^## / { exit } on' "$ROOT/CHANGELOG.md" | sed '/./,$!d'

cat <<NOTES

## Install

The container image runs on linux/amd64 and linux/arm64:

\`\`\`bash
docker pull $IMAGE:$VERSION
\`\`\`

To run the server without Docker, download the archive for your system below. To check a download:

\`\`\`bash
sha256sum --check --ignore-missing SHA256SUMS
\`\`\`

\`\`\`bash
gh attestation verify rowsmith_${VERSION}_linux_amd64.tar.gz --repo $REPO
\`\`\`

Setup, configuration and upgrades are in the [README](https://github.com/$REPO/blob/v$VERSION/README.md#install).
NOTES
