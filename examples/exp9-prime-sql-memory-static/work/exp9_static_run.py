#!/usr/bin/env python3
"""
Orchestrate exp9-static via loomcycle MCP JSON-RPC (/v1/_mcp).

Uses code-js static agents — no LLM calls, deterministic execution.
Start the server first:  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh

Both agents share user_id=exp9 + tenant_id=default → same user-scoped SQL DB.
The validator is started first (it loops on Channel.subscribe), then the coder.
"""
import sys, json, time, threading, urllib.request, urllib.error

BASE = "http://127.0.0.1:8787"
MCP  = f"{BASE}/v1/_mcp"

# ── MCP helpers ─────────────────────────────────────────────────────────────

def mcp_init() -> str:
    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "exp9-static-driver", "version": "1.0"},
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
                "user_id": "exp9",
                "segments": [{"role": "user", "content": [{"type": "trusted-text", "text": prompt}]}],
            },
        },
    }).encode()
    req = urllib.request.Request(
        MCP, data=body,
        headers={"Content-Type": "application/json", "Mcp-Session-Id": sid},
    )
    with urllib.request.urlopen(req, timeout=500) as r:
        return json.loads(r.read())


# ── Per-agent runner ─────────────────────────────────────────────────────────

def run_agent(label: str, agent: str, prompt: str, results: dict, key: str):
    try:
        print(f"[{label}] init MCP session…")
        sid = mcp_init()
        print(f"[{label}] session={sid[:8]}…  spawning {agent}")
        resp = mcp_spawn(sid, agent, prompt)
        err = resp.get("error")
        if err:
            print(f"[{label}] error: {err}")
            results[key] = {"error": err}
            return
        content = resp.get("result", {}).get("content", [])
        for block in content:
            if block.get("type") == "text":
                for line in block["text"].splitlines():
                    print(f"[{label}] {line}")
        results[key] = {"ok": True}
    except Exception as exc:
        print(f"[{label}] exception: {exc}")
        results[key] = {"error": str(exc)}


# ── Orchestration ────────────────────────────────────────────────────────────

def main():
    print(f"exp9-static via MCP  server={BASE}")

    results: dict = {}

    # 1. Validator first — it loops on Channel.subscribe until coder publishes.
    print("\n-> Spawning exp9-static-validator (loops on exp9-primes-done)…")
    t = threading.Thread(
        target=run_agent,
        args=("validator", "exp9-static-validator", "Validate primes from SQL memory.", results, "validator"),
        daemon=True,
    )
    t.start()

    # 2. Let the validator subscribe before the coder publishes.
    print("   (waiting 2 s for validator to subscribe…)")
    time.sleep(2)

    # 3. Coder generates primes and publishes the channel signal.
    print("\n-> Spawning exp9-static-coder (generate → SQL → channel)…")
    run_agent("coder", "exp9-static-coder", "Generate primes and store in SQL memory.", results, "coder")

    # 4. Wait for validator (coder's channel ping should release it).
    print("\n-> Waiting for validator to finish (up to 7 min)…")
    t.join(timeout=420)

    # 5. Summary.
    print("\n── exp9-static complete ────────────────────────────────────────")
    for k, v in results.items():
        status = "OK" if v.get("ok") else f"FAIL ({v.get('error', '?')})"
        print(f"  {k}: {status}")


if __name__ == "__main__":
    main()
