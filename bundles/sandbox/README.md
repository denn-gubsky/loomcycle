# sandbox bundle

A **code-execution agent** that compiles, tests, and runs code in an **isolated,
ephemeral sandbox container** — Python / Go / Rust / C++ / Node — *without* giving
the agent access to loomcycle's own container.

| Agent | Routing | Use it for |
|---|---|---|
| **`dev/sandbox`** | `tier: middle` | Writing + building + testing + running code safely in a disposable, network-off container. |

The container work happens in the **builder sidecar** (a separate service);
loomcycle stays distroless and drives it over HTTP-MCP. The bundle wires the
`mcp__sandbox__*` tools + the `dev/sandbox` agent + skill. This is a **first-class,
reusable bundle** — not an `examples/` experiment.

It also ships a **`dev/sandbox-usage` skill** so any agent with the `Agent` tool
(e.g. the `chat/*` agents, which now carry it) can **delegate** to `dev/sandbox` —
spawn it for a one-shot task, or drive a multi-step session by sharing a `session_id`
— without holding the powerful `mcp__sandbox__*` tools itself (see `docs/SANDBOX.md`
→ "Delegating from other agents").

```
bundles/sandbox/
├── loomcycle.yaml   # the dev/sandbox agent + skill + mcp_servers.sandbox (identical to the embedded copy)
└── README.md
```

The binary ships an **embedded** copy at
[`cmd/loomcycle/embedded/bundles/sandbox.yaml`](../../cmd/loomcycle/embedded/bundles/sandbox.yaml)
(`go:embed`). `bundles/sandbox/loomcycle.yaml` is the in-repo source mirror — keep
the two byte-for-byte identical.

## Enable

```sh
# the bundle carries no provider matrix — pair it with `base`
LOOMCYCLE_PRESETS=base,sandbox
```

### Requirements

- **The builder sidecar** deployed on loomcycle's app network — it serves the
  `mcp__sandbox__*` tools. Build + run it from
  [`deploy/builder/`](../../deploy/builder/); full operator guide in
  [`docs/SANDBOX.md`](../../docs/SANDBOX.md). Without it the sandbox tools are
  registered-but-unreachable (the MCP pool retries lazily).
- **`LOOMCYCLE_SANDBOX_TOKEN`** — the shared bearer, set to the SAME value as the
  sidecar's `SANDBOX_AUTH_TOKEN`. The `mcp_servers.sandbox` header consumes it.
- **A `middle` tier** — select `base` alongside, or supply your own.

## Overriding

The operator overlay can re-declare any field (last layer wins) — most commonly
`mcp_servers.sandbox.url` if the sidecar isn't at `http://builder-sidecar:9000`,
or the agent's tier/sampling. It cannot **widen** `tools` beyond what the bundle
grants.

## Isolation note

Unlike the `loomcycle-toolbox` image (which runs code in loomcycle's own
container — single-tenant/trusted only), this bundle runs each session in a
dedicated, ephemeral, `--network none`, `--read-only`, resource-capped container
with an in-memory workspace. It is the path for untrusted / multi-tenant code.
