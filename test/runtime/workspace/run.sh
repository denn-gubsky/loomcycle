#!/usr/bin/env bash
# test/runtime/workspace/run.sh — workspaces (RFC GW P1) end to end through a
# booted server in workspace.mode local. Deterministic: code-js agents (no
# LLM, no provider key), the real loop, the real local worker child on its
# private socket. Checks that:
#   - an agent opens a workspace, writes a Python script, runs it and reads
#     the file it wrote, and the workspace's directory is gone from the
#     worker's disk once the run has ended;
#   - a run paused mid-run (RFC X) keeps its workspace: the directory is
#     there while the runtime is paused, the run uses it again after the
#     resume, and the run's end releases it;
#   - the local worker exits when the runtime is killed (--parent-pid);
#   - in open mode (no authentication) a run that names no tenant_id, so
#     carries the empty tenant, opens, uses and releases a workspace on the
#     local worker, which serves that tenant when workspace.local.tenant is
#     unset;
#   - the data pass (RFC GW P2): a Document rendered into a workspace as
#     Markdown, rewritten there by a Python script, imported back as a new
#     Document whose provenance names the workspace, its id and the run,
#     rendered into a volume (PDF where Typst is on PATH, else DOCX); a PDF
#     copied in from the volume and extracted inside the workspace; a folder
#     copied out to the volume and a file removed — with no file's bytes in
#     any tool result of the stored transcript.
# Needs python3 on PATH (the workspace runs it, and it writes the PDF
# fixture); the whole suite skips cleanly without it.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

if ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP — python3 is not on PATH; the workspace scenario runs a Python script on the host engine"
  exit 0
fi

