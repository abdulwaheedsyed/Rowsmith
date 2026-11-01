#!/bin/sh
# Rebuilds internal/sshx, Rowsmith's copy of golang.org/x/crypto/ssh with
# compression added, from the x/crypto version in go.mod plus
# internal/sshx/rowsmith.patch. Files named rowsmith_* are Rowsmith's own and
# are kept as they are.
#
#   dev/sshx-sync.sh        rebuild (run after x/crypto is updated)
#   dev/sshx-sync.sh save   write the edits made to upstream files into rowsmith.patch
#
# CI rebuilds it and fails if the result differs from what is committed, so an
# x/crypto update can't land without its SSH fixes reaching the copy.
set -eu
cd "$(dirname "$0")/.."
if ! command -v go >/dev/null 2>&1; then
	# No Go here: run in the Go container, as dev/go.sh does.
	exec docker run --rm -v "$PWD":/src -w /src -v rowsmith-gomod:/go/pkg/mod \
		-e GOFLAGS=-buildvcs=false golang:1.27-bookworm sh dev/sshx-sync.sh "$@"
fi
out=internal/sshx
mod=golang.org/x/crypto

go mod download "$mod"
src=$(go list -m -f '{{.Dir}}' "$mod")
ver=$(go list -m -f '{{.Version}}' "$mod")

# pristine DIR copies the upstream files into DIR with Rowsmith's import paths.
pristine() {
	mkdir -p "$1/internal/bcrypt_pbkdf" "$1/internal/poly1305"
	for f in "$src"/ssh/*.go; do
		case "$f" in *_test.go) continue ;; esac
		cp "$f" "$1/"
	done
	cp "$src"/ssh/internal/bcrypt_pbkdf/bcrypt_pbkdf.go "$1/internal/bcrypt_pbkdf/"
	for f in "$src"/internal/poly1305/*; do
		case "$f" in *_test.go) continue ;; esac
		cp "$f" "$1/internal/poly1305/"
	done
	cp "$src/LICENSE" "$src/PATENTS" "$1/"
	chmod -R u+w "$1"
	find "$1" -name '*.go' -exec sed -i \
		-e 's#"golang.org/x/crypto/ssh/internal/bcrypt_pbkdf"#"rowsmith/internal/sshx/internal/bcrypt_pbkdf"#' \
		-e 's#"golang.org/x/crypto/internal/poly1305"#"rowsmith/internal/sshx/internal/poly1305"#' {} +
	printf '%s\n' "$mod $ver" >"$1/UPSTREAM"
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
pristine "$tmp/a"

if [ "${1:-}" = save ]; then
	# Every upstream file, as edited in internal/sshx, against the pristine copy.
	(
		cd "$tmp/a"
		find . -type f ! -name UPSTREAM | sort | while read -r f; do
			diff -u --label "a/${f#./}" --label "b/${f#./}" "$f" "$OLDPWD/$out/$f" || true
		done
	) >"$out/rowsmith.patch"
	echo "wrote $out/rowsmith.patch"
	exit 0
fi

if [ -s "$out/rowsmith.patch" ]; then
	# In the temporary copy, outside any repository, git apply works as patch does.
	(cd "$tmp/a" && git apply "$OLDPWD/$out/rowsmith.patch")
fi
mkdir -p "$out"
find "$out" -mindepth 1 -maxdepth 1 ! -name 'rowsmith_*' ! -name rowsmith.patch ! -name README.md -exec rm -rf {} +
cp -R "$tmp/a/." "$out/"
echo "rebuilt $out from $mod $ver"
