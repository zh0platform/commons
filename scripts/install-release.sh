#!/bin/sh
# Installs one release binary after checking the SHA-256 the caller recorded and, when asked, its Sigstore signature.
#
#   install-release.sh NAME DEST URL SHA256 [MEMBER [BUNDLE_URL IDENTITY ISSUER COSIGN]]
#
# NAME     the file name to install under, in the directory DEST
# URL      the release asset; SHA256 its expected digest
# MEMBER   the binary's path inside a .tar.gz asset; empty when the asset is the binary itself
# BUNDLE_URL, IDENTITY, ISSUER, COSIGN
#          verify the asset with `COSIGN verify-blob` against that Sigstore bundle, certificate identity and OIDC
#          issuer; COSIGN is an absolute path, so no other cosign on PATH is used
#
# Needs curl, tar, and sha256sum or shasum. Network errors and HTTP 408, 429 and 5xx are retried three times; a wrong
# digest, a failed signature, a missing member or any other HTTP error is not. On any failure DEST/NAME is unchanged.
set -eu

# Bounds per download: one attempt 120 s, all retries together 240 s. All four tools took 27 s on the Mac; the image
# job, which fetches cosign (146 MB), Trivy and its bundle, took 2 minutes in all on both CI legs (2026-11-23).
max_time=120
retry_max_time=240

if [ $# -ne 4 ] && [ $# -ne 5 ] && [ $# -ne 9 ]; then
	echo "usage: install-release.sh NAME DEST URL SHA256 [MEMBER [BUNDLE_URL IDENTITY ISSUER COSIGN]]" >&2
	exit 2
fi
name=$1 dest=$2 url=$3 want=$4 member=${5:-}

fail() {
	echo "$name: $*" >&2
	exit 1
}

for program in curl tar; do
	command -v "$program" >/dev/null 2>&1 || fail "$program is required"
done
if command -v sha256sum >/dev/null 2>&1; then
	digest() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	digest() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail "sha256sum or shasum is required"
fi
[ ! -d "$dest/$name" ] || fail "$dest/$name is a directory"
if [ $# -eq 9 ]; then
	cosign=$9
	case $cosign in /*) ;; *) fail "the cosign path must be absolute, not $cosign" ;; esac
	[ -x "$cosign" ] || fail "cosign not found at $cosign: run make install-cosign"
fi

mkdir -p "$dest"
work=$(mktemp -d "$dest/.$name.install.XXXXXX")
trap 'rm -rf "$work"' EXIT
asset=$work/asset binary=$work/binary bundle=$work/bundle

fetch() {
	curl --fail --silent --show-error --location --connect-timeout 20 --max-time "$max_time" \
		--retry 3 --retry-connrefused --retry-max-time "$retry_max_time" -o "$2" "$1" || fail "download failed: $1"
}

fetch "$url" "$asset"
got=$(digest "$asset")
[ "$got" = "$want" ] || fail "SHA-256 mismatch for ${url##*/}: recorded $want, computed $got"

verified=""
if [ $# -eq 9 ]; then
	identity=$7 issuer=$8
	fetch "$6" "$bundle"
	if ! output=$("$cosign" verify-blob "$asset" --bundle "$bundle" \
		--certificate-identity "$identity" --certificate-oidc-issuer "$issuer" 2>&1); then
		printf '%s\n' "$output" >&2
		fail "signature verification failed for ${url##*/}"
	fi
	verified=", signed by $identity"
fi

if [ -n "$member" ]; then
	tar -xzOf "$asset" "$member" >"$binary" 2>/dev/null || fail "$member is not in ${url##*/}"
else
	mv -f "$asset" "$binary"
fi
chmod 755 "$binary"
mv -f "$binary" "$dest/$name"
echo "$name: installed ${url##*/} into $dest (SHA-256 matched$verified)"
