#!/usr/bin/env bash
# Prints a -run regexp that selects part I of N of PACKAGE's top-level tests,
# for splitting one package's tests across CI runners.
#
#   split-tests.sh PACKAGE I/N [go test build flags...]
#
# The names come from `go test -list`, built with the same flags as the run so
# the run reuses the build. They are sorted and dealt round-robin, so every
# test lands in exactly one part: the parts together are the whole package,
# and a test added later joins a part with no list to update. Examples and
# fuzz seed corpora run as tests too, so they are dealt out the same way.
set -euo pipefail

pkg="${1:?usage: split-tests.sh PACKAGE I/N [build flags...]}"
part="${2:?usage: split-tests.sh PACKAGE I/N [build flags...]}"
shift 2
i="${part%/*}"
n="${part#*/}"
if ! [[ "$i" =~ ^[0-9]+$ && "$n" =~ ^[0-9]+$ ]] || [ "$i" -lt 1 ] || [ "$i" -gt "$n" ]; then
  echo "split-tests.sh: part must be I/N with 1 <= I <= N, got $part" >&2
  exit 2
fi

names=$(go test "$@" -list '.*' "$pkg" | grep -E '^(Test|Example|Fuzz)[A-Za-z0-9_]*$' | sort)
if [ -z "$names" ]; then
  echo "split-tests.sh: $pkg lists no tests" >&2
  exit 1
fi
mine=$(awk -v i="$i" -v n="$n" '(NR - 1) % n == i - 1' <<<"$names" | paste -sd'|' -)
if [ -z "$mine" ]; then
  # More parts than tests: this part has nothing. A pattern that matches no
  # name runs nothing, which is what this part should do.
  echo '^$'
  exit 0
fi
echo "^($mine)\$"
