"""End-to-end example for the loomcycle Python adapter.

Runs one agent against a local loomcycle gRPC server, streams the
provider events to stdout, and prints the final usage summary.

Usage:

    LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \\
    LOOMCYCLE_AUTH_TOKEN=devtoken \\
    python examples/python-cli/main.py "Your prompt here"

Default prompt is "Say hello in one sentence." if no argv given.

This is the smallest end-to-end example that exercises:
  - LoomcycleClient construction with auth
  - run_streaming with a single user segment
  - the on_handle callback (RunHandle capture)
  - text + usage event handling
  - graceful cleanup via async-context-manager

It does NOT cover continue_session, cancel_agent, list_user_agents,
get_transcript, or get_agent — those are exercised in the unit
tests and are direct one-liners on the same client instance.
"""

from __future__ import annotations

import asyncio
import os
import sys
from typing import Optional

from loomcycle import LoomcycleClient, RunHandle, UnavailableError


async def main() -> int:
    target = os.environ.get("LOOMCYCLE_GRPC_ADDR", "127.0.0.1:8788")
    auth = os.environ.get("LOOMCYCLE_AUTH_TOKEN", "")
    agent = os.environ.get("LOOMCYCLE_AGENT", "default")

    prompt = " ".join(sys.argv[1:]) or "Say hello in one sentence."

    print(f"→ loomcycle gRPC: {target}", file=sys.stderr)
    print(f"→ agent: {agent}", file=sys.stderr)
    print(f"→ prompt: {prompt}", file=sys.stderr)
    print("---", file=sys.stderr)

    handle: Optional[RunHandle] = None

    def on_handle(h: RunHandle) -> None:
        nonlocal handle
        handle = h
        print(
            f"[handle] agent_id={h.agent_id} run_id={h.run_id} session_id={h.session_id}",
            file=sys.stderr,
        )

    async with LoomcycleClient(target=target, auth_token=auth) as client:
        # Quick liveness probe before we burn any provider tokens —
        # if the gRPC server isn't there, fail fast with a useful
        # error message rather than a 30s grpc.aio timeout.
        try:
            health = await client.health()
            print(
                f"[health] ok={health['ok']} commit={health['commit']} uptime={health['uptime_seconds']}s",
                file=sys.stderr,
            )
        except UnavailableError as e:
            print(f"loomcycle unavailable at {target}: {e.message}", file=sys.stderr)
            return 2

        try:
            async for ev in client.run_streaming(
                agent=agent,
                segments=[
                    {
                        "role": "user",
                        "content": [{"type": "trusted-text", "text": prompt}],
                    }
                ],
                on_handle=on_handle,
            ):
                if ev.type == "text":
                    sys.stdout.write(ev.text)
                    sys.stdout.flush()
                elif ev.type == "tool_use" and ev.tool_use is not None:
                    print(f"\n[tool_use] {ev.tool_use.name}", file=sys.stderr)
                elif ev.type == "usage" and ev.usage is not None:
                    print(
                        f"\n[usage] in={ev.usage.input_tokens} "
                        f"out={ev.usage.output_tokens} "
                        f"cache_create={ev.usage.cache_creation_tokens} "
                        f"cache_read={ev.usage.cache_read_tokens} "
                        f"model={ev.usage.model}",
                        file=sys.stderr,
                    )
                elif ev.type == "retry" and ev.retry is not None:
                    print(
                        f"\n[retry] {ev.retry.provider} attempt={ev.retry.attempt} "
                        f"wait={ev.retry.wait_ms}ms reason={ev.retry.reason}",
                        file=sys.stderr,
                    )
                elif ev.type == "done":
                    print(f"\n[done] stop_reason={ev.stop_reason}", file=sys.stderr)
                elif ev.type == "error":
                    print(f"\n[error] {ev.error}", file=sys.stderr)
        except Exception as e:
            print(f"\nrun failed: {type(e).__name__}: {e}", file=sys.stderr)
            return 1

    if handle is not None:
        print(f"\nfinal handle: session_id={handle.session_id}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
