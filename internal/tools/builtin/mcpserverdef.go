package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/mcp"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// MCPServerDef is the v0.9.x substrate primitive for dynamic MCP server
// registration. Mirror of AgentDef (v0.8.5) + SkillDef (v0.8.22) — same
// op set (create / fork / get / list / promote / retire) plus two extras:
//
//   - rediscover: re-runs tools/list against the upstream + refreshes
//     the cached discovered_tools list on the row.
//   - verify: content_sha256 comparison (matches AgentDef/SkillDef verify
//     from PR #175 — operator passes their local hash, gets matches:
//     bool back).
//
// Operator-admin-only — this tool is NOT auto-attached to any agent's
// per-run dispatcher. It exists to be dispatched from:
//   - POST /v1/_mcpserverdef admin endpoint (bearer-authed),
//   - the LoomCycle MCP meta-tool (also bearer-authed via the stdio /
//     HTTP MCP server),
//   - the gRPC MCPServerDef RPC,
//   - the TS adapter's client.mcpServerDef() method.
//
// All four surfaces dispatch through Connector.MCPServerDef, which
// invokes Execute below.
//
// Refusals:
//   - Empty / invalid op → tool refusal.
//   - Transport other than "http" / "streamable-http" → refused.
//     Static yaml mcp_servers: covers stdio.
//   - URL hostname not in LOOMCYCLE_HTTP_HOST_ALLOWLIST → refused
//     (SSRF defence at registration boundary).
//   - `create` on a name already declared in cfg.MCPServers (static yaml)
//     → refused. Operator yaml is ground truth.
//   - `fork` on a name with no parent (no static yaml entry AND no DB
//     row) → refused. Use `create` for genuinely new names.
type MCPServerDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the operator-
	// blessed root (cfg.MCPServers[name]) for name-collision refusal
	// and HTTPHostAllowlist for the SSRF gate.
	Cfg *config.Config

	// Registry is the in-process registry the pool's build callback
	// consults for runtime-registered specs. Required. Mutated on
	// every successful create / promote (Set) and retire (Remove).
	Registry *loommcp.DynamicRegistry

	// Pool is the MCP connection pool. Required. Evicted on retire +
	// promote-of-replacement so cached clients with stale connection
	// metadata never keep serving.
	Pool *loommcp.Pool

	// MaxDefinitionBytes caps the serialised definition JSON. 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field. 0 = no cap.
	MaxDescriptionBytes int
}

const mcpServerDefDescription = `Register, fork, promote, retire, rediscover, and inspect MCP server registrations at runtime. ` +
	`Static yaml mcp_servers: entries remain the operator's stable ground truth; this tool ` +
	`produces the DERIVED layer of runtime registrations. Transport restricted to http + ` +
	`streamable-http (stdio stays yaml-only). URL hostname must be in the operator's HTTP host ` +
	`allowlist. Operations: create, fork, get, list, retire, promote, rediscover, verify.`

const mcpServerDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","promote","rediscover","verify"]},
    "name":          {"type": "string", "description": "MCP server name (required for create/fork/list/rediscover/verify)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire/promote)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset for create/fork: transport, url, headers, description.",
      "properties": {
        "transport": {"type": "string", "enum": ["http","streamable-http"]},
        "url":       {"type": "string", "format": "uri"},
        "headers":   {"type": "object", "additionalProperties": {"type": "string"}},
        "description": {"type": "string"}
      }
    },
    "description":   {"type": "string"},
    "promote":       {"type": "boolean", "description": "create defaults true, fork defaults false."},
    "retired":       {"type": "boolean", "description": "Required for retire."},
    "content_sha256":{"type": "string", "description": "Input for op:verify."}
  },
  "required": ["op"]
}`

type mcpServerDefInput struct {
	Op            string          `json:"op"`
	Name          string          `json:"name,omitempty"`
	DefID         string          `json:"def_id,omitempty"`
	ParentDefID   string          `json:"parent_def_id,omitempty"`
	Overlay       json.RawMessage `json:"overlay,omitempty"`
	Description   string          `json:"description,omitempty"`
	Promote       *bool           `json:"promote,omitempty"`
	Retired       *bool           `json:"retired,omitempty"`
	ContentSHA256 string          `json:"content_sha256,omitempty"`
}

// mcpServerOverlay is the operator-authored content of a registration.
// Persisted in agent_defs.definition JSONB; this is also the shape the
// `create` / `fork` overlay accepts on the wire.
type mcpServerOverlay struct {
	Transport       string            `json:"transport,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Description     string            `json:"description,omitempty"`
	DiscoveredTools []toolDescriptor  `json:"discovered_tools,omitempty"`
}

