# Runtime tests

Per-feature end-to-end smoke tests that drive a real loomcycle binary against a real LLM provider through the HTTP+SSE wire. Distinct from the Go unit tests under `internal/` — those validate the code; these validate the wire.

Each subdirectory under `test/runtime/<feature>/` is one self-contained scenario:

```
test/runtime/<feature>/
├── agents/<name>.md      # one MD per agent (frontmatter + body)
├── loomcycle.yaml        # operator config for THIS test
└── run.sh                # driver — builds the binary, boots it, drives the runs, inspects
```

## Conventions

- **Self-contained.** Each `run.sh` builds the binary, opens a fresh sqlite DB under `$(mktemp -d)`, boots loomcycle on a non-default port, and tears down on exit. No collisions with a running production loomcycle on the same host.
- **Boot log + SSE streams kept on disk** at the printed `$TEST_DIR` path so failures are debuggable without re-running.
- **Verdict is the last thing printed** — `PASS ✓` or `FAIL ✗`. Scripts exit non-zero on failure.
- **Env vars sourced from the operator's shell.** Scripts don't read `.env.local` themselves; the operator runs `set -a; source .env.local; set +a; ./run.sh`. Keeps secrets out of any tracked file.
- **Agents are MD-discovered** (the v0.8.1 `LOOMCYCLE_AGENTS_ROOT` mechanism). The yaml file declares operator-owned state (channels, MCP servers, user_tiers, …) but never the agents themselves — agents are the test's data.

## Current scenarios

| Feature | Scenario |
|---|---|
| `channels/` | Two-agent canonical handoff: researcher publishes 3 findings to a user-scoped queue; analyst subscribes and produces a structured report. Verifies the Channel tool's publish/subscribe/auto-ack path through real provider tool calls. |
| `memory/` | Single agent, two sequential runs. Run 1 writes a user-scope fact + an agent-scope counter (incr); run 2 reads them back and bumps the counter again. Validates cross-run state persistence — the core Memory promise. |
| `user-tier/` | Runtime fallback within a tier's candidate list. Primary provider stalls (induced via a sentinel); resolver walks the tier's candidates, picks the next, completes the run. Validates `fallback_on_error` + the 3-attempt cumulative cap. |
| `agent-def/` (v0.8.5) | Single-agent walkthrough of the six AgentDef ops: create → get → list → fork → promote → retire. Driver inspects `agent_defs` + `agent_def_active` rows to verify lifecycle, parent-chain wiring, retire flags, and active-pointer placement. |
| `evaluation/` (v0.8.5) | Two-run scenario. Run 1 (`worker`) executes a trivial deterministic op; driver extracts its run_id from the SSE `agent` event. Run 2 (`evaluator`) submits + reads back an Evaluation against the worker's run_id (emitter_role=`unrelated`; `submit_any` scope path). Verifies the full submit/get/list/aggregate surface. |
| `system-channels/` (v0.8.6) | Three exercises in one run: (A) `_system/heartbeat-1s` cadence — driver waits 3s and asserts ≥2 messages with the fixed `{ts, version, uptime_s}` payload + `_system` attribution. (B) Admin endpoint — `curl POST /v1/_channels/_system/alarms/info` lands a row with `published_by_user_id = _admin`. (C) Agent deferred publish — scheduler-bot publishes to `findings` with `deliver_at = now+30s`; driver verifies the `(visible_at - published_at)` delta is in the expected window + the tool_result envelope carries `visible_at`. |
| `context/` (v0.8.7) | Single-run introspection walkthrough. The `introspector` agent's `allowed_tools` omits Context — v0.8.7 default-add auto-attaches it at config-load. The agent chains four Context ops (`self`, `tools`, `doc(name=Memory)`, `permissions`) and reports findings. Driver verifies Context calls ≥4, run completed, and the final text mentions agent_name + Context-in-catalog + ends with DONE. Exercises the default-add behavior end-to-end. |

## When to add a runtime test

Anything that's hard to fake with httptest fakes — provider tool-call quirks, multi-run cursor/state interactions, ACL gating against a real model that may try to bypass it, sub-agent ctx inheritance under real spawn paths. Unit tests should still cover everything they can; these scripts cover what the unit tests can't.
