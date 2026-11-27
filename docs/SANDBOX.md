# Sandboxed code execution

> **Removed in v1.172.0.** The builder sidecar (`deploy/builder`), its images (`loomcycle-builder`, `loomcycle-builder-docker`, `loomcycle-sandbox-session`), the `sandbox` and `dev-exec` bundles and the `mcp__sandbox__*` tools were removed in v1.172.0 (RFC GW P15). This page stays for one release so old links resolve.

Workspaces replace the sidecar. A workspace worker with the `container` engine runs each workspace in a container of its own, with no network but the egress routes its workspace holds, and no credential inside it. `denngubsky/loomcycle-ws-worker` is a worker that drives the host's Docker, and `denngubsky/loomcycle-workspace` is the session image it starts. The `dev-workspace` bundle's `dev/exec` takes the sidecar's command envelope and returns the same result shape.

- [WORKSPACES.md](WORKSPACES.md) is the guide: running a worker, the `container` engine, the agent grant, egress.
- [MIGRATING-FROM-BASH.md](MIGRATING-FROM-BASH.md#the-builder-sidecar) maps each piece of the sidecar across, and its "Upgrading to v1.172.0" section lists every line to change.
- `LOOMCYCLE_PRESETS` naming `sandbox` or `dev-exec` refuses the start; select `dev-workspace`. An `mcp_servers.sandbox` server of your own is an ordinary MCP server and is not refused by name.
- Old image tags stay published; no new tag is pushed after v1.171.x.
