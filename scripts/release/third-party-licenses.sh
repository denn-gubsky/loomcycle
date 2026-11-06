#!/usr/bin/env bash
# Writes THIRD_PARTY_LICENSES: the license texts of every third-party Go module
# linked into the loomcycle binary and of every npm package bundled into its
# embedded Web UI. The release archives carry it (.goreleaser.yaml runs this as
# a before hook); CI runs the Go half on every Go change, so a dependency whose
# license cannot be classified fails the pull request, not the release.
#
#   scripts/release/third-party-licenses.sh OUT            # Go modules + Web UI
#   scripts/release/third-party-licenses.sh --go-only OUT  # Go modules only
#
# Run from anywhere; paths resolve against the repository root. The Web UI half
# needs the npm installs `make build-ui` makes (web/ and packages/*): it runs
# the same Vite build again into a scratch directory with Vite's license notice
# turned on, so internal/webui/dist stays exactly what the binary embeds.
#
# Network: module and npm downloads only. go-licenses `save` reads the module
# cache; `report` is not used because it looks up every license's URL online.
set -euo pipefail

# Not in go.mod, so dependabot does not bump it; bump it by hand.
GO_LICENSES_VERSION="v2.0.1"

go_only=false
if [ "${1:-}" = "--go-only" ]; then
  go_only=true
  shift
fi
if [ $# -ne 1 ]; then
  echo "usage: third-party-licenses.sh [--go-only] OUT" >&2
  exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out_dir="$(mkdir -p "$(dirname "$1")" && cd "$(dirname "$1")" && pwd)"
out="$out_dir/$(basename "$1")"
cd "$root"

if ! $go_only && [ ! -d web/node_modules ]; then
  echo "third-party-licenses: web/node_modules is missing; run 'make build-ui' first (or pass --go-only)" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT

# go-licenses tells the standard library apart by its own GOROOT, so it must be
# built by the same go that loads the packages: a prebuilt binary from another
# Go release reports every stdlib package as a module-less dependency and stops.
# Built for the host (GOOS/GOARCH unset) because it runs here.
env -u GOOS -u GOARCH GOBIN="$tmp/bin" go install "github.com/google/go-licenses/v2@${GO_LICENSES_VERSION}"

# Every platform the release builds (.goreleaser.yaml builds:). The module set
# barely differs between them, but a platform-only dependency must be listed.
platforms="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

for p in $platforms; do
  goos="${p%/*}"
  goarch="${p#*/}"
  # loomcycle's own packages (and its in-tree nested modules) are Apache-2.0,
  # shipped as LICENSE + NOTICE. goja/ftoa carries the Lucent dtoa permission
  # notice (LICENSE_LUCENE), which go-licenses cannot classify; it is a
  # permissive notice and is added by hand below.
  if ! GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 "$tmp/bin/go-licenses" save ./cmd/loomcycle \
    --save_path "$tmp/save-$goos-$goarch" \
    --ignore github.com/denn-gubsky/loomcycle \
    --ignore github.com/dop251/goja/ftoa \
    2> "$tmp/save-$goos-$goarch.log"; then
    # Only the errors: the log is otherwise thousands of "non-Go code" notes.
    grep -E '^[EF][0-9]{4} ' "$tmp/save-$goos-$goarch.log" >&2 || cat "$tmp/save-$goos-$goarch.log" >&2
    echo "third-party-licenses: go-licenses failed for $goos/$goarch" >&2
    exit 1
  fi
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go list -deps \
    -f '{{with .Module}}{{if not .Main}}{{.Path}} {{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}{{end}}{{end}}' \
    ./cmd/loomcycle >> "$tmp/modules"
done

goja_dir="$(go list -m -f '{{.Dir}}' github.com/dop251/goja)"
if [ ! -f "$goja_dir/ftoa/LICENSE_LUCENE" ]; then
  echo "third-party-licenses: $goja_dir/ftoa/LICENSE_LUCENE is gone; drop the goja/ftoa special case" >&2
  exit 1
fi

# The license files go-licenses found, one list across platforms. `save` also
# copies the full source of reciprocally licensed packages; only the notice
# files are wanted here.
for d in "$tmp"/save-*/; do
  (cd "$d" && find . -type f \( -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \))
done | sed 's|^\./||' | LC_ALL=C sort -u > "$tmp/files"

rule="--------------------------------------------------------------------------------"
{
  echo "loomcycle: third-party licenses"
  echo "==============================="
  echo
  echo "loomcycle is licensed under the Apache License 2.0 (see LICENSE and NOTICE)."
  echo "The loomcycle binary links the Go modules listed below and embeds a Web UI"
  echo "that bundles the npm packages listed at the end. Their license texts follow."
  echo "All are used unmodified; the source of each is available from the Go module"
  echo "proxy (https://proxy.golang.org) or the npm registry at the version listed."
  echo
  echo "Go modules"
  echo "----------"
  echo
  grep -v '^github.com/denn-gubsky/loomcycle' "$tmp/modules" | LC_ALL=C sort -u
  echo
  echo "Go module license texts"
  echo "-----------------------"
  while IFS= read -r f; do
    src=""
    for d in "$tmp"/save-*/; do
      if [ -f "$d$f" ]; then
        src="$d$f"
        break
      fi
    done
    echo
    echo "$rule"
    echo "$(dirname "$f")  ($(basename "$f"))"
    echo "$rule"
    echo
    cat "$src"
  done < "$tmp/files"
  echo
  echo "$rule"
  echo "github.com/dop251/goja/ftoa  (LICENSE_LUCENE)"
  echo "$rule"
  echo
  cat "$goja_dir/ftoa/LICENSE_LUCENE"
} > "$tmp/out"

if ! $go_only; then
  # Vite's build.license writes the name, version and license text of every npm
  # package that ends up in the bundle, which is the set that matters here (not
  # the dev dependencies). Inline config is merged over web/vite.config.ts.
  (
    cd web
    LICENSE_OUT="$tmp/web" node --input-type=module -e '
      import { build } from "vite";
      await build({
        configFile: "vite.config.ts",
        logLevel: "warn",
        build: {
          outDir: process.env.LICENSE_OUT,
          emptyOutDir: true,
          license: { fileName: "THIRD_PARTY_LICENSES.md" },
        },
      });
    '
  )
  if [ ! -s "$tmp/web/THIRD_PARTY_LICENSES.md" ]; then
    echo "third-party-licenses: the Vite build wrote no license file" >&2
    exit 1
  fi
  {
    echo
    echo
    echo "Web UI: npm packages bundled into the embedded single-page app"
    echo "--------------------------------------------------------------"
    echo
    cat "$tmp/web/THIRD_PARTY_LICENSES.md"
  } >> "$tmp/out"
fi

mv "$tmp/out" "$out"
echo "third-party-licenses: wrote $out" >&2