TEST_DIR="$(mktemp -d -t loomcycle-workspace.XXXXXX)"
PORT=18951
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  [[ -n "${RUNPID:-}" ]] && { kill "$RUNPID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DATA="$(cd "$TEST_DIR" && pwd -P)/data"
ROOT="$DATA/workspaces"
VOL="$(cd "$TEST_DIR" && pwd -P)/vol"
mkdir -p "$VOL"
fail() { echo "FAIL ✗ — $1"; exit 1; }

sse_text() {
  grep -A1 '^event: text$' "$1" | grep '^data:' | sed 's/^data: //' | \
    python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null || true
}
post_run() {
  curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" -d @- "$BASE/v1/runs" <<JSON > "$1"
{"agent":"$2","user_id":"ws-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":"${3:-go}"}]}]}
JSON
}
admin() { curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "$2" "$BASE$1"; }
# workspaces lists the workspace directories on the local worker's disk.
workspaces() { find "$ROOT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort; }
# wait_until polls a condition for up to $1 seconds.
wait_until() {
  local secs="$1"; shift
  for _ in $(seq 1 $((secs * 10))); do "$@" && return 0; sleep 0.1; done
  return 1
}
no_workspaces() { [[ -z "$(workspaces)" ]]; }

# data_pass runs wsdata (the RFC GW P2 data pass) and checks what it left: the
# agent's own report, the stored transcript, the volume and the worker's disk.
data_pass() {
  local fmt=pdf
  if ! command -v typst >/dev/null 2>&1; then
    fmt=docx
    echo "      typst is not on PATH: the result is rendered as DOCX instead of PDF"
  fi
  # A one-page PDF with a text layer, written by hand so the suite needs no
  # PDF tool. Its text carries a marker no tool result may show.
  python3 -I - "$VOL/fixture.pdf" <<'PY'
import sys
content = b"BT /F1 18 Tf 72 720 Td (EXTRACTMARKER fixture page) Tj ET"
objs = [b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
        b"<< /Length %d >>\nstream\n" % len(content) + content + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"]
out, offs = bytearray(b"%PDF-1.4\n"), []
for i, o in enumerate(objs, 1):
    offs.append(len(out))
    out += b"%d 0 obj\n" % i + o + b"\nendobj\n"
xref = len(out)
out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
for o in offs:
    out += b"%010d 00000 n \n" % o
out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)
open(sys.argv[1], "wb").write(out)
PY
  # copy into a volume needs the target folder to exist.
  mkdir -p "$VOL/kept"
  post_run "$TEST_DIR/data.sse" wsdata "$fmt"
  local R; R=$(sse_text "$TEST_DIR/data.sse")
  echo "      $R"
  grep -q '^event: error$' "$TEST_DIR/data.sse" && fail "an EventError appeared on the data-pass stream: $(cat "$TEST_DIR/data.sse")"
  for want in "state=ready" "exit=0" "render_md=workspace:data:in/source.md" \
      "captured_from=workspace:data " "tag_ws=true" "import_doc=yes" \
      "render_out=volume:work:result.$fmt" "copied_in=workspace:data:pdf/fixture.pdf" \
      "extracted=workspace:data:out/extracted/fixture.md" "kept=volume:work:kept/out/3" \
      "removed=workspace:data:out/notes.txt"; do
    echo "$R" | grep -q -- "$want" || fail "wsdata: want '$want' in: $R"
  done
  # The provenance names this workspace (its directory on the worker is its
  # id, which the script printed) and this run.
  local ws_dir ws_id run_id want_run
  ws_dir=$(echo "$R" | sed -n 's/.*ws_dir=\([^ ]*\).*/\1/p')
  ws_id=$(echo "$R" | sed -n 's/.*workspace_id=\([^ ]*\).*/\1/p')
  run_id=$(echo "$R" | sed -n 's/.*run_id=\([^ ]*\).*/\1/p')
  [[ "$ws_id" == ws_* && "$ws_id" == "$ws_dir" ]] || fail "provenance workspace_id '$ws_id' is not the workspace's id '$ws_dir'"
  want_run=$(sqlite3 "$DATA/loomcycle.db" "SELECT id FROM runs ORDER BY started_at DESC LIMIT 1;")
  [[ -n "$run_id" && "$run_id" == "$want_run" ]] || fail "provenance run_id '$run_id' is not the run's id '$want_run'"
  local size; size=$(echo "$R" | sed -n 's/.*extracted_size=\([0-9]*\).*/\1/p')
  [[ "${size:-0}" -gt 0 ]] || fail "the extracted Markdown is not in the workspace (stat size '$size')"

  echo "      the stored transcript carries no file's bytes"
  local n_results
  n_results=$(sqlite3 "$DATA/loomcycle.db" "SELECT count(*) FROM events WHERE run_id='$run_id' AND type='tool_result';")
  [[ "$n_results" -ge 12 ]] || fail "only $n_results tool results stored for the data-pass run; the check below would prove nothing"
  sqlite3 "$DATA/loomcycle.db" "SELECT CAST(payload AS TEXT) FROM events WHERE run_id='$run_id';" > "$TEST_DIR/data-events.txt"
  # A tool_result event repeats its call's input; only its text is the result.
  sqlite3 "$DATA/loomcycle.db" "SELECT json_extract(CAST(payload AS TEXT), '$.text') FROM events WHERE run_id='$run_id' AND type='tool_result';" > "$TEST_DIR/data-results.txt"
  ! grep -aq "DATAPASS_REWRITTEN_42" "$TEST_DIR/data-events.txt" || fail "the script's marker reached the transcript"
  ! grep -aq "EXTRACTMARKER" "$TEST_DIR/data-events.txt" || fail "the PDF's text reached the transcript"
  ! grep -aq "%PDF" "$TEST_DIR/data-results.txt" || fail "a PDF's bytes reached a tool result"
  ! grep -aq "The workspace keeps the files" "$TEST_DIR/data-results.txt" || fail "the Document's text reached a tool result"

  echo "      the volume holds the render and the copied folder; the Document holds the rewrite"
  case "$fmt" in
    pdf)  [[ "$(head -c 5 "$VOL/result.pdf")" == "%PDF-" ]] || fail "volume:work:result.pdf is not a PDF" ;;
    docx) [[ "$(head -c 2 "$VOL/result.docx")" == "PK" ]] || fail "volume:work:result.docx is not a DOCX"
          unzip -p "$VOL/result.docx" word/document.xml | grep -q "DATAPASS_REWRITTEN_42" || fail "the DOCX lacks the script's marker" ;;
  esac
  grep -q "DATAPASS_REWRITTEN_42" "$VOL/kept/out/result.md" || fail "the folder copied out lacks the rewritten Markdown"
  grep -q "scratch" "$VOL/kept/out/notes.txt" || fail "the folder copied out lacks notes.txt"
  grep -q "EXTRACTMARKER" "$VOL/kept/out/extracted/fixture.md" || fail "the Markdown extracted in the workspace lacks the PDF's text"
  grep -rlaq "DATAPASS_REWRITTEN_42" "$DATA/sqlmem" || fail "the imported Document's text is not in SQL Memory"

  echo "      the run's end released the workspace"
  wait_until 10 no_workspaces || fail "workspace directories left after the data pass ended: $(workspaces)"
}

# The legacy LOOMCYCLE_AUTH_TOKEN runs under tenant "default", the tenant the
# local worker serves by default.
cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
conversion:
  mode: inprocess
workspace:
  mode: local
volumes:
  work: { path: "$VOL", mode: rw }
agents:
  wsflow:
    provider: code-js
    description: "Workspace end to end: open, write a Python script, run it, read its output."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true }
  wspause:
    provider: code-js
    description: "A workspace held across a runtime pause."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true }
  wsdata:
    provider: code-js
    description: "The data pass: Document to workspace, a script, back to a Document, out to a volume."
    tools: [Workspace, File, Document, Read, Write]
    workspace: { ops: [read, write, exec], open: true }
    volumes: [work]
YAML

