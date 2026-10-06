#!/bin/bash
# RFC DR P2 (c) driver — pool depth, 20 vs 40, on #1571's turn store. See PREREG.md.
#
#   OUT=<dir> PG_DSN=<an EMPTY main-store DSN> SQLMEM_DSN=<password DSN> bench/docs/memory-rerank/depth.sh
#
# Runs after (b), never beside it: both arms here call qwen3.8 on the Spark, which (b)'s
# answerer and judge also use. Resumable at every step.
set -eu
cd "$(dirname "$0")/../../.."
ROOT=$PWD
: "${OUT:?OUT required}" "${PG_DSN:?PG_DSN required}" "${SQLMEM_DSN:?SQLMEM_DSN required}"
OLLAMA=${OLLAMA:-http://100.112.7.68:11434}
PORT=${PORT:-8825}
BASE="http://127.0.0.1:$PORT"
LAYERS="bench/synthesis/ingest.yaml:bench/synthesis/lme_gate_arms.yaml:bench/synthesis/locomo_rating.yaml:bench/docs/memory-rerank/measure.yaml"
mkdir -p "$OUT/data"
[ -f "$OUT/data/locomo10.json" ] || curl -sSL -o "$OUT/data/locomo10.json" \
  https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json
[ -x bin/rerankpool ] || go build -o bin/rerankpool ./bench/cmd/rerankpool

if [ ! -s "$OUT/pools20.jsonl" ]; then
  env -i HOME="$HOME" PATH="$PATH" LOOMCYCLE_LISTEN_ADDR=127.0.0.1:$PORT LOOMCYCLE_DATA_DIR="$OUT/data/server" \
    OLLAMA_BASE_URL="$OLLAMA" LOOMCYCLE_PRESETS=base,memory LOOMCYCLE_CONFIG_FILES="$LAYERS" \
    LOOMCYCLE_STORAGE_BACKEND=postgres LOOMCYCLE_PG_AUTOMIGRATE=1 LOOMCYCLE_PGVECTOR_ENABLED=1 \
    LOOMCYCLE_PG_DSN="$PG_DSN" LOOMCYCLE_SQLMEM_PG_DSN="$SQLMEM_DSN" LOOMCYCLE_SQLMEM_ENABLED=1 \
    LOOMCYCLE_CODE_AGENTS_ENABLED=1 LOOMCYCLE_MEMORY_TRACE_INDEX=0 \
    "$ROOT/bin/loomcycle" >> "$OUT/server.log" 2>&1 &
  SRV=$!
  trap 'kill $SRV 2>/dev/null' EXIT
  for _ in $(seq 1 90); do curl -s -m 2 "$BASE/healthz" >/dev/null && break; sleep 1; done
  loco() { LOOMCYCLE_LOCOMO_TOKEN=open-mode-probe "$ROOT/bin/locomo" -data "$OUT/data/locomo10.json" \
    -loomcycle "$BASE" -allow-shared-tenant "$@"; }
  [ -d "$OUT/convert" ] || loco -mode=convert -out "$OUT/convert" > "$OUT/convert.log" 2>&1
  [ -f "$OUT/ingest.done" ] || { loco -mode=ingest -scope user -out "$OUT/ingest" > "$OUT/ingest.log" 2>&1 && touch "$OUT/ingest.done"; }
  LC_BASE="$BASE" python3 bench/docs/memory-rerank/depth.py pools --convert "$OUT/convert" \
    --data "$OUT/data/locomo10.json" --dir "$OUT"
  kill $SRV; wait $SRV 2>/dev/null || true; trap - EXIT
fi
for arm in 20 40; do
  bin/rerankpool -config bench/docs/decision/locomo/reranker.yaml -in "$OUT/pools$arm.jsonl" -out "$OUT/rr$arm.jsonl"
done
python3 bench/docs/memory-rerank/depth.py score --dir "$OUT"
