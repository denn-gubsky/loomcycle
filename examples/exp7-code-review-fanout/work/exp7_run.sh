#!/usr/bin/env bash
# exp7 driver — delegate a code review of work/loomcycle-src to loomcycle, fanning the reviewers
# out THROUGH THE MCP SERVER (RFC Y `spawn_runs`).
#
#   ONE `spawn_runs` (mode=join, N=10) → 10 × code-reviewer (concurrent, server-side)
#        each reads its slice (read-only) → Memory.set review:<slice>:findings
#   ONE `spawn_run` → exp7-consolidator → reads review:* → consolidated:report → returns it
#
# The fan-out + the single consolidate both go through the loomcycle MCP thin client (exp7_mcp.py).
# (REST equivalent of `spawn_runs`: POST /v1/runs:batch.)
#
# Usage (start the server first with ../run.sh, and clone the review target — see README step 1):
#   ./work/exp7_run.sh smoke        # spawn_runs N=1 (pause slice) — validate the MCP path end-to-end
#   ./work/exp7_run.sh fanout       # spawn_runs N=10 — fan all reviewers out through MCP (join)
#   ./work/exp7_run.sh consolidate  # spawn_run exp7-consolidator — merge the ledger, return the report
#   ./work/exp7_run.sh delegate     # fanout then consolidate (the full delegation)
#   ./work/exp7_run.sh verify       # independent re-derivation from the store (REST reads)
#
# Token-safe: the MCP upstream bearer comes from .env.local via env (never argv/printed); empty =
# open mode. Env: EXP7_BASE (default from .env.local LOOMCYCLE_LISTEN_ADDR), EXP7_JOIN_MS.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"     # .../work
ROOT="$(cd "$HERE/.." && pwd)"
CURL="$ROOT/loomcurl.sh"
PY=python3
USER_ID=exp7
JOIN_MS="${EXP7_JOIN_MS:-1200000}"    # 20-min join deadline; a slow child is cancelled + reported in-envelope

# Consume provider/bearer + listen addr from .env.local (never printed).
set -a; [ -f "$ROOT/.env.local" ] && source "$ROOT/.env.local"; set +a
export EXP7_BASE="${EXP7_BASE:-http://${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}}"
export LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
export LOOMCYCLE_MCP_UPSTREAM_TOKEN="${LOOMCYCLE_MCP_UPSTREAM_TOKEN:-${LOOMCYCLE_AUTH_TOKEN:-}}"

