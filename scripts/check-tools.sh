#!/bin/sh
# Verifies Go and fetches the pinned linters into bin/ when missing or of another
# version, each checked against its SHA-256 for this platform. Every make target uses bin/<tool>.
set -eu

GO_VERSION=$1
GOLANGCI_LINT_VERSION=2.15.0
BIN=bin

test "$(go env GOVERSION)" = "go$GO_VERSION" || { echo "Go $GO_VERSION is required, found $(go env GOVERSION)"; exit 1; }
platform="$(go env GOHOSTOS)-$(go env GOHOSTARCH)"

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

# fetch <url> <sha256> <target>: downloads to a temporary file and moves it into place only when the hash matches.
fetch() {
	mkdir -p "$BIN"
	work="$BIN/.fetch.$$"
	trap 'rm -rf "$work"' EXIT
	curl --fail --silent --show-error --location --max-time 300 -o "$work" "$1"
	actual=$(sha256 "$work")
	if [ "$actual" != "$2" ]; then echo "$1: SHA-256 $actual, expected $2; not installed"; exit 1; fi
	mv "$work" "$3"
}

if [ "$("$BIN/golangci-lint" version --short 2>/dev/null || true)" != "$GOLANGCI_LINT_VERSION" ]; then
	case $platform in
	darwin-amd64) sum=767b899a28285e88ded2823d5dcc4bc5ef681173eb2d80f029a7b17ee4ece3b8 ;;
	darwin-arm64) sum=d1e409481a8ca846c4bc97e781eb5cb1bd5c23353a0b3977a1cbda1b1b6447d5 ;;
	linux-amd64) sum=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea ;;
	linux-arm64) sum=b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998 ;;
	*) echo "golangci-lint: no pinned build for $platform"; exit 1 ;;
	esac
	name=golangci-lint-$GOLANGCI_LINT_VERSION-$platform
	fetch "https://github.com/golangci/golangci-lint/releases/download/v$GOLANGCI_LINT_VERSION/$name.tar.gz" "$sum" "$BIN/golangci-lint.tar.gz"
	tar -xzOf "$BIN/golangci-lint.tar.gz" "$name/golangci-lint" > "$BIN/golangci-lint"
	rm "$BIN/golangci-lint.tar.gz"
	chmod 755 "$BIN/golangci-lint"
fi
