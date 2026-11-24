#!/bin/sh
# Checks the two allow lists, the only places a scanner finding may be silenced (README, "Allow lists").
#
#   check-allowlists.sh           the repository's .betterleaks.toml and .trivyignore.yaml, the absence of every other
#                                 ignore file, and the probe: the secret scan must still report a planted token
#   check-allowlists.sh FILE...   only the layout, fields and dates of the given files (*.toml as Betterleaks, *.yaml as
#                                 Trivy), for the fixtures that prove this check
#
# Each file must be in its canonical layout, line by line; anything else fails, because a shell check cannot parse
# general TOML or YAML and must not pass what it does not understand. An entry is expired from 00:00 UTC on its date.
# BETTERLEAKS names the pinned binary (the probe needs it). Prints one line per problem and exits 1, or one summary.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd -P)
today=$(date -u +%Y-%m-%d)

# Shared by both layouts: real calendar dates, and the expiry rule.
awk_dates='
function valid_date(d,    y, m, n) {
	if (d !~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/) return 0
	y = substr(d, 1, 4) + 0; m = substr(d, 6, 2) + 0; n = substr(d, 9, 2) + 0
	if (m < 1 || m > 12 || n < 1) return 0
	if (m == 2) return n <= ((y % 4 == 0 && y % 100 != 0) || y % 400 == 0 ? 29 : 28)
	return n <= ((m == 4 || m == 6 || m == 9 || m == 11) ? 30 : 31)
}
function problem(line, text) { printf "%s:%d: %s\n", FILENAME, line, text; bad = 1 }
function check_date(line, id, d) {
	if (!valid_date(d)) problem(line, id ": malformed date")
	else if (d <= today) problem(line, id ": expired on " d)
}
'

# .betterleaks.toml: the [extend] header, then entries of description, paths, regexes and condition, in that order;
# the file's own header says why an entry is global and narrowed to one file and one exact value.
check_betterleaks() {
	awk -v today="$today" "$awk_dates"'
	function literal(v) { return v ~ /^\^([A-Za-z0-9\/_@:=-]|\\[.+\/-])+\$$/ }
	function close_entry() {
		if (state != "" && state != "condition") problem(start, id ": missing field " next_field[state])
		state = ""
	}
	BEGIN { next_field["entry"] = "description"; next_field["description"] = "paths"
		next_field["paths"] = "regexes"; next_field["regexes"] = "condition" }
	/^[ \t]*#.*betterleaks\/betterleaks#300/ { noted = 1 }
	/^[ \t]*(#.*)?$/ { next }
	header < 2 {
		if (header == 0 && $0 == "[extend]") { header = 1; next }
		if (header == 1 && $0 == "useDefault = true") { header = 2; next }
		problem(NR, "not the canonical layout: the file starts with [extend] and useDefault = true"); exit
	}
	$0 == "[[allowlists]]" {
		if (!noted) problem(NR, "not the canonical layout: the header comment naming betterleaks/betterleaks#300 is missing")
		noted = 1; close_entry(); state = "entry"; start = NR; id = "entry at line " NR; next
	}
	/^\[/ { problem(NR, "table not allowed (only [extend] and [[allowlists]])"); next }
	/^[A-Za-z]+ = / && $1 !~ /^(description|paths|regexes|condition)$/ {
		problem(NR, "key not allowed (an entry has only description, paths, regexes and condition)"); next
	}
	state == "entry" && /^description = "BL-[0-9]+: .*; expires=[^"]*"$/ {
		text = $0; sub(/^description = "/, "", text); sub(/"$/, "", text)
		id = text; sub(/:.*/, "", id)
		date = text; sub(/.*; expires=/, "", date)
		reason = text; sub(/^[^:]*: /, "", reason); sub(/; expires=[^;]*$/, "", reason)
		if (reason == "" || reason ~ /["\\]|expires=/) problem(NR, id ": the reason is empty or holds \", \\ or expires=")
		if (seen[id]++) problem(NR, id ": duplicate ID")
		check_date(NR, id, date); state = "description"; next
	}
	state == "description" && /^paths = \[\047[^\047]*\047\]$/ {
		if (!literal(substr($0, 11, length($0) - 12))) problem(NR, id ": path not an anchored literal (one file)")
		state = "paths"; next
	}
	state == "paths" && /^regexes = \[\047[^\047]*\047\]$/ {
		if (!literal(substr($0, 13, length($0) - 14))) problem(NR, id ": regex not an anchored literal (one exact value)")
		state = "regexes"; next
	}
	state == "regexes" && $0 == "condition = \"AND\"" { state = "condition"; next }
	{ problem(NR, "not the canonical layout" (state == "" ? "" : ", expected " next_field[state] " of " id)) }
	END {
		if (header < 2) { if (!bad) problem(NR, "not the canonical layout: no [extend] header") }
		else if (!noted) problem(NR, "not the canonical layout: the header comment naming betterleaks/betterleaks#300 is missing")
		close_entry(); exit bad
	}
	' "$1"
}

# .trivyignore.yaml: "vulnerabilities: []", or "vulnerabilities:" and entries of id, statement and expired_at.
check_trivy() {
	awk -v today="$today" "$awk_dates"'
	function close_entry() {
		if (state == "id") problem(start, id ": missing field statement")
		else if (state == "statement") problem(start, id ": missing field expired_at")
		state = ""
	}
	/^[ \t]*(#.*)?$/ { next }
	top == "" && $0 == "vulnerabilities: []" { top = "empty"; next }
	top == "" && $0 == "vulnerabilities:" { top = "list"; next }
	top != "list" { problem(NR, "not the canonical layout"); next }
	/^  - id: [A-Z]+-[A-Za-z0-9._-]+$/ {
		close_entry(); id = "entry at line " NR; start = NR; entries++
		if (seen[substr($0, 9)]++) problem(NR, id ": duplicate ID")
		state = "id"; next
	}
	state == "id" && /^    statement: "[^"\\]+"$/ { state = "statement"; next }
	state == "statement" && /^    expired_at: [^ ]+$/ { check_date(NR, id, substr($0, 17)); state = ""; next }
	state != "" && /^    statement:/ { problem(NR, id ": malformed statement"); next }
	state != "" && /^    expired_at:/ { problem(NR, id ": malformed expired_at"); next }
	/^  - id:/ { problem(NR, "malformed id"); next }
	{ problem(NR, "not the canonical layout") }
	END {
		if (top == "") problem(NR, "not the canonical layout: no vulnerabilities key")
		else if (top == "list" && !entries) problem(NR, "not the canonical layout: an empty list is written vulnerabilities: []")
		close_entry(); exit bad
	}
	' "$1"
}

check_file() {
	case $1 in
	*.toml) check_betterleaks "$1" ;;
	*.yaml) check_trivy "$1" ;;
	*) echo "$1: neither a Betterleaks (.toml) nor a Trivy (.yaml) allow list"; return 1 ;;
	esac
}

