#!/usr/bin/env bash
# apibudget.sh — the chok API surface budget (plan §6); `make budget`.
#
# For every public package (neither internal nor main) it counts
#   exports       `go doc -short` lines, indentation stripped, that start
#                 with func|type|var|const: constructors are listed indented
#                 under their type, methods are not listed, and a grouped
#                 var/const block is one line
#   option ctors  those func lines whose result type is Option, RouteOption
#                 or LoadOption; store/where is skipped, its predicates are
#                 a query DSL rather than options (plan §6)
# plus the module's direct dependencies and, once examples/blog has Go
# files, its application lines (non-test files; blank and // lines not
# counted) and the distinct chok packages it imports.
#
# Limits live in hack/budget.txt. The table is always printed; a count over
# its limit, or a public package missing from budget.txt, exits 1.
# Portable to the bash 3.2 that macOS ships.
set -euo pipefail

cd "$(dirname "$0")/.."
budget=hack/budget.txt
failed=0

limit_of() { awk -v k="$1" '$1 == "limit" && $2 == k { print $3 }' "$budget"; }
kind_of() { awk -v p="$1" '($1 == "pkg" || $1 == "provider") && $2 == p { print $1 }' "$budget"; }
keep() { grep -E "$@" || true; } # grep that may match nothing under pipefail
count() { grep -c . || true; }   # non-empty lines on stdin

row() { # item actual limit
	local status=ok
	if [ "$2" = "-" ]; then
		status=absent
	elif [ "$3" != "-" ] && [ "$2" -gt "$3" ]; then
		status=OVER
		failed=1
	fi
	printf '%-36s %7s %7s  %s\n' "$1" "$2" "$3" "$status"
}

# Every go command runs in a plain assignment, so set -e stops on its
# failure instead of reporting an empty, passing budget.
module=$(go list -m)
listed=$(go list -f '{{if ne .Name "main"}}{{.ImportPath}}{{end}}' ./...)
deps=$(go list -m -f '{{if not (or .Main .Indirect)}}{{.Path}}{{end}}' all)

public=0 exports_total=0 options_total=0 unlisted=""
counts="" # lines of "<rel> <exports>"
for ip in $(printf '%s\n' "$listed" | keep -v '(^|/)internal(/|$)'); do
	rel=${ip#"$module"}
	rel=${rel#/}
	[ -n "$rel" ] || rel=chok
	kind=$(kind_of "$rel")
	if [ -z "$kind" ]; then
		unlisted="$unlisted $rel"
	elif [ "$kind" = pkg ]; then
		public=$((public + 1))
	fi
	doc=$(go doc -short "$ip")
	doc=$(printf '%s\n' "$doc" | sed 's/^[[:space:]]*//')
	n=$(printf '%s\n' "$doc" | keep '^(func|type|var|const) ' | count)
	o=$(printf '%s\n' "$doc" | keep '^func .*\) (Option|RouteOption|LoadOption)$' | count)
	exports_total=$((exports_total + n))
	[ "$rel" = store/where ] || options_total=$((options_total + o))
	counts="$counts$rel $n
"
done

exports_of() { printf '%s' "$counts" | awk -v p="$1" '$1 == p { print $2; found = 1 } END { if (!found) print "-" }'; }

printf 'chok API budget (limits in %s)\n\n' "$budget"
printf '%-36s %7s %7s  %s\n' ITEM ACTUAL LIMIT STATUS
row "public packages (providers excluded)" "$public" "$(limit_of public_packages)"
while read -r kind path cap; do
	case "$kind" in
	pkg) row "exports $path" "$(exports_of "$path")" "$cap" ;;
	provider) row "exports $path (provider)" "$(exports_of "$path")" "${cap:--}" ;;
	esac
done <"$budget"
for rel in $unlisted; do
	row "exports $rel (UNLISTED)" "$(exports_of "$rel")" -
done
row "exports total" "$exports_total" "$(limit_of exports_total)"
row "option constructors" "$options_total" "$(limit_of option_ctors)"
row "direct dependencies" "$(printf '%s\n' "$deps" | count)" "$(limit_of direct_deps)"

blog_files=""
if [ -d examples/blog ]; then
	blog_files=$(find examples/blog -name '*.go' ! -name '*_test.go' ! -path '*/testdata/*' | sort)
fi
lines=- imports=-
if [ -n "$blog_files" ]; then
	# shellcheck disable=SC2086 # file names are plain paths
	lines=$(cat $blog_files | keep -v '^[[:space:]]*(//.*)?$' | count)
	# shellcheck disable=SC2086
	imports=$(cat $blog_files | keep -o '"[^"]+"' | tr -d '"' |
		awk -v m="$module" '$0 == m || index($0, m "/") == 1' | sort -u | count)
fi
row "examples/blog app lines" "$lines" "$(limit_of blog_app_lines)"
row "examples/blog chok imports" "$imports" "$(limit_of blog_chok_imports)"

if [ -n "$unlisted" ]; then
	echo
	echo "unlisted public package(s):$unlisted — plan §1 D3 allows only the packages in $budget"
	failed=1
fi
echo
if [ "$failed" -ne 0 ]; then
	echo "budget: FAILED"
	exit 1
fi
echo "budget: ok"
