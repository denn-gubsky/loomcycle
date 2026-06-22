#!/usr/bin/env python3
"""
RFC AJ GBash Comparative Benchmark — MCP driver.

Spawns exp10-bash-agent and exp10-bashbox-agent IN PARALLEL via the loomcycle
MCP JSON-RPC interface (/v1/_mcp).  Both agents run identical file-discovery
operations on a cloned copy of the loomcycle repo, recording per-operation
wall-clock timings.  After both complete, this script produces a side-by-side
comparison report and writes it to work/reports/exp10-bench-report.md.

Start the server first:
  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh

Then run (from a second terminal, inside work/):
  python3 exp10_run.py
"""
import sys, json, time, threading, urllib.request, urllib.error, os

BASE = "http://127.0.0.1:8787"
MCP  = f"{BASE}/v1/_mcp"
HERE = os.path.dirname(os.path.abspath(__file__))
REPORTS_DIR = os.path.join(HERE, "reports")

# ── MCP helpers ─────────────────────────────────────────────────────────────

def mcp_init() -> str:
    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "exp10-bench-driver", "version": "1.0"},
        }
    }).encode()
    req = urllib.request.Request(MCP, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        sid = r.headers.get("Mcp-Session-Id", "")
        r.read()
    if not sid:
        raise RuntimeError("MCP initialize: no Mcp-Session-Id returned")
    return sid


def mcp_spawn(sid: str, agent: str, prompt: str) -> dict:
    body = json.dumps({
        "jsonrpc": "2.0", "id": 2,
        "method": "tools/call",
        "params": {
            "name": "spawn_run",
            "arguments": {
                "agent": agent,
                "user_id": "exp10",
                "segments": [{"role": "user", "content": [{"type": "trusted-text", "text": prompt}]}],
            },
        },
    }).encode()
    req = urllib.request.Request(
        MCP, data=body,
        headers={"Content-Type": "application/json", "Mcp-Session-Id": sid},
    )
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.loads(r.read())


# ── Per-agent runner ─────────────────────────────────────────────────────────

def run_agent(label: str, agent: str, results: dict, key: str):
    try:
        print(f"[{label}] initializing MCP session…")
        sid = mcp_init()
        print(f"[{label}] session={sid[:8]}…  spawning {agent}")
        wall_start = time.monotonic()
        resp = mcp_spawn(sid, agent, "Run the benchmark.")
        wall_ms = int((time.monotonic() - wall_start) * 1000)

        err = resp.get("error")
        if err:
            print(f"[{label}] MCP error: {err}")
            results[key] = {"error": str(err), "raw": resp}
            return

        content = resp.get("result", {}).get("content", [])
        final_text = ""
        for block in content:
            if block.get("type") == "text":
                final_text = block["text"]
                break

        # final_text is a JSON object from the agent
        try:
            outer = json.loads(final_text)
            inner_text = ""
            # spawn_run result wraps in {final_text: ...}
            for k in ("final_text", "text"):
                if k in outer:
                    inner_text = outer[k]
                    break
            data = json.loads(inner_text) if inner_text else outer
        except Exception:
            data = {"raw_text": final_text}

        data["_wall_ms"] = wall_ms
        results[key] = data
        print(f"[{label}] done  total_ms={data.get('total_ms','?')}  errors={len(data.get('errors',[]))}")

    except Exception as exc:
        print(f"[{label}] exception: {exc}")
        results[key] = {"error": str(exc)}


# ── Report generation ────────────────────────────────────────────────────────

OP_NAMES = [
    "cleanup", "git_clone",
    "ls_root",
    "count_all_files", "count_go_files", "count_funcs",
    "total_loc", "grep_rfc_aj", "large_files", "dir_depth",
]

def format_delta(bash_ms: int, bb_ms: int) -> tuple[str, str]:
    delta = bb_ms - bash_ms
    pct   = (delta / bash_ms * 100) if bash_ms > 0 else 0
    sign  = "+" if delta >= 0 else ""
    return f"{sign}{delta}", f"{sign}{pct:.0f}%"


def build_report(bash: dict, bb: dict) -> str:
    lines = []
    lines.append("# exp10-gbash-bench — RFC AJ GBash Benchmark Report\n")
    lines.append("## Setup\n")
    lines.append("| Item | Value |")
    lines.append("|------|-------|")
    lines.append("| loomcycle source | `/Users/denn/work/loomcycle` |")
    lines.append(f"| Bash target dir  | `loomcycle-bench/` |")
    lines.append(f"| Bashbox target dir | `loomcycle-bench-bb/` |")
    ephem_bash = bash.get("ephemeral_vol", "?")
    ephem_bb   = bb.get("ephemeral_vol", "?")
    lines.append(f"| Ephemeral vol (Bash)    | `{ephem_bash}` |")
    lines.append(f"| Ephemeral vol (Bashbox) | `{ephem_bb}` |")
    lines.append("")

    # index ops by name
    bash_ops = {o["name"]: o for o in bash.get("ops", [])}
    bb_ops   = {o["name"]: o for o in bb.get("ops", [])}
    all_names = OP_NAMES + [n for n in bash_ops if n not in OP_NAMES]

    lines.append("## Timing Results\n")
    lines.append("| Operation | Bash (ms) | Bashbox (ms) | Δ (ms) | Δ% | Notes |")
    lines.append("|-----------|-----------|--------------|--------|----|-------|")
    for name in all_names:
        bop = bash_ops.get(name)
        bbop = bb_ops.get(name)
        b_ms  = bop["ms"]  if bop  else "-"
        bb_ms = bbop["ms"] if bbop else "-"
        note = ""
        if bop and "[ERROR]" in bop.get("result", ""):
            note = "⚠ Bash error"
        if bbop and "[ERROR]" in bbop.get("result", ""):
            note = ("⚠ Bash+Bashbox error" if note else "⚠ Bashbox error")
        if isinstance(b_ms, int) and isinstance(bb_ms, int):
            d_ms, d_pct = format_delta(b_ms, bb_ms)
        else:
            d_ms, d_pct = "-", "-"
        if name == "git_clone" and not note:
            note = "Bashbox uses RFC AJ §13 fallback proxy"
        lines.append(f"| `{name}` | {b_ms} | {bb_ms} | {d_ms} | {d_pct} | {note} |")

    bash_total = bash.get("total_ms", "-")
    bb_total   = bb.get("total_ms", "-")
    if isinstance(bash_total, int) and isinstance(bb_total, int):
        d_ms, d_pct = format_delta(bash_total, bb_total)
    else:
        d_ms, d_pct = "-", "-"
    lines.append(f"| **TOTAL** | **{bash_total}** | **{bb_total}** | **{d_ms}** | **{d_pct}** | |")
    lines.append("")

    lines.append("## Output Comparison\n")
    lines.append("Verifying that both modes produce consistent results for discovery ops.\n")
    lines.append("| Operation | Bash result | Bashbox result | Match? |")
    lines.append("|-----------|-------------|----------------|--------|")
    for name in ["count_all_files", "count_go_files", "count_funcs", "grep_rfc_aj", "total_loc"]:
        bop  = bash_ops.get(name, {})
        bbop = bb_ops.get(name, {})
        br_raw  = bop.get("result", "-").strip()
        bbr_raw = bbop.get("result", "-").strip()
        # Normalize to first line only: gbash combines stdout+stderr so internal
        # error messages appear on subsequent lines (e.g. "stat ... denied").
        br  = br_raw.split("\n")[0].strip()
        bbr = bbr_raw.split("\n")[0].strip()
        match = "✓" if br.split() == bbr.split() else "✗ MISMATCH"
        note = ""
        if bbr_raw != bbr:
            note = " *(gbash stderr appended)*"
        br_disp  = br[:60].replace("|", "\\|")
        bbr_disp = (bbr[:57] + "…" if len(bbr) > 60 else bbr).replace("|", "\\|")
        lines.append(f"| `{name}` | `{br_disp}` | `{bbr_disp}`{note} | {match} |")
    lines.append("")

    lines.append("## Error Summary\n")
    bash_errs = bash.get("errors", [])
    bb_errs   = bb.get("errors", [])
    if not bash_errs and not bb_errs:
        lines.append("No errors in either mode.\n")
    else:
        if bash_errs:
            lines.append(f"**Bash mode ({len(bash_errs)} error(s)):**\n")
            for e in bash_errs:
                lines.append(f"- `{e.get('op','?')}`: {e.get('error','?')}")
            lines.append("")
        if bb_errs:
            lines.append(f"**Bashbox mode ({len(bb_errs)} error(s)):**\n")
            for e in bb_errs:
                lines.append(f"- `{e.get('op','?')}`: {e.get('error','?')}")
            lines.append("")

    lines.append("## Observations\n")
    lines.append("- **Mode 1 (Bash)**: real `/bin/sh` subprocess; full host PATH; git runs natively.")
    lines.append("- **Mode 2 (Bashbox)**: pure-Go in-process sandbox; git escapes via RFC AJ §13 fallback proxy.")
    lines.append("- Ephemeral volumes (RFC AH) are created and auto-purge after each run ends.")
    lines.append("- **Timing overhead**: each op includes ~30ms from two `python3` subprocess calls used")
    lines.append("  for wall-clock measurement (`Date.now()` measures JS overhead only, not tool execution).")
    lines.append("- A positive Δ% means Bashbox is slower; negative means Bashbox is faster.")
    lines.append("")
    lines.append("## Bashbox Compatibility Findings\n")
    lines.append("These gaps were discovered during the benchmark:\n")
    lines.append("1. **`grep --include=GLOB`**: not supported by gbash. Replaced with `find | while | grep` pipeline.")
    lines.append("2. **EvalSymlinks on relative symlinks**: gbash's `find` calls `EvalSymlinks` on every traversed")
    lines.append("   path before type filters are applied. Relative symlinks inside a cloned repo (e.g.")
    lines.append("   `loomcycle.example.yaml → cmd/loomcycle/embedded/...`) trigger a containment check that")
    lines.append("   fails and aborts traversal. Workaround: use shell glob `dir/*/` to enumerate subdirs,")
    lines.append("   bypassing the top-level symlink. Error appears in combined stdout+stderr output.")
    lines.append("3. **`xargs`**: not in gbash's built-in coreutils. Replaced with `while read` loops.")
    lines.append("4. **`rm -rf` returns null in code-js**: Bashbox returns `null`/`undefined` for commands")
    lines.append("   with empty stdout. Code-js must use `Bashbox({...}) || ''` defensively.")
    lines.append("")

    return "\n".join(lines)


# ── Orchestration ────────────────────────────────────────────────────────────

def main():
    print(f"exp10-gbash-bench  server={BASE}")
    print(f"Spawning both agents IN PARALLEL…\n")

    results: dict = {}

    t_bash = threading.Thread(
        target=run_agent,
        args=("bash", "exp10-bash-agent", results, "bash"),
        daemon=True,
    )
    t_bb = threading.Thread(
        target=run_agent,
        args=("bashbox", "exp10-bashbox-agent", results, "bashbox"),
        daemon=True,
    )

    t_bash.start()
    t_bb.start()

    t_bash.join(timeout=300)
    t_bb.join(timeout=300)

    bash = results.get("bash", {})
    bb   = results.get("bashbox", {})

    if "error" in bash:
        print(f"\n[bash] FAILED: {bash['error']}")
    if "error" in bb:
        print(f"\n[bashbox] FAILED: {bb['error']}")

    if "error" not in bash and "error" not in bb:
        report = build_report(bash, bb)
        print("\n" + "─" * 70)
        print(report)
        print("─" * 70)
        os.makedirs(REPORTS_DIR, exist_ok=True)
        report_path = os.path.join(REPORTS_DIR, "exp10-bench-report.md")
        with open(report_path, "w") as f:
            f.write(report)
        print(f"\nReport written to: {report_path}")
    else:
        print("\nOne or both agents failed — skipping report generation.")
        print("Raw results:")
        for k, v in results.items():
            print(f"  {k}: {json.dumps(v, indent=2)[:500]}")


if __name__ == "__main__":
    main()
