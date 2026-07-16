// loomcycle-builder — the reference sandbox sidecar for loomcycle.
//
// A STANDALONE module (stdlib only, no dependency on loomcycle's own module or
// any third party) so it builds + versions independently and never pulls into
// loomcycle's binary. It runs alongside a distroless loomcycle on the app
// network and exposes container-backed code-execution as MCP-over-HTTP tools
// (mcp__sandbox__*), driving rootless podman for per-session sandbox containers.
//
// See README.md for the design + deploy story; docs/SANDBOX.md in the loomcycle
// repo for the operator guide.
module github.com/denn-gubsky/loomcycle/deploy/builder

go 1.26
