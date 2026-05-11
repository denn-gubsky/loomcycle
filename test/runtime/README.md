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
| `channels/` | Two-agent canonical handoff: researcher publishes 3 findings to a user-scoped queue; analyst subscribes and produces a structured report. Verifies the Channel tool's publish/subscribe/auto-ack path through real DeepSeek tool calls. |

Future scenarios (one folder per primitive):

- `memory/` — set/get/incr round-trip across two runs of the same agent
- `user-tier/` — runtime fallback on a 429 from the primary provider in a tier's candidate list
- `agent-def/` (v0.8.5) — fork → spawn → retire lifecycle
- `evaluation/` (v0.8.5) — sibling-emitter evaluation of a forked variant
- `context/` (v0.8.6) — introspection rollup against an agent with the full primitive surface

## When to add a runtime test

Anything that's hard to fake with httptest fakes — provider tool-call quirks, multi-run cursor/state interactions, ACL gating against a real model that may try to bypass it, sub-agent ctx inheritance under real spawn paths. Unit tests should still cover everything they can; these scripts cover what the unit tests can't.
