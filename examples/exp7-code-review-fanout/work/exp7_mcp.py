#!/usr/bin/env python3
"""exp7 MCP stdio driver — drive `loomcycle mcp --upstream <base>` (the thin client that backs the
Claude Code loomcycle plugin) and call its meta-tools over JSON-RPC. This is the REAL MCP path: the
thin client proxies to the upstream runtime's /v1/_mcp. Used to fan reviewers out through the MCP
server's `spawn_runs` tool (RFC Y), then to spawn the consolidator.

Token-safe: the upstream bearer is read from $LOOMCYCLE_MCP_UPSTREAM_TOKEN (env, set by run.sh /
exp7_run.sh from .env.local) and handed to the child via env only — never argv, never printed. Empty
token = dev open mode (no auth header).

Usage:
  exp7_mcp.py tools                        # tools/list (names only) — quick connectivity check
  exp7_mcp.py call <tool> <args_json_file> # tools/call <tool> with arguments from a JSON file
Env: LOOMCYCLE_BIN (default "loomcycle"), EXP7_BASE (default http://127.0.0.1:8787),
     LOOMCYCLE_MCP_UPSTREAM_TOKEN (optional).
"""
import json, os, subprocess, sys, threading

BIN  = os.environ.get("LOOMCYCLE_BIN", "loomcycle")
BASE = os.environ.get("EXP7_BASE", "http://127.0.0.1:8787")


def _pump_stderr(stream):
    for line in iter(stream.readline, b""):
        sys.stderr.write("[mcp] " + line.decode("utf-8", "replace"))
    stream.close()


def run(method, params, want_id):
    """Spawn the thin client, do the initialize handshake, send one request, return its result."""
    env = dict(os.environ)  # the child consumes LOOMCYCLE_MCP_UPSTREAM_TOKEN from env; we never echo it
    proc = subprocess.Popen(
        [BIN, "mcp", "-upstream", BASE],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
    )
    threading.Thread(target=_pump_stderr, args=(proc.stderr,), daemon=True).start()

    def send(obj):
        proc.stdin.write((json.dumps(obj) + "\n").encode())
        proc.stdin.flush()

    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                     "clientInfo": {"name": "exp7", "version": "1"}}})
    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    send({"jsonrpc": "2.0", "id": want_id, "method": method, "params": params})

    result = None
    for raw in iter(proc.stdout.readline, b""):
        raw = raw.strip()
        if not raw:
            continue
        try:
            msg = json.loads(raw)
        except json.JSONDecodeError:
            sys.stderr.write("[mcp/non-json] " + raw.decode("utf-8", "replace") + "\n")
            continue
        if msg.get("id") == want_id:
            result = {"__error__": msg["error"]} if "error" in msg else msg.get("result")
            break
    try:
        proc.stdin.close()
    except Exception:
        pass
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except Exception:
        proc.kill()
    return result


def main():
    if len(sys.argv) < 2:
        sys.exit("usage: exp7_mcp.py {tools|call <tool> <args_json_file>}")
    verb = sys.argv[1]
    if verb == "tools":
        r = run("tools/list", {}, 2)
        tools = (r or {}).get("tools", []) if isinstance(r, dict) else []
        print(json.dumps([t.get("name") for t in tools], indent=2))
        return
    if verb == "call":
        tool = sys.argv[2]
        with open(sys.argv[3]) as f:
            args = json.load(f)
        print(json.dumps(run("tools/call", {"name": tool, "arguments": args}, 2), indent=2))
        return
    sys.exit("unknown verb " + verb)


if __name__ == "__main__":
    main()
