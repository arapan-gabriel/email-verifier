#!/bin/sh
# coverage-gate.sh -- per-package statement coverage, held to a ratchet (plan 027).
#
#   scripts/coverage-gate.sh              run the suite, print, enforce the floors
#   COVERPROFILE=cover.out scripts/coverage-gate.sh   keep the profile
#   FLOORS=path/to/floors scripts/coverage-gate.sh    use another floors file
#
# Each package is counted by ITS OWN tests only: no -coverpkg. A package is
# proven by its tests, not by a caller that happens to walk through it.
#
# The floors live in scripts/coverage-floors.txt, one "<package-dir> <percent>"
# per line plus "total <percent>" for the service as a whole. They are whole
# numbers and only move up; a change that lowers one says why in its PR.
#
# internal/mxsim/* and cmd/mxsim are excluded: ported test infrastructure, as
# in .golangci.yml.
#
# POSIX sh plus awk and the go tool. No new Go dependency.
set -eu

cd "$(dirname "$0")/.."

floors=${FLOORS:-scripts/coverage-floors.txt}
[ -f "$floors" ] || { echo "coverage-gate: no floors file at $floors" >&2; exit 2; }

module=$(go list -m)

if [ -n "${COVERPROFILE:-}" ]; then
	profile=$COVERPROFILE
	keep=1
else
	profile=$(mktemp "${TMPDIR:-/tmp}/coverage.XXXXXX")
	keep=0
fi
cleanup() { [ "$keep" = 1 ] || rm -f "$profile"; }
trap cleanup EXIT INT TERM

# A failing test fails the gate too: coverage from a red suite measures nothing.
go test -count=1 -covermode=atomic -coverprofile="$profile" ./... >/dev/null

awk -v module="$module/" -v floors="$floors" '
BEGIN {
	while ((getline line < floors) > 0) {
		sub(/#.*/, "", line)
		n = split(line, f, /[ \t]+/)
		if (n < 2) {
			if (n == 1 && f[1] != "") { printf "coverage-gate: bad floors line: %s\n", line; bad = 1 }
			continue
		}
		# split() leaves an empty first field when the line starts with blanks.
		k = (f[1] == "") ? f[2] : f[1]
		v = (f[1] == "") ? f[3] : f[2]
		floor[k] = v + 0
		order[++nfloors] = k
	}
}
NR == 1 && /^mode:/ { next }
{
	# file.go:1.2,3.4 <stmts> <count>. The same block can appear more than
	# once in a merged profile; it counts once, covered if any copy was hit.
	key = $1
	stmts[key] = $2
	if (($3 + 0) > 0) hit[key] = 1
}
END {
	for (key in stmts) {
		file = key
		sub(/:.*/, "", file)
		sub("^" module, "", file)
		pkg = file
		sub(/\/[^\/]*$/, "", pkg)
		if (pkg ~ /^internal\/mxsim(\/|$)/ || pkg == "cmd/mxsim") continue
		total[pkg] += stmts[key]
		all += stmts[key]
		if (key in hit) { covered[pkg] += stmts[key]; allc += stmts[key] }
	}

	fail = bad
	printf "%-24s %7s %7s %7s  %s\n", "package", "stmts", "cover", "floor", ""
	# Sorted output without gawk: a simple insertion sort over package names.
	n = 0
	for (p in total) names[++n] = p
	for (i = 2; i <= n; i++) {
		v = names[i]
		for (j = i - 1; j >= 1 && names[j] > v; j--) names[j + 1] = names[j]
		names[j + 1] = v
	}
	for (i = 1; i <= n; i++) {
		p = names[i]
		# Truncated, never rounded, both for the comparison and for display:
		# 75.99 must not pass a 76 floor, and must not print as "76.0" beside
		# a BELOW FLOOR either.
		pct = int(1000 * covered[p] / total[p]) / 10
		status = ""
		if (p in floor) {
			if (pct < floor[p]) { status = "BELOW FLOOR"; fail = 1 } else status = "ok"
			printf "%-24s %7d %6.1f%% %6d%%  %s\n", p, total[p], pct, floor[p], status
		} else {
			printf "%-24s %7d %6.1f%% %7s  %s\n", p, total[p], pct, "-", status
		}
		seen[p] = 1
	}
	tp = (all > 0) ? int(1000 * allc / all) / 10 : 0
	status = ""
	if ("total" in floor) {
		if (tp < floor["total"]) { status = "BELOW FLOOR"; fail = 1 } else status = "ok"
		printf "%-24s %7d %6.1f%% %6d%%  %s\n", "total", all, tp, floor["total"], status
	} else {
		printf "%-24s %7d %6.1f%%\n", "total", all, tp
	}
	# A floor for a package that no longer exists (or produced no profile) is
	# a stale gate, and a stale gate is one nobody reads.
	for (i = 1; i <= nfloors; i++) {
		k = order[i]
		if (k != "total" && !(k in seen)) { printf "coverage-gate: floor for %s, but no coverage for it\n", k; fail = 1 }
	}
	if (fail) { print "coverage-gate: FAIL"; exit 1 }
	print "coverage-gate: ok"
}
' "$profile"
