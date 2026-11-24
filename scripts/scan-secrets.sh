#!/bin/sh
# Scans for secrets with the repository's Betterleaks configuration. Each finding prints as rule, file:line and commit
# (scripts/findings.tmpl): never the value, the source line, the author or the commit message. This is the one place
# the scan's options are spelled; the hook, the Secrets workflow and the allow-list check's probe all run it.
#
#   scan-secrets.sh staged             what is staged for the commit being made (the pre-commit hook)
#   scan-secrets.sh history SINCE [REPOSITORY]
#                                      every commit in SINCE..HEAD, merge commits against their first parent, in the
#                                      current directory's repository or (for the probe) the one named
#
# BETTERLEAKS names the pinned binary. The configuration and the template are this repository's, by absolute path,
# wherever the scan runs. Exit 1 on a finding, on a scan error, on an unusable SINCE and on a shallow clone.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd -P)
betterleaks=${BETTERLEAKS:?BETTERLEAKS must name the pinned betterleaks binary}
source=.

fail() {
	echo "secret scan: $*" >&2
	exit 1
}

scan() {
	exec "$betterleaks" git "$source" "$@" \
		--config "$root/.betterleaks.toml" --redact --ignore-gitleaks-allow --no-banner \
		--report-format template --report-template "$root/scripts/findings.tmpl" --report-path -
}

# SINCE must be a full commit ID present in the clone; anything else is refused before git sees it.
check_since() {
	case $1 in
	*[!0-9a-f]* | '') fail "SINCE is not a commit ID (40 lowercase hexadecimal digits); it is not shown" ;;
	esac
	[ ${#1} -eq 40 ] || fail "SINCE is not a commit ID (40 lowercase hexadecimal digits); it is not shown"
	git -C "$source" rev-parse -q --verify "$1^{commit}" >/dev/null || fail "SINCE $1 is not a commit in this clone"
}

case "${1:-}:$#" in
staged:1)
	scan --pre-commit --staged
	;;
history:2 | history:3)
	source=${3:-.}
	[ "$(git -C "$source" rev-parse --is-shallow-repository)" = false ] ||
		fail "the clone is shallow: check out the full history (actions/checkout fetch-depth: 0)"
	check_since "$2"
	echo "scanning $2..HEAD"
	scan --log-opts="--full-history --diff-filter=uxdb --diff-merges=first-parent $2..HEAD"
	;;
*)
	fail "usage: scan-secrets.sh staged | history SINCE [REPOSITORY]"
	;;
esac
