#!/usr/bin/env bash
# Prints the Go modules this repository builds from, one directory per line,
# the root (".") first: the root module and every module the root go.mod
# replaces with a directory inside the tree (RFC FE). Tools run from the root
# do not see a nested module's packages, so anything that lists, vets, tests
# or tidies Go code iterates these.
#
#   scripts/ci/modules.sh                 # directories
#   scripts/ci/modules.sh -group PKG...   # "DIR PKG..." per module owning a package
#
# The discovery itself is Go (scripts/ci/gomodules), shared with
# scripts/ci/affected and internal/archtest, so all of them agree.
set -euo pipefail
cd "$(dirname "$0")/../.."
exec go run ./scripts/ci/modules "$@"
