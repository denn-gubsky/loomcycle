package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"os"
	"strings"
)

// applyEnvHooks appends the hooks block's env lists to the yaml's.
func applyEnvHooks(cfg *Config) {
	// Hooks block (v0.8.17). The env-var override APPENDS to whatever
	// yaml already declared rather than replacing, so an operator can
	// keep their static list in yaml and add an ops-only entry via env
	// without rewriting the config file. Duplicates are tolerated — the
	// registry's IsHostWidenPermitted() does set membership.
	if v := os.Getenv("LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS"); v != "" {
		for _, owner := range splitCSV(v) {
			cfg.Hooks.PermitHostWiden.Owners = append(cfg.Hooks.PermitHostWiden.Owners, owner)
		}
	}
	if v := os.Getenv("LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST"); v != "" {
		cfg.Hooks.PrivateHostAllowlist = append(cfg.Hooks.PrivateHostAllowlist, splitCSV(v)...)
	}
}

// applyEnvNetPolicy reads the private-host allowlist mode; under cidr_only
// the MCP private-IP default flips. Reads cfg.Env, so it runs after
// applyEnvCore.
func applyEnvNetPolicy(cfg *Config) {
	// Checked against HTTPPrivateHostAllowlist in validate.
	cfg.Env.HTTPPrivateHostAllowlistMode = strings.TrimSpace(os.Getenv("LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST_MODE"))
	// Under cidr_only the operator's MCP servers dial through the guard by
	// default too; an explicit =1 is refused in validate.
	if cfg.Env.NetPolicy().CIDROnly() {
		cfg.Env.MCPAllowPrivateIPs = getenvBool("LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS", false)
	}
}

// applyEnvListeners reads the gRPC and metrics listen addresses.
func applyEnvListeners(cfg *Config) {
	// gRPC server (v0.5.5+). Disabled by default; operator opts in
	// by setting LOOMCYCLE_GRPC_ADDR. Coexists with HTTP+SSE (which
	// remains the default and is on a separate port).
	cfg.Env.GrpcAddr = os.Getenv("LOOMCYCLE_GRPC_ADDR")
	// The unauthenticated metrics listener: off unless set.
	cfg.Env.MetricsListenAddr = os.Getenv("LOOMCYCLE_METRICS_LISTEN")
}
