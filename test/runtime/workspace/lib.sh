# test/runtime/workspace/lib.sh — helpers the workspace runtime suites share:
# the host-engine suite (run.sh, workspace.mode local) and the container-engine
# suite (container.sh, a remote worker on the container engine). Sourced, not
# run. The caller sets TEST_DIR, TOKEN, BASE, DATA, ROOT and VOL first.
#
# WS_ENGINE (default host) says which engine the data pass runs on: on the
# container engine a command's working directory is the fixed /workspace, so
# the workspace's id is checked against the directories seen on the worker's
# disk during the run instead.

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
  if [[ "${WS_ENGINE:-host}" == container ]]; then
    # The session's working directory is /workspace, whatever the id: record
    # the directories on the worker's disk while the run holds them.
    ( while :; do workspaces >> "$TEST_DIR/seen-ws"; sleep 0.1; done ) &
    local watcher=$!
    post_run "$TEST_DIR/data.sse" wsdata "$fmt"
    kill "$watcher" 2>/dev/null || true
    wait "$watcher" 2>/dev/null || true
  else
    post_run "$TEST_DIR/data.sse" wsdata "$fmt"
  fi
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
  if [[ "${WS_ENGINE:-host}" == container ]]; then
    [[ "$ws_dir" == workspace ]] || fail "the command ran in '$ws_dir', not the fixed /workspace"
    [[ "$ws_id" == ws_* ]] && grep -q "/$ws_id\$" "$TEST_DIR/seen-ws" || fail "provenance workspace_id '$ws_id' is not a workspace the worker held: $(sort -u "$TEST_DIR/seen-ws" | tr '\n' ' ')"
  else
    [[ "$ws_id" == ws_* && "$ws_id" == "$ws_dir" ]] || fail "provenance workspace_id '$ws_id' is not the workspace's id '$ws_dir'"
  fi
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