# The probe: a throw-away repository where one commit modifies a file and adds another, and a merge commit adds a third
# of its own, each line holding a fresh github-pat token and an inline allow comment; the real range scan must report
# all three. It runs with Git's repository variables cleared and no global or system configuration, so inside a
# pre-commit hook it never touches the commit being made, and it stops at the first failed step: it runs under "!",
# where set -e does not apply.
probe() (
	repository_variables=$(git rev-parse --local-env-vars) && [ -n "$repository_variables" ] || return 1
	unset $repository_variables || return 1
	export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
	dir=$(mktemp -d) && [ -d "$dir" ] || return 1
	trap 'rm -rf "$dir"' EXIT
	token() { printf 'ghp_0123456789abcdef0123456789abcdef%s' "$(LC_ALL=C tr -dc 0-9a-f </dev/urandom | head -c 4)"; }
	plant() { printf 'token = "%s" # gitleaks:allow betterleaks:allow\n' "$(token)" >>"$dir/$1"; }
	in_probe() { git -C "$dir" -c user.name=probe -c user.email=probe@example.invalid -c commit.gpgsign=false \
		-c core.hooksPath=/dev/null "$@"; }
	git -c init.defaultBranch=main init -q "$dir" && echo base >"$dir/modified.txt" &&
		in_probe add -A && in_probe commit -q -m base || return 1
	base=$(in_probe rev-parse HEAD) || return 1
	in_probe checkout -q -b side && echo side >"$dir/side.txt" && in_probe add -A && in_probe commit -q -m side &&
		in_probe checkout -q main && plant modified.txt && plant added.txt && in_probe add -A && in_probe commit -q -m probe &&
		in_probe merge -q --no-ff --no-commit side >/dev/null 2>&1 && plant merged.txt && in_probe add -A && in_probe commit -q -m merge ||
		return 1
	report=$(sh "$root/scripts/scan-secrets.sh" history "$base" "$dir" 2>/dev/null) && return 1
	for file in added modified merged; do
		printf '%s\n' "$report" | grep -q "^github-pat $file\.txt:" || return 1
	done
)

status=0
if [ $# -gt 0 ]; then
	for file in "$@"; do check_file "$file" || status=1; done
	exit $status
fi

: "${BETTERLEAKS:?BETTERLEAKS must name the pinned betterleaks binary (the probe needs it)}"
cd "$root"
for ignore in .betterleaksignore .gitleaksignore .trivyignore; do
	if [ -e "$ignore" ]; then echo "$ignore: ignore file present; the only allow lists are .betterleaks.toml and .trivyignore.yaml"; status=1; fi
done
for list in .betterleaks.toml .trivyignore.yaml; do
	if [ -f "$list" ]; then check_file "$list" || status=1; else echo "$list: file missing"; status=1; fi
done
if [ $status -eq 0 ] && ! probe; then
	echo "probe did not fire: the secret scan did not report a planted github-pat token through scripts/scan-secrets.sh"
	status=1
fi
[ $status -ne 0 ] || echo "allow lists: canonical, none expired, probe fired"
exit $status
