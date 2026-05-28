// Package recipes is the v1.x RFC C1 curated MCP server recipe library.
//
// Each recipe is a JSON file in `.claude/mcp.json` per-server shape plus
// a sibling `_loomcycle:` metadata block:
//
//	{
//	  "command": "npx",
//	  "args": ["-y", "@scope/server-name"],
//	  "env":  {"FOO_TOKEN": "${LOOMCYCLE_FOO_TOKEN}"},
//	  "_loomcycle": {
//	    "description": "...", "transport": "stdio", "pool_size": 2,
//	    "env_vars_required": [...], "credentials": [...],
//	    "schedule_compatible": true, "agent_prompt_hint": "..."
//	  }
//	}
//
// The top-level fields are byte-compatible with Claude Code's
// `.claude/mcp.json` — Claude ignores unknown fields, so loomcycle
// recipes can be dropped into Claude Code projects by stripping the
// `_loomcycle:` block. RFC C2's importer reads the same shape in the
// other direction.
//
// Recipes live as files (not Go-source). Bundled defaults ship under
// `docs/mcp-recipes/*.json` and are //go:embed'ed into the binary.
// Operators override / supplement via the `LOOMCYCLE_MCP_RECIPES_ROOT`
// filesystem overlay: any `<name>.json` under that directory
// completely replaces (not merges) the bundled recipe of the same
// name. Same pattern as Context.help + skills/.
//
// The library is a TEMPLATE source, not a runtime registration source.
// `loomcycle mcp-registry append-to-config <name>` translates a recipe
// to YAML and writes it into the operator's `mcp_servers:` block.
// Loomcycle never auto-instantiates an MCP server from the library at
// boot — that's Tier 2 (yaml) and Tier 3 (MCPServerDef) territory.
package recipes

import (
	"encoding/json"
	"fmt"
)

