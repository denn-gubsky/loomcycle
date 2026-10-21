#!/usr/bin/env bash
# Run one npm task across every JavaScript package in the repo.
#
# The package list is derived from the TRACKED lockfiles
# (`git ls-files '*package-lock.json'`), so a new package is picked up the
# moment its package-lock.json is committed — there is no second list to
# forget to update.
#
# Deliberately NOT an npm/pnpm/yarn workspace: each package keeps its own
# lockfile and consumes a sibling (e.g. @loomcycle/client, bridge-core) by its
# PUBLISHED npm version, never its source. A workspace would symlink siblings,
# so local runs and CI would test a dependency graph npm users never get.
#
# Bash 3.2-compatible (the macOS /bin/bash): no associative arrays, no mapfile.

set -u
set -o pipefail

usage() {
	cat <<'EOF'
usage: scripts/js-packages.sh <command> [options]

Commands:
  list        print the packages (and warn about a package.json with no lockfile)
  install     npm ci
  build       npm run --if-present build
  test        npm run --if-present test
  typecheck   npm run --if-present typecheck
  lint        npm run --if-present lint
  audit       npm audit --omit=dev --audit-level=high
  outdated    npm outdated (report only; never fails)

Options:
  -p, --package PAT  only packages whose path contains PAT (repeatable; any match)
  -k, --keep-going   continue past a failing package and summarize at the end
  -h, --help         show this help

A package without the script is reported as "skip", not a failure.
build/test/typecheck/lint need node_modules: run `install` first. web/ also
compiles packages/{library,explorer,memory-view,def-fields} from source, so
install those before checking web/ alone.

Exit status: 0 if every selected package passed or skipped, 1 if any failed,
2 on a usage error.
EOF
}

die_usage() {
	printf 'js-packages: %s\n' "$1" >&2
	printf "run 'scripts/js-packages.sh --help' for usage\n" >&2
	exit 2
}

# Packages excluded from every command, by repo-relative path. Empty today;
# add a path here (with the reason) rather than deleting a lockfile.
EXCLUDE=""

cmd=""
keep_going=0
filters=()

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	-k | --keep-going) keep_going=1 ;;
	-p | --package)
		[ $# -ge 2 ] || die_usage "$1 needs a value"
		filters+=("$2")
		shift
		;;
	--package=*) filters+=("${1#--package=}") ;;
	-*) die_usage "unknown option: $1" ;;
	*)
		[ -z "$cmd" ] || die_usage "unexpected argument: $1"
		cmd="$1"
		;;
	esac
	shift
done

case "$cmd" in
list | install | build | test | typecheck | lint | audit | outdated) ;;
"") die_usage "no command given" ;;
*) die_usage "unknown command: $cmd" ;;
esac

script_dir=$(cd "$(dirname "$0")" && pwd) || exit 2
root=$(git -C "$script_dir" rev-parse --show-toplevel) || exit 2

