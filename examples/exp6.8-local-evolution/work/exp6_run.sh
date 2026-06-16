#!/usr/bin/env bash
# exp6 driver — self-evolving agents. A THIN generation-stepper: the agents do ALL the
# mutation/scoring/forking; this script only loops generations, blocks on each breeder run,
# reads the shared Memory ledger to detect the stop condition, then runs the independent
# re-derivation. Token-safe: every REST call goes through ../loomcurl.sh (bearer via stdin;
# omitted in dev open mode).
#
# Usage (run from a second terminal after ./run.sh):
#   ./work/exp6_run.sh evolve     # run the generation loop until a solver crosses THRESHOLD (or MAX_GEN)
#   ./work/exp6_run.sh verify     # re-run the independent re-derivation only (reads the store)
#
# Env: EXP6_BASE (default http://127.0.0.1:8787 — match run.sh's LOOMCYCLE_LISTEN_ADDR),
#      EXP6_LOG (default /tmp/exp6.log), EXP6_GEN_TIMEOUT (default 900s),
#      MAX_GEN (default 5), THRESHOLD (default 0.90 — match the breeder prompt's constant).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
CURL="$ROOT/loomcurl.sh"
BASE="${EXP6_BASE:-http://127.0.0.1:8787}"
USER_ID=exp6
MAX_GEN="${MAX_GEN:-5}"
THRESHOLD="${THRESHOLD:-0.90}"
LOG="${EXP6_LOG:-/tmp/exp6.log}"
PY=python3

jcurl() { "$CURL" -sS "$@"; }

mem_get() { # $1=key → entry.value JSON, or EMPTY string when the key is absent (404)
  jcurl "$BASE/v1/_memory/scopes/user/$USER_ID/keys/$1" \
    | "$PY" -c 'import sys,json
try:
  d=json.load(sys.stdin); e=d.get("entry")
  v=e.get("value") if isinstance(e,dict) else None
  if v is not None:
    sys.stdout.write(v if isinstance(v,str) else json.dumps(v))
except Exception: pass' 2>/dev/null
}

mem_list() { # $1=prefix → entries array JSON
  jcurl "$BASE/v1/_memory/scopes/user/$USER_ID/keys?prefix=$1&limit=500"
}

spawn_blocking() { # $1=agent $2=text ; blocks until the run's SSE stream closes
  local agent="$1" text="$2" body
  body=$("$PY" -c 'import json,sys
print(json.dumps({"agent":sys.argv[1],"user_id":sys.argv[2],
  "segments":[{"role":"user","content":[{"type":"trusted-text","text":sys.argv[3]}]}]}))' \
    "$agent" "$USER_ID" "$text")
  timeout "${EXP6_GEN_TIMEOUT:-900}" "$CURL" -N -X POST "$BASE/v1/runs" \
    -H 'Content-Type: application/json' -d "$body" >>"$LOG" 2>&1 || true
}

agentdef() { # $1 = JSON body (passed as a literal arg — loomcurl reserves stdin for the auth header).
  "$CURL" -sS -X POST "$BASE/v1/_agentdef" -H 'Content-Type: application/json' -d "$1"
}

# ───────────────────────────── evolve (static variant) ─────────────────────────────
evolve() {
  : > "$LOG"
  echo "exp6 → $BASE  user=$USER_ID  MAX_GEN=$MAX_GEN  THRESHOLD=$THRESHOLD"
  local g stopped=""
  for g in $(seq 0 $((MAX_GEN-1))); do
    echo "=== generation $g — spawning exp6-breeder ==="
    spawn_blocking exp6-breeder "g=$g"
    local summ; summ="$(mem_get "gen:$g:summary")"
    echo "  gen:$g:summary = ${summ:-<none>}"
    local res; res="$(mem_get "result:summary")"
    if [ -n "$res" ]; then echo "  result:summary = $res"; stopped="$res"; break; fi
  done
  [ -z "$stopped" ] && echo "  (reached MAX_GEN without an explicit result:summary)"
  echo; verify
}

# ───────────────────────────── verify (independent re-derivation) ─────────────────────────────
verify() {
  echo "===== independent re-derivation (from the store, not the breeder's report) ====="
  local tmp; tmp="$(mktemp /tmp/exp6_ledger.XXXXXX.json)"
  mem_list 'gen:' > "$tmp"
  local res; res="$(mem_get "result:summary")"
  "$PY" "$HERE/exp6_verify.py" "$tmp" "$res"
  rm -f "$tmp"
  # winner + promotion detail (re-uses $res from above)
  echo "[winner] result:summary = ${res:-<none>}"
  if [ -n "$res" ]; then
    local wdef; wdef="$(printf '%s' "$res" | "$PY" -c 'import sys,json; print(json.load(sys.stdin).get("winner_def_id",""))' 2>/dev/null || true)"
    if [ -n "$wdef" ]; then
      echo "[check] winner def + lineage parent:"
      agentdef "{\"op\":\"get\",\"def_id\":\"$wdef\"}" \
        | "$PY" -c 'import sys,json
try:
  d=json.load(sys.stdin); print("  def:",d.get("name"),"v"+str(d.get("version")),"parent="+str(d.get("parent_def_id")))
except Exception as e: print("  (agentdef get failed:",e,")")' 2>/dev/null || true
    fi
  fi
  echo "[check] active exp6-solver def (promotion target):"
  jcurl "$BASE/v1/_agentdef/names" | "$PY" -c 'import sys,json
try:
  d=json.load(sys.stdin);
  rows=d.get("names") or d.get("agent_defs") or d
  print("  ",json.dumps(rows)[:400])
except Exception: print("  (names query shape differs; inspect manually)")' 2>/dev/null || true
}

case "${1:-evolve}" in
  evolve) evolve ;;
  verify) verify ;;
  *) echo "usage: $0 {evolve|verify}" >&2; exit 2 ;;
esac