echo "[1/7] build + boot (workspace.mode local)"
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_DATA_DIR="$DATA" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
READY=0
for i in $(seq 1 50); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }
grep -q "workspace: mode local runs every agent's workspace commands" "$TEST_DIR/boot.log" || fail "boot log does not warn that local mode has no isolation"
wait_until 15 grep -q "workspace worker attached.*worker_id=local:default/local" "$TEST_DIR/boot.log" || fail "the local worker did not attach"

echo "[2/7] wsflow: open, write square.py, run it, read out.txt"
post_run "$TEST_DIR/flow.sse" wsflow
R=$(sse_text "$TEST_DIR/flow.sse")
echo "      $R"
for want in "state=ready" "engine=host" "exit=0" "printed=squared_12" "result=144" "listed=1"; do
  echo "$R" | grep -q -- "$want" || fail "wsflow: want '$want' in: $R"
done

echo "[3/7] the run's end released the workspace: its directory is gone"
wait_until 10 no_workspaces || fail "workspace directories left after the run ended: $(workspaces)"

echo "[4/7] wspause: a run paused mid-run keeps its workspace"
post_run "$TEST_DIR/pause.sse" wspause &
RUNPID=$!
started() { compgen -G "$ROOT/*/started" >/dev/null; }
wait_until 15 started || fail "the paused run's command never started"
P=$(admin /v1/_pause '{"timeout_ms":20000}')
echo "      pause: $P"
echo "$P" | grep -q '"paused_runs_count":1' || fail "pause did not park the run: $P"
HELD=$(workspaces)
[[ -n "$HELD" && -f "$HELD/marker.txt" ]] || fail "the paused run's workspace is gone while it is paused"
sleep 2
[[ -f "$HELD/marker.txt" ]] || fail "the paused run's workspace went away during the pause"
R=$(admin /v1/_resume '{}')
echo "      resume: $R"
wait "$RUNPID" || fail "the paused run's stream failed"
RUNPID=""
R=$(sse_text "$TEST_DIR/pause.sse")
echo "      $R"
for want in "slept=slept" "marker=kept_across_the_pause" "after=42"; do
  echo "$R" | grep -q -- "$want" || fail "wspause: want '$want' in: $R"
done
wait_until 10 no_workspaces || fail "workspace directories left after the resumed run ended: $(workspaces)"
if command -v sqlite3 >/dev/null 2>&1 && [[ -f "$DATA/loomcycle.db" ]]; then
  N=$(sqlite3 "$DATA/loomcycle.db" "SELECT count(*) FROM workspaces;")
  [[ "$N" == "0" ]] || fail "$N workspace rows left after both runs ended"
fi
if grep -q '^event: error$' "$TEST_DIR/flow.sse" "$TEST_DIR/pause.sse"; then
  fail "an EventError appeared on a run stream"
fi

echo "[5/7] the data pass: Document → workspace → script → Document → volume"
data_pass

echo "[6/7] the local worker exits when the runtime is killed"
CHILD=$(pgrep -P "$PID" -f "role workspace" || true)
[[ -n "$CHILD" ]] || fail "no local workspace worker is running under the runtime"
kill -9 "$PID"; wait "$PID" 2>/dev/null || true; PID=""
gone() { ! kill -0 "$CHILD" 2>/dev/null; }
wait_until 10 gone || fail "the local worker (pid $CHILD) outlived the runtime"
echo "[7/7] open mode: a run that names no tenant opens, uses and releases a workspace"
OPEN_PORT=18952
OPEN_BASE="http://127.0.0.1:$OPEN_PORT"
OPEN_DATA="$(cd "$TEST_DIR" && pwd -P)/open-data"
ROOT="$OPEN_DATA/workspaces"
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_DATA_DIR="$OPEN_DATA" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$OPEN_PORT" \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot-open.log" 2>&1 &
PID=$!
READY=0
for i in $(seq 1 50); do
  curl -fsS "$OPEN_BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "open-mode boot failed:"; cat "$TEST_DIR/boot-open.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot-open.log"; exit 1; }
wait_until 15 grep -q "workspace worker attached.*worker_id=local:/local" "$TEST_DIR/boot-open.log" || fail "the open-mode local worker did not attach for the empty tenant"
# No Authorization header and no tenant_id: the run carries the empty tenant.
curl -fsS -N -H "Content-Type: application/json" -H "Accept: text/event-stream" \
  -d '{"agent":"wsflow","user_id":"ws-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}' \
  "$OPEN_BASE/v1/runs" > "$TEST_DIR/open.sse"
R=$(sse_text "$TEST_DIR/open.sse")
echo "      $R"
for want in "state=ready" "exit=0" "result=144" "listed=1"; do
  echo "$R" | grep -q -- "$want" || fail "open mode: want '$want' in: $R"
done
grep -q '^event: error$' "$TEST_DIR/open.sse" && fail "an EventError appeared on the open-mode run stream"
wait_until 10 no_workspaces || fail "open mode: workspace directories left after the run ended: $(workspaces)"
echo "=== workspace runtime suite PASSED ==="
