#!/usr/bin/env python3
"""
exp9 orchestration via the loomcycle MCP JSON-RPC interface (/v1/_mcp).
Demonstrates using the MCP protocol directly instead of the REST API.

Each agent gets its own MCP session (sessions are stateful, one active run
per session at a time). spawn_run blocks until the run completes, so
validator is started in a background thread before coder.
"""
import sys, json, time, threading, urllib.request, urllib.error

BASE = "http://127.0.0.1:8787"
MCP  = f"{BASE}/v1/_mcp"

# ── MCP session helpers ────────────────────────────────────────────────────

def mcp_session_init() -> str:
    """Initialize an MCP session and return the session ID."""
    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "exp9-driver", "version": "1.0"},
        }
    }).encode()
    req = urllib.request.Request(MCP, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        sid = r.headers.get("Mcp-Session-Id", "")
        r.read()
    if not sid:
        raise RuntimeError("MCP initialize: no Mcp-Session-Id in response")
    return sid


def mcp_call(sid: str, call_id: int, tool: str, args: dict) -> dict:
    """Call a tool via MCP tools/call and return the result dict."""
    body = json.dumps({
        "jsonrpc": "2.0", "id": call_id,
        "method": "tools/call",
        "params": {"name": tool, "arguments": args},
    }).encode()
    req = urllib.request.Request(
        MCP, data=body,
        headers={"Content-Type": "application/json", "Mcp-Session-Id": sid},
    )
    with urllib.request.urlopen(req, timeout=400) as r:
        return json.loads(r.read())


# ── Segment builder ────────────────────────────────────────────────────────

def user_segment(text: str) -> dict:
    return {"role": "user", "content": [{"type": "trusted-text", "text": text}]}


# ── Per-agent runner ───────────────────────────────────────────────────────

def run_agent(label: str, agent: str, prompt: str, results: dict, key: str):
    """
    Open a fresh MCP session, call spawn_run, print streamed text chunks,
    and record the outcome in results[key].
    """
    try:
        print(f"[{label}] init MCP session…")
        sid = mcp_session_init()
        print(f"[{label}] session={sid[:8]}…  calling spawn_run agent={agent}")

        resp = mcp_call(sid, 2, "spawn_run", {
            "agent": agent,
            "user_id": "exp9",
            "tenant_id": "default",   # open-mode: normalize to avoid empty-tenant sqlmem bug
            "segments": [user_segment(prompt)],
        })

        # spawn_run returns content blocks (text) when the run completes.
        err = resp.get("error")
        if err:
            print(f"[{label}] spawn_run error: {err}")
            results[key] = {"error": err}
            return

        content = resp.get("result", {}).get("content", [])
        for block in content:
            if block.get("type") == "text":
                for line in block["text"].splitlines():
                    print(f"[{label}] {line}")

        results[key] = {"ok": True, "content": content}

    except Exception as exc:
        print(f"[{label}] exception: {exc}")
        results[key] = {"error": str(exc)}


# ── Main orchestration ─────────────────────────────────────────────────────

def main():
    print(f"exp9 via MCP  server={BASE}")

    results: dict = {}

    # 1. Start validator first — it blocks on channel for up to 5 min.
    print("\n→ Spawning exp9-validator (blocks on exp9-primes-done channel)…")
    t_validator = threading.Thread(
        target=run_agent,
        args=("validator", "exp9-validator",
              "Validate the prime numbers once the coder has loaded them.",
              results, "validator"),
        daemon=True,
    )
    t_validator.start()

    # 2. Give validator 3s to subscribe before coder publishes.
    print("   (waiting 3 s for validator to subscribe…)")
    time.sleep(3)

    # 3. Spawn coder in foreground (this session blocks until coder is done).
    print("\n→ Spawning exp9-coder (generates primes → SQL memory → channel)…")
    run_agent("coder", "exp9-coder",
              "Generate all primes up to 500 and store them in SQL memory.",
              results, "coder")

    # 4. Wait for validator (coder's channel ping should have unblocked it).
    print("\n→ Waiting for validator to finish (up to 6 min)…")
    t_validator.join(timeout=360)

    # 5. Summary.
    print("\n── exp9 complete ──────────────────────────────────────────────")
    for k, v in results.items():
        status = "OK" if v.get("ok") else f"FAIL ({v.get('error','?')})"
        print(f"  {k}: {status}")


if __name__ == "__main__":
    main()
