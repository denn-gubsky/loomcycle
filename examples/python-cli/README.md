# python-cli — minimal loomcycle Python adapter example

End-to-end smoke test for the [`loomcycle`](../../adapters/python/) Python
adapter. Runs one agent against a local loomcycle gRPC server,
streams the events to stdout, and prints final usage to stderr.

## Run

In one shell, start loomcycle with gRPC enabled:

```bash
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
./bin/loomcycle --config loomcycle.yaml
```

In another shell:

```bash
# One-time: install the adapter in editable mode.
python3 -m venv adapters/python/.venv
adapters/python/.venv/bin/pip install -e adapters/python[dev]

# Run with the default prompt:
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
adapters/python/.venv/bin/python examples/python-cli/main.py

# Or pass your own prompt:
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
adapters/python/.venv/bin/python examples/python-cli/main.py \
  "Explain the loomcycle agentic loop in two sentences."
```

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_GRPC_ADDR` | `127.0.0.1:8788` | gRPC server address. |
| `LOOMCYCLE_AUTH_TOKEN` | (empty) | Bearer for the `Run` RPC. Required when the server has auth enabled. |
| `LOOMCYCLE_AGENT` | `default` | Agent name from your `loomcycle.yaml`. |

## What it exercises

- `LoomcycleClient(...)` construction with auth.
- `client.health()` — liveness probe before any provider call.
- `client.run_streaming(...)` — full server-stream consumption.
- `on_handle` callback — RunHandle capture with `agent_id` /
  `run_id` / `session_id`.
- Event types: `text`, `tool_use`, `usage`, `retry`, `done`, `error`.
- Async-context-manager cleanup (`async with` closes the channel).

The example does NOT cover `continue_session`, `cancel_agent`,
`list_user_agents`, `get_transcript`, or `get_agent` — those are
single-method calls on the same `client` instance and are
exercised in the adapter's unit tests.
