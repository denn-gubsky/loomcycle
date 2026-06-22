#!/usr/bin/env python3
"""
Orchestrate exp9: starts exp9-validator (waits on channel), then exp9-coder
(generates primes → SQL memory → channel ping).  Both share user_id="exp9"
so they read/write the same user-scoped SQL database.

Usage (from the exp9 work/ directory with the server running):
    python3 exp9_run.py [--limit N] [--base-url URL] [--token TOKEN]

Defaults: limit=500, base-url=http://127.0.0.1:8787, token from LOOMCYCLE_AUTH_TOKEN env.
"""
import os, sys, json, time, argparse, threading
import urllib.request, urllib.error

def parse_args():
    p = argparse.ArgumentParser()
    p.add_argument("--limit",    type=int, default=500,
                   help="Upper bound for prime sieve (default 500)")
    p.add_argument("--base-url", default=os.environ.get("LOOMCYCLE_BASE_URL",
                   "http://127.0.0.1:8787"))
    p.add_argument("--token",    default=os.environ.get("LOOMCYCLE_AUTH_TOKEN", ""))
    return p.parse_args()

def spawn(base_url: str, token: str, agent: str, prompt: str) -> dict:
    body = json.dumps({
        "agent": agent,
        "user_id": "exp9",
        "segments": [{"role": "user", "content": [{"type": "trusted-text", "text": prompt}]}],
    }).encode()
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(f"{base_url}/v1/runs", data=body, headers=headers)
    with urllib.request.urlopen(req, timeout=10) as r:
        # /v1/runs returns SSE; first event contains the run_id
        for raw in r:
            line = raw.decode().strip()
            if line.startswith("data:"):
                ev = json.loads(line[5:].strip())
                if "run_id" in ev:
                    return ev
    return {}

def stream_run(base_url: str, token: str, run_id: str, label: str):
    """Stream SSE events for a run, printing assistant text."""
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(f"{base_url}/v1/runs/{run_id}/stream", headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=400) as r:
            for raw in r:
                line = raw.decode().strip()
                if not line.startswith("data:"):
                    continue
                try:
                    ev = json.loads(line[5:].strip())
                except json.JSONDecodeError:
                    continue
                if ev.get("type") == "content_block_delta":
                    text = ev.get("delta", {}).get("text", "")
                    if text:
                        print(f"[{label}] {text}", end="", flush=True)
                elif ev.get("type") == "run_end":
                    print(f"\n[{label}] run_end status={ev.get('status')}")
                    break
    except Exception as e:
        print(f"[{label}] stream error: {e}")

def main():
    args = parse_args()
    base = args.base_url.rstrip("/")
    tok  = args.token

    print(f"exp9: base_url={base}  limit={args.limit}")

    # 1. Start validator first — it blocks on the channel.
    print("\n→ Spawning exp9-validator (will wait on exp9-primes-done channel)…")
    v = spawn(base, tok, "exp9-validator", "Validate the prime numbers once the coder has loaded them.")
    v_id = v.get("run_id", "?")
    print(f"  validator run_id={v_id}")

    # Stream validator in background thread.
    t_v = threading.Thread(target=stream_run, args=(base, tok, v_id, "validator"), daemon=True)
    t_v.start()

    # Small delay so validator is subscribed before coder publishes.
    time.sleep(2)

    # 2. Start coder — generates primes and publishes the channel signal.
    print(f"\n→ Spawning exp9-coder (limit={args.limit})…")
    c = spawn(base, tok, "exp9-coder",
              f"Generate all primes up to {args.limit} and store them in SQL memory.")
    c_id = c.get("run_id", "?")
    print(f"  coder run_id={c_id}")

    # Stream coder in foreground.
    stream_run(base, tok, c_id, "coder")

    # Wait for validator to finish (up to 6 min).
    print("\n→ Waiting for validator to finish…")
    t_v.join(timeout=360)
    print("\nexp9 complete.")

if __name__ == "__main__":
    main()
