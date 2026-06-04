package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
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
	`allowlist. create/fork auto-discover the upstream's tools (tools/list) at ingestion ` +
	`(best-effort; opt out with discover:false), so a separate rediscover is only needed to ` +
	`refresh a changed tool surface. Operations: create, fork, get, list, retire, promote, rediscover, verify.`

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
    "discover":      {"type": "boolean", "description": "create/fork: run the tools/list handshake at ingestion so discovered_tools is populated immediately (default true). Best-effort — a peer that is unreachable at ingestion still registers and self-heals on first call. Set false to register metadata only."},
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
	Discover      *bool           `json:"discover,omitempty"`
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
	// RFC N: the tenant is authoritative from the run ctx — never from the
	// wire/tool input. It scopes the version lock, the active pointer, the
	// dedup read, and the registry entry. "" = shared/operator/legacy.
	tenantID := tools.RunIdentity(ctx).TenantID
	// Refuse if the yaml block already covers this name FOR THE SHARED
	// TENANT. Mirror of AgentDef.create refusal over static cfg.Agents.
	// A tenant principal (tenantID != "") MAY register a name that collides
	// with a shared yaml server — that is the RFC N per-tenant override
	// (the tenant-dynamic pass shadows the static base); yaml stays ground
	// truth only for the "" tenant.
	if _, ok := m.Cfg.MCPServers[in.Name]; ok && tenantID == "" {
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

	contentSHA := signFromMCPServerOverlay(in.Name, def)
	// Idempotent create: if the active def already carries this exact
	// content, return it as a no-op instead of minting a byte-identical new
	// version. A consumer that blindly re-registers on every restart no
	// longer spams the lineage; this is the server-side complement to
	// verify-before-create. content_sha256 covers {name, description,
	// transport, url, headers} — NOT discovered_tools, which the rediscover
	// path dedups separately. Re-creating content that matches a NON-active
	// version still mints + promotes (re-activation is a real state change),
	// so we compare only against the active row.
	if active, gerr := m.Store.MCPServerDefGetActive(ctx, tenantID, in.Name); gerr == nil && active.ContentSHA256 == contentSHA {
		resp := mcpServerDefRowResponse(active, true)
		resp["deduplicated"] = true
		return okJSON(resp)
	}

	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	// Automatic discovery-on-ingestion: fold the tools/list handshake into the
	// create so v1 carries discovered_tools immediately (no separate
	// rediscover). Best-effort + only when this version becomes active; an
	// unreachable peer leaves the def metadata-only and self-heals lazily.
	var discovered int
	defJSON, discovered = m.discoverOnIngestion(ctx, tenantID, in.Name, in, promote, &def, defJSON)

	ident := tools.RunIdentity(ctx)
	row := store.MCPServerDefRow{
		DefID:            mintMCPServerDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    contentSHA,
		TenantID:         tenantID,
	}
	created, err := m.Store.MCPServerDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}

	if promote {
		if err := m.promoteAndWireRegistry(ctx, created, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	resp := mcpServerDefRowResponse(created, promote)
	resp["discovered"] = discovered
	return okJSON(resp)
}

// ---- fork ----

func (m *MCPServerDef) execFork(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}
	// RFC N: tenant from ctx (never the wire). Scopes the parent lookup,
	// the active pointer, and the row's tenant stamp.
	tenantID := tools.RunIdentity(ctx).TenantID

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
		// RFC N: a fork stays within its own tenant — refuse a parent that
		// belongs to a different tenant (no cross-tenant lineage / leak).
		if parent.TenantID != tenantID {
			return errResult(fmt.Sprintf("fork: parent_def_id %q belongs to tenant %q, refusing to fork under tenant %q", parentDefID, parent.TenantID, tenantID)), nil
		}
		parentJSON = string(parent.Definition)
	} else {
		// No explicit parent → fork from the active row.
		active, err := m.Store.MCPServerDefGetActive(ctx, tenantID, in.Name)
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

	contentSHA := signFromMCPServerOverlay(in.Name, def)
	promote := false
	if in.Promote != nil {
		promote = *in.Promote
	}
	// Discover only on a promoted fork: discovery dials by name, which the
	// pool resolves to the about-to-be-active version's connection params, so
	// a non-promoted fork (not yet active) can't be dialed meaningfully here.
	var discovered int
	defJSON, discovered = m.discoverOnIngestion(ctx, tenantID, in.Name, in, promote, &def, defJSON)

	ident := tools.RunIdentity(ctx)
	row := store.MCPServerDefRow{
		DefID:            mintMCPServerDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    contentSHA,
		TenantID:         tenantID,
	}
	created, err := m.Store.MCPServerDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if promote {
		if err := m.promoteAndWireRegistry(ctx, created, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	resp := mcpServerDefRowResponse(created, promote)
	resp["discovered"] = discovered
	return okJSON(resp)
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
	// RFC N: def_id is a global handle but a def is owned by exactly one
	// tenant. MCPServerDefGet is by-def_id and tenant-blind, so guard here:
	// a caller in tenant T cannot read another tenant's def. Return the SAME
	// opaque not-found a missing def returns — never leak existence/body of a
	// cross-tenant row.
	if row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
	}
	return okJSON(mcpServerDefRowResponseMap(row))
}

func (m *MCPServerDef) execList(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	// RFC N: both list shapes return rows across ALL tenants (names are
	// per-tenant now). Filter to the caller's own tenant so a tenant lists
	// only its own servers — never enumerates another tenant's.
	tenantID := tools.RunIdentity(ctx).TenantID
	if in.Name != "" {
		rows, err := m.Store.MCPServerDefListByName(ctx, in.Name)
		if err != nil {
			return errResult(fmt.Sprintf("list: %s", err)), nil
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			if r.TenantID != tenantID {
				continue
			}
			out = append(out, mcpServerDefRowResponseMap(r))
		}
		return okJSON(map[string]any{"name": in.Name, "versions": out})
	}
	// MCPServerDefListNames returns a TenantID per summary (it's the
	// boot/advertising key, NOT a tenant-scoped query) — filter the
	// summaries to the caller's tenant here, the consumer-side guard.
	summaries, err := m.Store.MCPServerDefListNames(ctx)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]store.MCPServerDefNameSummary, 0, len(summaries))
	for _, s := range summaries {
		if s.TenantID != tenantID {
			continue
		}
		out = append(out, s)
	}
	return okJSON(map[string]any{"names": out})
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
	// RFC N: refuse cross-tenant retire. MCPServerDefSetRetired is a global
	// by-def_id mutation; without this guard a caller in tenant T could
	// retire another tenant's def (and below, evict another tenant's pooled
	// client). Opaque not-found — don't leak existence. After this guard
	// row.TenantID == the caller's tenant, so the Registry.Remove /
	// Pool.Evict below stay keyed to the caller's own (tenant, name).
	if row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
	}
	if err := m.Store.MCPServerDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// Side-effect on the registry + pool ONLY if retiring the currently-
	// active version. Otherwise the active row stays callable. RFC N: the
	// active pointer + registry + pool entries are keyed by the def's OWN
	// tenant, so eviction stays tenant-correct (retiring tenant A's server
	// leaves tenant B's same-name server untouched).
	if *in.Retired {
		active, err := m.Store.MCPServerDefGetActive(ctx, row.TenantID, row.Name)
		if err == nil && active.DefID == in.DefID {
			m.Registry.Remove(row.TenantID, row.Name)
			if m.Pool != nil {
				m.Pool.Evict(row.TenantID, row.Name)
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
	// RFC N: promote within the caller's OWN tenant. def_id is a global
	// handle, and promoteAndWireRegistry keys SetActive/Registry/Pool on the
	// def's own tenant — so without this guard a caller in tenant T could
	// promote (and evict the pooled client of) another tenant's def. Opaque
	// not-found — don't leak existence. Mirrors agentdef execPromote, which
	// gets the same protection by passing ident.TenantID to AgentDefSetActive.
	if row.TenantID != ident.TenantID {
		return errResult(fmt.Sprintf("promote: def_id %q not found", in.DefID)), nil
	}
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
	// RFC N: promote within the def's OWN tenant. The store refuses a
	// def whose tenant_id ≠ the passed tenant, so a caller can't point
	// another tenant's active pointer at a def it owns.
	if err := m.Store.MCPServerDefSetActive(ctx, row.TenantID, row.Name, row.DefID, promotedByAgentID); err != nil {
		return err
	}
	// Parse the definition back into the in-memory spec for the registry.
	var ov mcpServerOverlay
	if err := json.Unmarshal(row.Definition, &ov); err != nil {
		return fmt.Errorf("definition unmarshal: %w", err)
	}
	m.Registry.Set(specFromOverlay(row.TenantID, row.Name, ov))
	if m.Pool != nil {
		m.Pool.Evict(row.TenantID, row.Name) // existing cached client uses stale metadata; rebuild on next agent call
	}
	return nil
}

// specFromOverlay projects an overlay's connection fields onto the in-memory
// DynamicMCPServerSpec the pool's caller factory resolves names through.
// RFC N: the spec carries its tenant so the registry keys it by
// (tenant, name).
func specFromOverlay(tenantID, name string, ov mcpServerOverlay) loommcp.DynamicMCPServerSpec {
	return loommcp.DynamicMCPServerSpec{
		TenantID:  tenantID,
		Name:      name,
		Transport: ov.Transport,
		URL:       ov.URL,
		Headers:   ov.Headers,
	}
}

// discoverTools runs the upstream's tools/list handshake via the pool and
// returns the result reshaped as toolDescriptors. The caller MUST have
// registered name in the registry first (so the pool's caller factory can
// resolve it). Evict-then-Get forces a fresh handshake. Bounded by a 30s
// budget so a non-responding peer can't park the goroutine — matches the
// boot-time MCP-init budget shape. Shared by create/fork (best-effort) and
// rediscover (fatal on error).
func (m *MCPServerDef) discoverTools(ctx context.Context, tenantID, name string) ([]toolDescriptor, error) {
	if m.Pool == nil {
		return nil, fmt.Errorf("pool not configured")
	}
	// RFC N: evict + dial the (tenant, name) entry. Pool.Get derives the
	// tenant from ctx (RunIdentity), which is this run's tenant == tenantID,
	// so the handshake dials this tenant's spec.
	m.Pool.Evict(tenantID, name)
	hsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, descs, err := m.Pool.Get(hsCtx, name)
	if err != nil {
		return nil, err
	}
	out := make([]toolDescriptor, 0, len(descs))
	for _, d := range descs {
		out = append(out, toolDescriptor{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
		})
	}
	return out, nil
}

// discoverOnIngestion is the best-effort create/fork discovery pass. When the
// new version will become active (promote) and discovery isn't opted out, it
// registers the spec so the pool can dial by name, runs the handshake, and —
// if the peer is reachable AND the result still fits MaxDefinitionBytes —
// folds discovered_tools into def and re-marshals. discovered_tools is NOT
// part of content_sha256, so the dedup hash computed by the caller stays
// valid. Returns the (possibly updated) defJSON + the discovered count.
// Never errors: an unreachable peer leaves def untouched (empty tools) and
// self-heals via lazy registration on first call.
func (m *MCPServerDef) discoverOnIngestion(ctx context.Context, tenantID, name string, in mcpServerDefInput, promote bool, def *mcpServerOverlay, defJSON []byte) ([]byte, int) {
	discover := promote && m.Pool != nil
	if in.Discover != nil {
		discover = discover && *in.Discover
	}
	if !discover {
		return defJSON, 0
	}
	// Register under (tenant, name) so pool.Get resolves to THIS tenant's
	// connection params during the discovery handshake.
	m.Registry.Set(specFromOverlay(tenantID, name, *def))
	tools, err := m.discoverTools(ctx, tenantID, name)
	if err != nil {
		return defJSON, 0 // best-effort — keep metadata-only def; lazy/rediscover later.
	}
	withTools := *def
	withTools.DiscoveredTools = tools
	b, merr := json.Marshal(withTools)
	if merr != nil || (m.MaxDefinitionBytes > 0 && len(b) > m.MaxDefinitionBytes) {
		// Tools push the definition past the cap → store metadata-only; the
		// operator can rediscover with a larger cap if they really need them.
		return defJSON, 0
	}
	*def = withTools
	return b, len(tools)
}

// ---- rediscover ----

func (m *MCPServerDef) execRediscover(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("rediscover: missing required field: name"), nil
	}
	// RFC N: tenant from ctx scopes the active lookup, the discovery
	// handshake, and the new row's stamp.
	tenantID := tools.RunIdentity(ctx).TenantID
	active, err := m.Store.MCPServerDefGetActive(ctx, tenantID, in.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("rediscover: no active row for name %q", in.Name)), nil
		}
		return errResult(fmt.Sprintf("rediscover: %s", err)), nil
	}
	if m.Pool == nil {
		return errResult("rediscover: pool not configured"), nil
	}
	newTools, err := m.discoverTools(ctx, tenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("rediscover: pool.Get: %s", err)), nil
	}
	// Reshape onto a new row version (fork-from-current; preserves audit history).
	var ov mcpServerOverlay
	if err := json.Unmarshal(active.Definition, &ov); err != nil {
		return errResult(fmt.Sprintf("rediscover: definition unmarshal: %s", err)), nil
	}
	// Idempotent rediscover: if the freshly-discovered tools match the active
	// def's, don't mint a new version — otherwise re-discovery on every boot
	// version-spams even when the peer's tool surface is unchanged.
	// content_sha256 excludes discovered_tools, so this is a direct (order- and
	// JSON-whitespace-insensitive) comparison.
	if canonicalTools(ov.DiscoveredTools) == canonicalTools(newTools) {
		resp := mcpServerDefRowResponse(active, true)
		resp["deduplicated"] = true
		resp["discovered"] = len(newTools)
		return okJSON(resp)
	}
	ov.DiscoveredTools = newTools
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
		TenantID:         tenantID,
	}
	created, err := m.Store.MCPServerDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("rediscover: persist: %s", err)), nil
	}
	if err := m.Store.MCPServerDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("rediscover: promote: %s", err)), nil
	}
	return okJSON(map[string]any{
		"def_id":         created.DefID,
		"name":           in.Name,
		"version":        created.Version,
		"discovered":     len(newTools),
		"content_sha256": created.ContentSHA256,
	})
}

