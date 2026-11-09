#!/usr/bin/env bash
# Fails when a retired LOOMCYCLE_OTEL_* tracing variable is named anywhere in
# the tracked tree.
#
# Tracing moved to the standard OTEL_* variables with no fallback: a guide,
# example or deploy file that still says LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT
# leaves the operator's tracing silently off. LOOMCYCLE_OTEL_EXPORTER_OTLP_
# HEADERS_FILE is current and does not match.
#
# A line that names a retired variable on purpose — the rename table in
# docs/CONFIGURATION.md, the infrastructure-secret denylist that keeps the old
# headers name from being interpolated, the tests pinning both — says
# "retired" on the same line and is allowed. examples/exp10-gbash-bench/work/
# is a frozen copy of an older tree.
set -euo pipefail

cd "$(dirname "$0")/../.."

pattern='LOOMCYCLE_OTEL_(EXPORTER_OTLP_ENDPOINT|SERVICE_NAME|TRACES_SAMPLER_RATIO|SAMPLER_RATIO|EXPORTER_OTLP_HEADERS([^_A-Za-z0-9]|$))'
hits=$(git grep -nIE "$pattern" -- . \
  ':!examples/exp10-gbash-bench/work/' \
  ':!scripts/ci/retired-otel-env.sh' | grep -vi 'retired' || true)

if [ -n "$hits" ]; then
  echo "Retired LOOMCYCLE_OTEL_* tracing variables are still named here; use the OTEL_* names (docs/CONFIGURATION.md, \"Renamed tracing variables\"):" >&2
  echo "$hits" >&2
  exit 1
fi
echo "no retired LOOMCYCLE_OTEL_* tracing variables"