// toolDescriptor is the cached form of a single tool the upstream
// exposed via tools/list. Refreshed via the `rediscover` op.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func (a *mcpServerOverlay) applyOverlay(ov mcpServerOverlay) {
	if ov.Transport != "" {
		a.Transport = ov.Transport
	}
	if ov.URL != "" {
		a.URL = ov.URL
	}
	if ov.Headers != nil {
		a.Headers = ov.Headers
	}
	if ov.Description != "" {
		a.Description = ov.Description
	}
	// DiscoveredTools is never overlaid — refreshed only via rediscover.
}

// Name implements tools.Tool.
func (m *MCPServerDef) Name() string { return "MCPServerDef" }

// Description implements tools.Tool.
func (m *MCPServerDef) Description() string { return mcpServerDefDescription }

// InputSchema implements tools.Tool.
func (m *MCPServerDef) InputSchema() json.RawMessage {
	return json.RawMessage(mcpServerDefInputSchema)
}

// Execute implements tools.Tool.
func (m *MCPServerDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if m.Store == nil {
		return errResult("MCPServerDef tool: not configured (no Store backend)"), nil
	}
	if m.Cfg == nil {
		return errResult("MCPServerDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	if m.Registry == nil {
		return errResult("MCPServerDef tool: not configured (no DynamicRegistry — pool integration unavailable)"), nil
	}
	var in mcpServerDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}

	switch in.Op {
	case "create":
		return m.execCreate(ctx, in)
	case "fork":
		return m.execFork(ctx, in)
	case "get":
		return m.execGet(ctx, in)
	case "list":
		return m.execList(ctx, in)
	case "retire":
		return m.execRetire(ctx, in)
	case "promote":
		return m.execPromote(ctx, in)
	case "rediscover":
		return m.execRediscover(ctx, in)
	case "verify":
		return m.execVerify(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, promote, rediscover, verify)", in.Op)), nil
	}
}

// ---- create ----

func (m *MCPServerDef) execCreate(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	// Refuse if the yaml block already covers this name. Mirror of
	// AgentDef.create refusal over static cfg.Agents.
	if _, ok := m.Cfg.MCPServers[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.MCPServers entry — yaml is ground truth; use a different name", in.Name)), nil
	}

	def, err := m.buildDefinition("", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := m.validateOverlay(def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}

	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	if m.MaxDefinitionBytes > 0 && len(defJSON) > m.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("create: definition (%d bytes) exceeds max %d", len(defJSON), m.MaxDefinitionBytes)), nil
	}
	if m.MaxDescriptionBytes > 0 && len(in.Description) > m.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("create: description (%d bytes) exceeds max %d", len(in.Description), m.MaxDescriptionBytes)), nil
	}

	ident := tools.RunIdentity(ctx)
	row := store.MCPServerDefRow{
		DefID:            mintMCPServerDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromMCPServerOverlay(in.Name, def),
	}
	created, err := m.Store.MCPServerDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}

	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := m.promoteAndWireRegistry(ctx, created, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(mcpServerDefRowResponse(created, promote))
}

// ---- fork ----

func (m *MCPServerDef) execFork(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	var parentJSON string
	parentDefID := in.ParentDefID
	if parentDefID != "" {
		parent, err := m.Store.MCPServerDefGet(ctx, parentDefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: parent_def_id %q not found", parentDefID)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		if parent.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, parent.Name, in.Name)), nil
		}
		parentJSON = string(parent.Definition)
	} else {
		// No explicit parent → fork from the active row.
		active, err := m.Store.MCPServerDefGetActive(ctx, in.Name)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: name %q has no DB rows — use `create`", in.Name)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		parentDefID = active.DefID
		parentJSON = string(active.Definition)
	}

	def, err := m.buildDefinition(parentJSON, in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := m.validateOverlay(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("fork: marshal: %s", err)), nil
	}
	if m.MaxDefinitionBytes > 0 && len(defJSON) > m.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("fork: definition (%d bytes) exceeds max %d", len(defJSON), m.MaxDefinitionBytes)), nil
	}
	if m.MaxDescriptionBytes > 0 && len(in.Description) > m.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("fork: description (%d bytes) exceeds max %d", len(in.Description), m.MaxDescriptionBytes)), nil
	}

	ident := tools.RunIdentity(ctx)
	row := store.MCPServerDefRow{
		DefID:            mintMCPServerDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromMCPServerOverlay(in.Name, def),
	}
	created, err := m.Store.MCPServerDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := false
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := m.promoteAndWireRegistry(ctx, created, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(mcpServerDefRowResponse(created, promote))
}

// ---- get / list ----

func (m *MCPServerDef) execGet(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := m.Store.MCPServerDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	return okJSON(mcpServerDefRowResponseMap(row))
}