// ---- verify ----

func (m *MCPServerDef) execVerify(ctx context.Context, in mcpServerDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	row, err := m.Store.MCPServerDefGetActive(ctx, tenantID, in.Name)
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
		return fmt.Errorf("host %q not in LOOMCYCLE_HTTP_HOST_ALLOWLIST or LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST (operator-blessed allowlists are the floor for outbound HTTP; loopback / RFC1918 callback hosts like a self-hosted localhost MCP server belong in the private allowlist)", host)
	}
	return nil
}

// hostAllowed delegates to the canonical package-level helper used by
// HTTP + WebFetch (httptool.go). Identical semantics across every
// tool that gates outbound HTTP on `LOOMCYCLE_HTTP_HOST_ALLOWLIST` is
// the load-bearing operator contract: the same yaml allowlist must
// produce the same allow/deny decision regardless of which tool the
// call goes through.
//
// A runtime-registered MCP server's host may be either a general outbound
// target (HTTPHostAllowlist) OR an operator-blessed loopback / RFC1918
// callback (HTTPPrivateHostAllowlist — the SAME exemption the HTTP/WebFetch
// *dial-time* SSRF guard already honours, see httptool.go). Consulting only
// the general floor here was an asymmetry the static `mcp_servers:` block
// hid: an operator-declared loopback callback (e.g. a self-hosted
// `http://localhost:3000/api/mcp`) would be refused at create even though
// the operator explicitly blessed it for outbound HTTP via the private
// allowlist — forcing them to widen the GENERAL SSRF floor just to register
// their own callback server. Both lists are operator-declared, so honouring
// either never widens beyond operator intent; it aligns create-time with
// dial-time.
func (m *MCPServerDef) hostAllowed(host string) bool {
	return hostAllowed(host, m.Cfg.Env.HTTPHostAllowlist) ||
		hostAllowed(host, m.Cfg.Env.HTTPPrivateHostAllowlist)
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
	// Mirror yaml-load's ${LOOMCYCLE_*} expansion on the operator-authored
	// connection fields. A yaml MCP server is expanded at config.Load; a
	// dynamically-registered one never passes through Load, so without this
	// the inner ${LOOMCYCLE_TOKEN} in a header template like
	//   Bearer ${run.credentials.jobs:-${LOOMCYCLE_JOBS_SEARCH_API_TOKEN}}
	// is stored verbatim. The request-time substituter's lazy `.*?` fallback
	// then truncates on the inner `}` (see internal/tools/mcp/http/
	// substitute.go:14, whose safety comment depends on this prior expansion)
	// and sends `Bearer ${LOOMCYCLE_…}` as a literal → 401 upstream.
	// Expanding here keeps the stored value flat (no nested brace), so
	// request-time substitution behaves identically for yaml- and substrate-
	// registered servers. The outer ${run.*} token carries a "." in its name,
	// which envVarRe ([A-Za-z_][A-Za-z0-9_]*) cannot match, so it survives to
	// request time untouched. Re-expanding an already-stored (forked) value is
	// a no-op: bearer tokens cannot contain `${…}` per the HTTP-boundary
	// charset. Caveat: this bakes the resolved token into the stored def
	// content (and thus content_sha256) — consistent with yaml semantics, and
	// dedup stays stable as long as the env value is stable.
	base.URL = config.ExpandEnv(base.URL)
	if len(base.Headers) > 0 {
		expanded := make(map[string]string, len(base.Headers))
		for k, v := range base.Headers {
			expanded[k] = config.ExpandEnv(v)
		}
		base.Headers = expanded
	}
	return base, nil
}

// signFromMCPServerOverlay computes the content_sha256 for a row's
// content. Mirror of signFromMergedDef in agentdef.go.
// canonicalTools returns a stable string representation of a discovered-
// tools list for equality comparison: tools sorted by name, and each
// input_schema re-marshalled through any (sorting object keys + stripping
// insignificant whitespace). Two lists that differ only in tool order or
// JSON formatting compare equal — so rediscover treats an unchanged peer
// surface as a no-op regardless of how the peer happened to serialize it.
func canonicalTools(tds []toolDescriptor) string {
	cp := make([]toolDescriptor, len(tds))
	for i, t := range tds {
		cp[i] = t
		if len(t.InputSchema) > 0 {
			var v any
			if json.Unmarshal(t.InputSchema, &v) == nil {
				if b, err := json.Marshal(v); err == nil {
					cp[i].InputSchema = b
				}
			}
		}
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i].Name < cp[j].Name })
	b, _ := json.Marshal(cp)
	return string(b)
}

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