// Recipe is one parsed recipe — either a bundled default or an
// operator-supplied override.
type Recipe struct {
	// Name is the recipe identifier — the filename minus `.json`.
	// Lookups + CLI args use this. Never serialised inside the JSON
	// body itself (the filename is the canonical key, matching how
	// Claude Code addresses its servers).
	Name string `json:"-"`

	// Source is "bundled" or "overlay", surfaced in `mcp-registry
	// list` output so operators see at a glance which entries they've
	// customised. Mirrors the Context.help "Source" field.
	Source string `json:"-"`

	// Path is the absolute filesystem path of the overlay file.
	// Empty for bundled (it's an embed.FS entry, not a real path).
	Path string `json:"-"`

	// --- Claude-compatible top-level fields ---
	//
	// `command` / `args` / `env` shape for stdio servers; `url` /
	// `headers` shape for http / streamable-http servers. We keep them
	// as json.RawMessage so an operator-authored recipe can include
	// fields we don't yet understand and the round-trip stays lossless
	// (e.g. Claude Code might add fields in a future release; we don't
	// want loomcycle to strip them).

	Command json.RawMessage `json:"command,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Env     json.RawMessage `json:"env,omitempty"`
	URL     json.RawMessage `json:"url,omitempty"`
	Headers json.RawMessage `json:"headers,omitempty"`

	// `_loomcycle` carries metadata loomcycle uses and Claude Code
	// doesn't — description, pool_size, env-var-allowlist hints,
	// schedule-compatibility, etc. Optional from a wire-shape POV
	// (Claude .mcp.json entries won't have it), required for the
	// loomcycle CLI's display + safety checks.
	Loomcycle *LoomcycleMeta `json:"_loomcycle,omitempty"`

	// rawDoc preserves the entire JSON body for the round-trip
	// `mcp-registry show` path. Without it, fields we don't model in
	// this struct would silently drop on re-encode. Populated by
	// (un)marshal helpers below.
	rawDoc map[string]json.RawMessage
}

// LoomcycleMeta is the loomcycle-only metadata block. All fields
// optional; absent fields default to sensible no-op values so
// operator-authored recipes can omit them.
type LoomcycleMeta struct {
	Description        string   `json:"description,omitempty"`
	Transport          string   `json:"transport,omitempty"`
	PoolSize           int      `json:"pool_size,omitempty"`
	EnvVarsRequired    []string `json:"env_vars_required,omitempty"`
	Credentials        []string `json:"credentials,omitempty"`
	ScheduleCompatible bool     `json:"schedule_compatible,omitempty"`
	AgentPromptHint    string   `json:"agent_prompt_hint,omitempty"`
}

// HasTransport returns the transport this recipe describes. Inferred
// from the shape when `_loomcycle.transport` is absent: `command`
// implies stdio; `url` implies http. Returns empty string when neither
// is set, which the CLI surfaces as a validation error at `add` time.
func (r *Recipe) HasTransport() string {
	if r.Loomcycle != nil && r.Loomcycle.Transport != "" {
		return r.Loomcycle.Transport
	}
	if len(r.Command) > 0 {
		return "stdio"
	}
	if len(r.URL) > 0 {
		return "http"
	}
	return ""
}

// Description returns the one-line summary, falling back to a
// placeholder when `_loomcycle.description` is absent.
func (r *Recipe) Description() string {
	if r.Loomcycle != nil && r.Loomcycle.Description != "" {
		return r.Loomcycle.Description
	}
	return "(no description)"
}

// PackageName returns a short identifier for the recipe's underlying
// package — the npm package name when args includes a `-y <package>`
// shape, the URL host when http. Used by RFC C2's recipe-match logic.
// Empty when neither pattern matches.
func (r *Recipe) PackageName() string {
	// stdio shape: args is a JSON array; convention is ["-y",
	// "@scope/package-name", ...] or ["-y", "package-name", ...].
	if len(r.Args) > 0 {
		var args []string
		if err := json.Unmarshal(r.Args, &args); err == nil {
			for i, a := range args {
				if a == "-y" && i+1 < len(args) {
					return args[i+1]
				}
			}
			// Fallback: first non-flag argument.
			for _, a := range args {
				if len(a) > 0 && a[0] != '-' {
					return a
				}
			}
		}
	}
	// http shape: URL host (without scheme / path).
	if len(r.URL) > 0 {
		var url string
		if err := json.Unmarshal(r.URL, &url); err == nil {
			return url
		}
	}
	return ""
}

// MarshalJSON preserves any operator-authored fields outside the ones
// we model (in r.rawDoc), so `mcp-registry show <name>` round-trips
// the file byte-stable. Bundled recipes use a known field set so the
// rawDoc captures everything; operator recipes with custom fields
// also survive intact.
//
// Field order: known fields first (command/args/env/url/headers),
// then `_loomcycle` last. This matches the convention used by the
// bundled `docs/mcp-recipes/*.json` files.
func (r *Recipe) MarshalJSON() ([]byte, error) {
	// Start with rawDoc (preserves unknown fields + operator
	// ordering hints) and overwrite the known fields with the
	// in-memory values, so a programmatic mutation (e.g. CLI editing
	// pool_size) survives the round-trip.
	out := map[string]json.RawMessage{}
	for k, v := range r.rawDoc {
		out[k] = v
	}
	if len(r.Command) > 0 {
		out["command"] = r.Command
	}
	if len(r.Args) > 0 {
		out["args"] = r.Args
	}
	if len(r.Env) > 0 {
		out["env"] = r.Env
	}
	if len(r.URL) > 0 {
		out["url"] = r.URL
	}
	if len(r.Headers) > 0 {
		out["headers"] = r.Headers
	}
	if r.Loomcycle != nil {
		b, err := json.MarshalIndent(r.Loomcycle, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal _loomcycle: %w", err)
		}
		out["_loomcycle"] = b
	}
	return json.MarshalIndent(out, "", "  ")
}

// UnmarshalJSON populates Recipe + retains the raw map so
// MarshalJSON can round-trip unknown fields.
func (r *Recipe) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.rawDoc = raw

	if v, ok := raw["command"]; ok {
		r.Command = v
	}
	if v, ok := raw["args"]; ok {
		r.Args = v
	}
	if v, ok := raw["env"]; ok {
		r.Env = v
	}
	if v, ok := raw["url"]; ok {
		r.URL = v
	}
	if v, ok := raw["headers"]; ok {
		r.Headers = v
	}
	if v, ok := raw["_loomcycle"]; ok {
		var meta LoomcycleMeta
		if err := json.Unmarshal(v, &meta); err != nil {
			return fmt.Errorf("decode _loomcycle block: %w", err)
		}
		r.Loomcycle = &meta
	}
	return nil
}

// Validate enforces the minimum shape every recipe must satisfy.
// Returns nil on success, a descriptive error otherwise. Called at
// load time + by the CLI's `add` subcommand to refuse malformed
// operator-supplied JSON before it lands in the overlay root.
func (r *Recipe) Validate() error {
	// Must have at least a transport-defining shape.
	if len(r.Command) == 0 && len(r.URL) == 0 {
		return fmt.Errorf("recipe must declare either `command` (stdio) or `url` (http)")
	}
	if len(r.Command) > 0 && len(r.URL) > 0 {
		return fmt.Errorf("recipe cannot declare both `command` and `url` — pick stdio OR http")
	}
	// Transport hint, if present, must match the inferred shape.
	if r.Loomcycle != nil && r.Loomcycle.Transport != "" {
		want := r.Loomcycle.Transport
		var got string
		if len(r.Command) > 0 {
			got = "stdio"
		} else {
			// Accept either http or streamable-http for url shapes.
			got = "http"
			if want == "streamable-http" {
				got = "streamable-http"
			}
		}
		if want != got && !(want == "streamable-http" && got == "http") {
			return fmt.Errorf("transport mismatch: _loomcycle.transport=%q but shape implies %q", want, got)
		}
	}
	return nil
}