func (m *MCPServerDef) execList(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name != "" {
		rows, err := m.Store.MCPServerDefListByName(ctx, in.Name)
		if err != nil {
			return errResult(fmt.Sprintf("list: %s", err)), nil
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, mcpServerDefRowResponseMap(r))
		}
		return okJSON(map[string]any{"name": in.Name, "versions": out})
	}
	summaries, err := m.Store.MCPServerDefListNames(ctx)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	return okJSON(map[string]any{"names": summaries})
}

// ---- retire / promote ----

func (m *MCPServerDef) execRetire(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (bool)"), nil
	}
	row, err := m.Store.MCPServerDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	if err := m.Store.MCPServerDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// Side-effect on the registry + pool ONLY if retiring the currently-
	// active version. Otherwise the active row stays callable.
	if *in.Retired {
		active, err := m.Store.MCPServerDefGetActive(ctx, row.Name)
		if err == nil && active.DefID == in.DefID {
			m.Registry.Remove(row.Name)
			if m.Pool != nil {
				m.Pool.Evict(row.Name)
			}
		}
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

func (m *MCPServerDef) execPromote(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("promote: missing required field: def_id"), nil
	}
	row, err := m.Store.MCPServerDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("promote: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	ident := tools.RunIdentity(ctx)
	if err := m.promoteAndWireRegistry(ctx, row, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
}

// promoteAndWireRegistry is the shared promote path: store + registry +
// pool eviction. promotion is by-def_id, so the pool entry for the
// name (which may be using the previous active row's URL / headers)
// MUST be evicted so the next agent call gets a fresh client.
func (m *MCPServerDef) promoteAndWireRegistry(ctx context.Context, row store.MCPServerDefRow, promotedByAgentID string) error {
	if err := m.Store.MCPServerDefSetActive(ctx, row.Name, row.DefID, promotedByAgentID); err != nil {
		return err
	}
	// Parse the definition back into the in-memory spec for the registry.
	var ov mcpServerOverlay
	if err := json.Unmarshal(row.Definition, &ov); err != nil {
		return fmt.Errorf("definition unmarshal: %w", err)
	}
	m.Registry.Set(loommcp.DynamicMCPServerSpec{
		Name:      row.Name,
		Transport: ov.Transport,
		URL:       ov.URL,
		Headers:   ov.Headers,
	})
	if m.Pool != nil {
		m.Pool.Evict(row.Name) // existing cached client uses stale metadata; rebuild on next agent call
	}
	return nil
}

// ---- rediscover ----

func (m *MCPServerDef) execRediscover(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("rediscover: missing required field: name"), nil
	}
	active, err := m.Store.MCPServerDefGetActive(ctx, in.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("rediscover: no active row for name %q", in.Name)), nil
		}
		return errResult(fmt.Sprintf("rediscover: %s", err)), nil
	}
	// Force a fresh pool handshake: evict + Get triggers tools/list.
	if m.Pool != nil {
		m.Pool.Evict(in.Name)
		// Bound the handshake — a non-responding upstream would otherwise
		// park this goroutine forever AND block every subsequent
		// rediscover on the same name (they queue on e.ready). 30s
		// matches the boot-time MCP-init budget shape so an operator
		// who deployed a working server can still rediscover even on
		// the slowest legitimate handshake path.
		hsCtx, hsCancel := context.WithTimeout(ctx, 30*time.Second)
		_, descs, err := m.Pool.Get(hsCtx, in.Name)
		hsCancel()
		if err != nil {
			return errResult(fmt.Sprintf("rediscover: pool.Get: %s", err)), nil
		}
		// Reshape descs into the JSON-able toolDescriptor slice + store on a
		// new row version (fork-from-current; preserves audit history).
		var ov mcpServerOverlay
		if err := json.Unmarshal(active.Definition, &ov); err != nil {
			return errResult(fmt.Sprintf("rediscover: definition unmarshal: %s", err)), nil
		}
		ov.DiscoveredTools = nil
		for _, d := range descs {
			ov.DiscoveredTools = append(ov.DiscoveredTools, toolDescriptor{
				Name:        d.Name,
				Description: d.Description,
				InputSchema: d.InputSchema,
			})
		}
		defJSON, _ := json.Marshal(ov)
		ident := tools.RunIdentity(ctx)
		row := store.MCPServerDefRow{
			DefID:            mintMCPServerDefID(),
			Name:             in.Name,
			ParentDefID:      active.DefID,
			Definition:       defJSON,
			Description:      "rediscovered tools/list",
			CreatedByAgentID: ident.AgentID,
			ContentSHA256:    signFromMCPServerOverlay(in.Name, ov),
		}
		created, err := m.Store.MCPServerDefCreate(ctx, row)
		if err != nil {
			return errResult(fmt.Sprintf("rediscover: persist: %s", err)), nil
		}
		if err := m.Store.MCPServerDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("rediscover: promote: %s", err)), nil
		}
		return okJSON(map[string]any{
			"def_id":         created.DefID,
			"name":           in.Name,
			"version":        created.Version,
			"discovered":     len(descs),
			"content_sha256": created.ContentSHA256,
		})
	}
	return errResult("rediscover: pool not configured"), nil
}

