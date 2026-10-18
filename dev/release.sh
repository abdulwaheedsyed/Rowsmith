#!/usr/bin/env bash
# Builds the release archives into dist/: the server for Linux, macOS and
# Windows on amd64 and arm64, each with the license files, plus SHA256SUMS.
#   dev/release.sh 0.1.0
# Build the web app into internal/web/dist first (dev/build.sh does). Uses the
# local Go toolchain if there is one, otherwise dev/go.sh. Archives are
# reproducible: files carry the commit's time, so the same commit built with
# the same Go version gives the same checksums.
set -euo pipefail
VERSION="${1:?usage: dev/release.sh VERSION}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
[ -f internal/web/dist/index.html ] || { echo "build the web app into internal/web/dist first" >&2; exit 1; }

build() { # os arch output
  local flags=(-trimpath -buildvcs=false -ldflags "-s -w -X rowsmith/internal/api.Version=$VERSION" -o "$3" ./cmd/rowsmith)
  if command -v go >/dev/null; then
    GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build "${flags[@]}"
  else
    GO_ENV="-e GOOS=$1 -e GOARCH=$2" dev/go.sh build "${flags[@]}"
  fi
}

EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}"
export TZ=UTC
rm -rf dist
mkdir -p dist/build
for target in $TARGETS; do
  os=${target%/*} arch=${target#*/}
  name="rowsmith_${VERSION}_${os}_${arch}"
  exe=rowsmith
  [ "$os" = windows ] && exe=rowsmith.exe
  mkdir -p "dist/build/$name"
  build "$os" "$arch" "dist/build/$name/$exe"
  cp LICENSE NOTICE THIRD_PARTY_NOTICES.txt README.md "dist/build/$name/"
  find "dist/build/$name" -exec touch -d "@$EPOCH" {} +
  if [ "$os" = windows ]; then
    (cd dist/build && find "$name" | LC_ALL=C sort | zip -qX "../$name.zip" -@)
  else
    tar -C dist/build --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$EPOCH" -cf - "$name" | gzip -9n >"dist/$name.tar.gz"
  fi
  echo "built $name"
done
(cd dist && sha256sum -- *.tar.gz *.zip >SHA256SUMS)