matches_filter() {
	[ ${#filters[@]} -eq 0 ] && return 0
	local f
	for f in "${filters[@]}"; do
		case "$1" in *"$f"*) return 0 ;; esac
	done
	return 1
}

is_excluded() {
	local e
	for e in $EXCLUDE; do
		[ "$1" = "$e" ] && return 0
	done
	return 1
}

pkgs=()
while IFS= read -r lock; do
	[ -n "$lock" ] || continue
	dir=$(dirname "$lock")
	is_excluded "$dir" && continue
	matches_filter "$dir" || continue
	pkgs+=("$dir")
done <<EOF
$(git -C "$root" ls-files -- '*package-lock.json')
EOF

# A tracked package.json with no tracked lockfile would be silently left out
# of every command (and `npm ci` refuses it anyway) — say so on every run.
while IFS= read -r manifest; do
	[ -n "$manifest" ] || continue
	dir=$(dirname "$manifest")
	is_excluded "$dir" && continue
	if ! git -C "$root" ls-files --error-unmatch -- "$dir/package-lock.json" >/dev/null 2>&1; then
		printf 'js-packages: warning: %s/package.json has no tracked package-lock.json; it is NOT covered\n' "$dir" >&2
	fi
done <<EOF
$(git -C "$root" ls-files -- '*package.json' | grep -E '(^|/)package\.json$')
EOF

if [ ${#pkgs[@]} -eq 0 ]; then
	die_usage "no package matches the filter"
fi

if [ "$cmd" = list ]; then
	printf '%s\n' "${pkgs[@]}"
	exit 0
fi

has_script() {
	node -e 'const s = (require(process.argv[1]).scripts || {}); process.exit(Object.prototype.hasOwnProperty.call(s, process.argv[2]) ? 0 : 1)' \
		"$root/$1/package.json" "$2"
}

tmp=$(mktemp -d) || exit 2
trap 'rm -rf "$tmp"' EXIT

# Parallel arrays (bash 3.2 has no maps): one row per package run.
res_pkg=()
res_status=()
res_secs=()
res_note=()
failed=0
total=${#pkgs[@]}
i=0

for pkg in "${pkgs[@]}"; do
	i=$((i + 1))
	status=ok
	note=""
	log="$tmp/out.log"
	: >"$log"

	case "$cmd" in
	install) run=(npm ci) ;;
	audit) run=(npm audit --omit=dev --audit-level=high) ;;
	outdated) run=(npm outdated) ;;
	*) run=(npm run --if-present "$cmd") ;;
	esac

	printf '\n==> [%d/%d] %s: %s\n' "$i" "$total" "$pkg" "${run[*]}"
	start=$(date +%s)

	case "$cmd" in
	build | test | typecheck | lint)
		if ! has_script "$pkg" "$cmd"; then
			status=skip
			note="no \"$cmd\" script"
		elif [ ! -d "$root/$pkg/node_modules" ]; then
			status=FAIL
			note="no node_modules; run install first"
		fi
		;;
	esac

	if [ "$status" = ok ]; then
		(cd "$root/$pkg" && "${run[@]}") 2>&1 | tee "$log"
		rc=$?
		case "$cmd" in
		outdated)
			# npm outdated exits 1 whenever anything is outdated; this is a report.
			if [ "$rc" -eq 0 ]; then note="up to date"; else note="outdated (see above)"; fi
			;;
		audit)
			note=$(grep -E 'vulnerabilit' "$log" | tail -n 1)
			[ "$rc" -eq 0 ] || status=FAIL
			;;
		*) [ "$rc" -eq 0 ] || status=FAIL ;;
		esac
		[ "$status" = FAIL ] && [ -z "$note" ] && note="exit $rc"
	else
		[ -n "$note" ] && printf '%s: %s\n' "$status" "$note"
	fi

	res_pkg+=("$pkg")
	res_status+=("$status")
	res_secs+=("$(($(date +%s) - start))s")
	res_note+=("$note")

	if [ "$status" = FAIL ]; then
		failed=$((failed + 1))
		[ "$keep_going" -eq 1 ] || break
	fi
done

printf '\n==> js-packages %s: summary\n' "$cmd"
printf '%-28s %-6s %6s  %s\n' PACKAGE RESULT TIME NOTE
j=0
while [ $j -lt ${#res_pkg[@]} ]; do
	printf '%-28s %-6s %6s  %s\n' "${res_pkg[$j]}" "${res_status[$j]}" "${res_secs[$j]}" "${res_note[$j]}"
	j=$((j + 1))
done
not_run=$((total - ${#res_pkg[@]}))
[ "$not_run" -gt 0 ] && printf '(%d package(s) not run: stopped at the first failure; use --keep-going)\n' "$not_run"

if [ "$failed" -gt 0 ]; then
	printf '%d of %d package(s) failed\n' "$failed" "$total"
	exit 1
fi
exit 0