// ---- verify ----

func (m *MCPServerDef) execVerify(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	row, err := m.Store.MCPServerDefGetActive(ctx, in.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return okJSON(map[string]any{
				"matches":        false,
				"current_sha256": "",
				"current_def_id": "",
				"version":        0,
				"name":           in.Name,
				"deployed":       false,
			})
		}
		return errResult(fmt.Sprintf("verify: %s", err)), nil
	}
	return okJSON(map[string]any{
		"matches":        in.ContentSHA256 != "" && in.ContentSHA256 == row.ContentSHA256,
		"current_sha256": row.ContentSHA256,
		"current_def_id": row.DefID,
		"version":        row.Version,
		"name":           row.Name,
		"deployed":       true,
	})
}

// ---- helpers ----

// validateOverlay enforces transport whitelist + URL parseability +
// hostname allowlist. Called by create + fork before the row hits the
// store.
func (m *MCPServerDef) validateOverlay(ov mcpServerOverlay) error {
	switch ov.Transport {
	case "http", "streamable-http":
		// ok
	case "":
		return fmt.Errorf("transport is required (http or streamable-http)")
	default:
		return fmt.Errorf("transport %q not allowed for dynamic registration — only http and streamable-http are supported (stdio stays yaml-only)", ov.Transport)
	}
	if ov.URL == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(ov.URL)
	if err != nil {
		return fmt.Errorf("url parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https; got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url missing hostname")
	}
	if !m.hostAllowed(host) {
		return fmt.Errorf("host %q not in LOOMCYCLE_HTTP_HOST_ALLOWLIST (operator-blessed allowlist is the floor for outbound HTTP)", host)
	}
	return nil
}

// hostAllowed delegates to the canonical package-level helper used by
// HTTP + WebFetch (httptool.go). Identical semantics across every
// tool that gates outbound HTTP on `LOOMCYCLE_HTTP_HOST_ALLOWLIST` is
// the load-bearing operator contract: the same yaml allowlist must
// produce the same allow/deny decision regardless of which tool the
// call goes through.
func (m *MCPServerDef) hostAllowed(host string) bool {
	return hostAllowed(host, m.Cfg.Env.HTTPHostAllowlist)
}

// buildDefinition merges parent JSON (or empty for create) with the
// caller's overlay into a fresh mcpServerOverlay.
func (m *MCPServerDef) buildDefinition(parentJSON string, overlay json.RawMessage) (mcpServerOverlay, error) {
	base := mcpServerOverlay{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mcpServerOverlay{}, fmt.Errorf("parse parent definition: %w", err)
		}
	}
	if len(overlay) > 0 {
		var ov mcpServerOverlay
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mcpServerOverlay{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	return base, nil
}

// signFromMCPServerOverlay computes the content_sha256 for a row's
// content. Mirror of signFromMergedDef in agentdef.go.
func signFromMCPServerOverlay(name string, ov mcpServerOverlay) string {
	return mcp.Sign(mcp.MCPServerContent{
		Name:        name,
		Description: ov.Description,
		Transport:   ov.Transport,
		URL:         ov.URL,
		Headers:     ov.Headers,
	})
}

// mcpServerDefRowResponse + Map shape the tool's reply envelope.
// Mirror of agentdef.go's rowResponse / rowResponseMap.
func mcpServerDefRowResponse(row store.MCPServerDefRow, promoted bool) map[string]any {
	m := mcpServerDefRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func mcpServerDefRowResponseMap(row store.MCPServerDefRow) map[string]any {
	return map[string]any{
		"def_id":                   row.DefID,
		"name":                     row.Name,
		"version":                  row.Version,
		"parent_def_id":            row.ParentDefID,
		"description":              row.Description,
		"created_at":               row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"created_by_agent_id":      row.CreatedByAgentID,
		"retired":                  row.Retired,
		"bootstrapped_from_static": row.BootstrappedFromStatic,
		"content_sha256":           row.ContentSHA256,
		// v0.9.x Library v2: include the persisted transport/url/
		// headers/discovered_tools body so the UI can render MCP
		// server content inline without a second round-trip.
		"definition": row.Definition,
	}
}

// mintMCPServerDefID returns a fresh opaque ID. "mdf_<hex>".
func mintMCPServerDefID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return "mdf_" + hex.EncodeToString(buf[:])
}