mcp_call(){ "$PY" "$HERE/exp7_mcp.py" call "$1" "$2"; }   # $1=tool $2=args-json-file
jcurl(){ "$CURL" -sS "$@"; }
mem_get(){ jcurl "$EXP7_BASE/v1/_memory/scopes/user/$USER_ID/keys/$1" \
  | "$PY" -c 'import sys,json
try:
  e=json.load(sys.stdin).get("entry"); v=e.get("value") if isinstance(e,dict) else None
  sys.stdout.write("" if v is None else (v if isinstance(v,str) else json.dumps(v)))
except Exception: pass' 2>/dev/null; }

# Build the spawns args for spawn_runs. $1 = "all" | a single slice name. $2 = join_ms.
# Paths are RELATIVE to the read-root (=../work); the repo is cloned at work/loomcycle-src. An
# absolute /…/**/*.go glob does NOT match under the read-only sandbox.
build_spawns(){ "$PY" - "$1" "$2" <<'PY'
import json, sys
which = sys.argv[1]
SLICES = [
  ("api-http","internal/api/http"), ("tools-builtin","internal/tools/builtin"),
  ("providers","internal/providers"), ("store","internal/store"),
  ("config","internal/config"), ("snapshot","internal/snapshot"),
  ("scheduler","internal/scheduler"), ("pause","internal/pause"),
  ("channels","internal/channels"), ("cmd","cmd/loomcycle"),
]
sel = SLICES if which == "all" else [s for s in SLICES if s[0] == which]
spawns = []
for slice_name, path in sel:
    prompt = (f"slice={slice_name} path={path}. Review loomcycle-src/{path} (read-only; path is "
              f"RELATIVE to your sandbox read-root). The repo IS present — if a glob is empty, retry "
              f"shallower / Read files directly. Record your confidence->=80 findings to Memory key "
              f"review:{slice_name}:findings, then output one REVIEWED line and stop.")
    spawns.append({
        "agent": "code-reviewer", "user_id": "exp7",
        "segments": [{"role": "user", "content": [{"type": "trusted-text", "text": prompt}]}],
        "parent_context": {"root_agent_run_id": "exp7-mcp-fanout"},
    })
print(json.dumps({"spawns": spawns, "mode": "join", "timeout_ms": int(sys.argv[2])}))
PY
}

fanout(){ # $1 = "all" | slice
  local which="${1:-all}" args; args="$(mktemp "${TMPDIR:-/tmp}/exp7_spawns.XXXX.json")"
  build_spawns "$which" "$JOIN_MS" > "$args"
  local n; n=$("$PY" -c 'import json,sys;print(len(json.load(open(sys.argv[1]))["spawns"]))' "$args")
  echo "[fanout] caller -> MCP spawn_runs: fanning out $n code-reviewer run(s) (mode=join)..."
  mcp_call spawn_runs "$args" \
    | "$PY" -c 'import sys,json
r=json.load(sys.stdin)
if isinstance(r,dict) and r.get("__error__"): print("  ERROR:",r["__error__"]); sys.exit(0)
txt=None
for c in (r or {}).get("content",[]):
    if c.get("type")=="text": txt=c.get("text")
env=json.loads(txt) if txt else r
res=env.get("results") or env.get("runs") or []
print(f"  envelope: {len(res)} child result(s)")
for x in res:
    ft=(x.get("final_text") or x.get("error") or ""); fl=ft.splitlines()
    print("   %9s run=%-22s %s" % (x.get("status"), str(x.get("run_id"))[:20], fl[-1] if fl else ""))' 2>/dev/null
  rm -f "$args"
}

consolidate(){
  local args; args="$(mktemp "${TMPDIR:-/tmp}/exp7_consol.XXXX.json")"
  "$PY" - <<'PY' > "$args"
import json
prompt=("The 10 code-reviewer agents have finished (fanned out via MCP spawn_runs). Gather every "
        "review:<slice>:findings from Memory, consolidate, write consolidated:report, return the report.")
print(json.dumps({"agent":"exp7-consolidator","user_id":"exp7",
  "segments":[{"role":"user","content":[{"type":"trusted-text","text":prompt}]}]}))
PY
  echo "[consolidate] caller -> MCP spawn_run: exp7-consolidator (merge the ledger, return the report)..."
  mcp_call spawn_run "$args" \
    | "$PY" -c 'import sys,json
r=json.load(sys.stdin)
if isinstance(r,dict) and r.get("__error__"): print("  ERROR:",r["__error__"]); sys.exit(0)
txt=None
for c in (r or {}).get("content",[]):
    if c.get("type")=="text": txt=c.get("text")
print("  --- consolidator final_text ---"); print(txt or json.dumps(r)[:800])' 2>/dev/null
  rm -f "$args"
  echo "  --- consolidated:report (from Memory) ---"
  mem_get "consolidated:report" | "$PY" -m json.tool 2>/dev/null || echo "  <none>"
}

verify(){
  echo "===== independent re-derivation (REST reads from the store) ====="
  echo "--- per-slice reviews recorded in Memory (review:<slice>:findings) ---"
  jcurl "$EXP7_BASE/v1/_memory/scopes/user/$USER_ID/keys?prefix=review:&limit=100" | "$PY" -c 'import sys,json,re
def load(v):
    if isinstance(v,dict): return v
    try: return json.loads(v)
    except Exception:
        try: return json.loads(re.sub(r"\\(?![\\/\"bfnrtu])", r"\\\\", v))  # tolerate stray backslash escapes
        except Exception: return {}
d=json.load(sys.stdin); tot=0; rows=[]
for e in d.get("entries",[]):
    k=e["key"]
    if not k.endswith(":findings"): continue
    v=load(e["value"]); n=len(v.get("issues",[])); tot+=n
    rows.append((k, v.get("files_reviewed"), n, str(v.get("run_id"))[:18]))
for k,f,n,r in sorted(rows): print("  %-34s files=%s issues=%s run=%s" % (k,f,n,r))
print(f"  slices_recorded={len(rows)}/10  TOTAL issues={tot}")' 2>/dev/null
  echo "--- consolidated:report (returned to the caller) ---"
  mem_get "consolidated:report" | "$PY" -m json.tool 2>/dev/null || echo "  <none>"
}

case "${1:-delegate}" in
  smoke)       fanout pause ;;
  fanout)      fanout all ;;
  consolidate) consolidate ;;
  delegate)    fanout all; echo; consolidate ;;
  verify)      verify ;;
  *) echo "usage: $0 {smoke|fanout|consolidate|delegate|verify}" >&2; exit 2 ;;
esac
