#!/bin/bash
# RFC DR P2 driver — per LoCoMo conversation: A (build), B-off, B-on. See PREREG.md.
#
#   OUT=<dir> PG_DSN=<main store DSN> SQLMEM_DSN=<password DSN> bench/docs/memory-rerank/run.sh
#
# Resumable: a conversation whose B-on report exists is skipped. The server is restarted
# for each phase with the trace index and the rerank layer that phase needs.
set -u
cd "$(dirname "$0")/../../.."
ROOT=$PWD
: "${OUT:?OUT required}" "${PG_DSN:?PG_DSN required}" "${SQLMEM_DSN:?SQLMEM_DSN required}"
# nimble calls so far, from the usage ledger: check 2 needs the count per answer phase.
nimble_calls() { psql "$PG_DSN" -Atc "select count(*) from token_usage where model = 'nimble'" 2>/dev/null || echo -1; }
OLLAMA=${OLLAMA:-http://100.112.7.68:11434}
PORT=${PORT:-8824}
BASE="http://127.0.0.1:$PORT"
mkdir -p "$OUT/data"
[ -f "$OUT/data/locomo10.json" ] || curl -sSL -o "$OUT/data/locomo10.json" \
  https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json
LAYERS="bench/synthesis/ingest.yaml:bench/synthesis/lme_gate_arms.yaml:bench/synthesis/locomo_rating.yaml:bench/docs/memory-rerank/measure.yaml"

start() { # $1 = trace index (1/0), $2 = extra --config (or "")
  local extra=()
  [ -n "$2" ] && extra=(--config "$2")
  env -i HOME="$HOME" PATH="$PATH" LOOMCYCLE_LISTEN_ADDR=127.0.0.1:$PORT LOOMCYCLE_DATA_DIR="$OUT/data/server" \
    OLLAMA_BASE_URL="$OLLAMA" LOOMCYCLE_PRESETS=base,memory LOOMCYCLE_CONFIG_FILES="$LAYERS" \
    LOOMCYCLE_STORAGE_BACKEND=postgres LOOMCYCLE_PG_AUTOMIGRATE=1 LOOMCYCLE_PGVECTOR_ENABLED=1 \
    LOOMCYCLE_PG_DSN="$PG_DSN" LOOMCYCLE_SQLMEM_PG_DSN="$SQLMEM_DSN" LOOMCYCLE_SQLMEM_ENABLED=1 \
    LOOMCYCLE_CODE_AGENTS_ENABLED=1 LOOMCYCLE_MEMORY_TRACE_INDEX="$1" \
    "$ROOT/bin/loomcycle" ${extra[@]+"${extra[@]}"} >> "$OUT/server.log" 2>&1 &
  SRV=$!
  for _ in $(seq 1 90); do curl -s -m 2 "$BASE/healthz" >/dev/null && return 0; sleep 1; done
  echo "server did not start"; kill $SRV 2>/dev/null; exit 1
}
stop() { kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; sleep 2; }
harness() { LOOMCYCLE_LOCOMO_TOKEN=open-mode-probe "$ROOT/bin/locomo" -mode=answer -allow-shared-tenant \
  -loomcycle "$BASE" -run-timeout 30m "$@"; }

python3 - "$OUT/data/locomo10.json" "$OUT/data" <<'EOF'
import json, sys
for s in json.load(open(sys.argv[1])):
    json.dump([s], open("%s/%s.json" % (sys.argv[2], s["sample_id"]), "w"))
EOF

for f in "$OUT"/data/conv-*.json; do
  c=$(basename "$f" .json)
  [ -f "$OUT/B-on-$c/answer-report.json" ] && { echo "$c: done, skipped"; continue; }
  echo "=== $c $(date +%T)"
  if ! grep -q "consolidated in" "$OUT/A-$c.log" 2>/dev/null; then
    start 1 ""
    harness -data "$f" -ingest-as-chats -scribe locomo/scribe -consolidate-passes 12 \
      -retrieval-dump "$OUT/A-$c.dump.jsonl" -out "$OUT/A-$c" > "$OUT/A-$c.log" 2>&1
    stop
  fi
  if ! grep -q "consolidated in" "$OUT/A-$c.log" || grep -qE "work still queued|SKIPPED" "$OUT/A-$c.log"; then
    echo "$c: COVERAGE CHECKPOINT FAILED — see A-$c.log"; continue
  fi
  for arm in off on; do
    [ -f "$OUT/B-$arm-$c/answer-report.json" ] && continue
    extra=""; [ "$arm" = on ] && extra=bench/docs/memory-rerank/rerank-on.yaml
    start 0 "$extra"
    before=$(nimble_calls)
    harness -data "$f" -answer-only -answerer locomo/orn-l2k24-sp -judge locomo/judge \
      -sample-questions 40 -out "$OUT/B-$arm-$c" > "$OUT/B-$arm-$c.log" 2>&1
    echo $(( $(nimble_calls) - before )) > "$OUT/B-$arm-$c.nimble"
    stop
  done
done
echo "ALL DONE $(date +%T)"
