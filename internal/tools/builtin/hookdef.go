package builtin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// HookDef is the substrate tool for reusable hook definitions: one hook — the
// event it answers, what it matches, its body (code-js or a webhook), and how
// it fails — stored once, versioned, and named wherever it is used.
//
// A HookDef fires on nothing by itself. An AgentDef, a TeamDef or a run request
// names it, and that attachment decides which agent and which run it gates.
//
// Who may write one: the same principals who may write an AgentDef — a tenant's
// operators and its non-isolated members — through the operator surfaces
// (HTTP /v1/_hookdef, gRPC, the MCP meta-tool, the adapters). The route and RPC
// gates enforce that. The tool is never in an agent's tool list and has no
// *_def_scopes grant, so no agent can author a hook from inside a run: a hook
// is authority over the agent it gates, and an agent writing its own gate would
// be the gate asking itself.
//
// Differences from TeamDef, all deliberate:
//   - A fork's parent is resolved in the caller's own tenant only, for an
//     admin too. A shared ("") HookDef can be named by a tenant's definitions,
//     but not forked into the tenant: the fork would copy its body, which stays
//     with its author.
//   - A code-js body is compiled on write (CompileCode), so a broken body is
//     refused when it is saved rather than at its first matching call.
//   - get also takes a name (the active version), because a hook is named by
//     its name wherever it is used.
type HookDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// CompileCode parses a code-js body. nil means code hooks are not enabled
	// on this server, and a code body is refused.
	CompileCode func(src string) error

	// MaxDescriptionBytes caps the free-text rationale on create/fork. 0 = no
	// cap. (The hook's own description is capped by hooks.MaxDescriptionBytes.)
	MaxDescriptionBytes int
}

const hookDefDescription = `Author, fork, promote, retire, delete and inspect hook definitions. ` +
	`A HookDef is one reusable hook — the event it answers, the tools it matches, its body (code-js or a webhook URL), ` +
	`its fail mode and timeout. It fires on nothing by itself: an agent definition, a team definition or a run names it. ` +
	`Operations: create, fork, get, list, promote, retire, verify, delete.`

const hookDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","promote","retire","verify","delete"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Hook name (required for create/fork/list/verify/delete; get takes name or def_id). Segments of A-Z a-z 0-9 _ - joined by /."},
    "def_id":        {"type": "string", "description": "Existing def_id (get/promote/retire; get takes def_id or name)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional; defaults to the active version of name in your tenant)."},
    "overlay": {
      "type": "object",
      "description": "The hook's definition. On fork, each top-level field given replaces the parent's.",
      "properties": {
        "description": {"type": "string", "description": "Shown to whoever the hook denies, holds or rewrites. The body is never shown."},
        "event":       {"type": "string", "enum": ["pre","post","post_failure","agent_start","agent_stop","subagent_start","subagent_stop","pre_compact","post_compact","run_end"], "description": "What the hook answers. pre/post/post_failure wrap a tool call; the others are about the run."},
        "match":       {"type": "object", "properties": {"tools": {"type": "array", "items": {"type": "string"}, "description": "Exact tool names or trailing-* globs; empty = every tool. Tool events only."}}, "additionalProperties": false},
        "body":        {"type": "object", "properties": {"kind": {"type": "string", "enum": ["code-js","http"]}, "code": {"type": "string", "description": "code-js: a script defining function hook(ev) that returns the decision."}, "url": {"type": "string", "description": "http: the webhook URL."}, "headers": {"type": "object", "additionalProperties": {"type": "string"}, "description": "http: headers sent with each call. A value may name a credential as $cred:<name>, resolved for the run when the hook is called — keep secrets there, not in the URL or a literal value."}}, "required": ["kind"], "additionalProperties": false},
        "fail_mode":   {"type": "string", "enum": ["open","closed"], "description": "open (default): a failing hook lets the call through. closed: it stops it."},
        "timeout_ms":  {"type": "integer", "minimum": 0, "description": "Webhook: the whole call (default 5 s, max 60 s). code-js: each run of the script (default 50 ms, max 1 s)."}
      },
      "additionalProperties": false
    },
    "description":    {"type": "string", "description": "Free-text rationale for create/fork (distinct from overlay.description)."},
    "promote":        {"type": "boolean", "description": "create defaults true, fork defaults false."},
    "retired":        {"type": "boolean", "description": "Required for retire — true to retire, false to un-retire."},
    "content_sha256": {"type": "string", "description": "verify: the hash to compare with the active version's."}
  },
  "required": ["op"]
}`

type hookDefInput struct {
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

// Name implements tools.Tool.
func (h *HookDef) Name() string { return "HookDef" }

// Description implements tools.Tool.
func (h *HookDef) Description() string { return hookDefDescription }

// InputSchema implements tools.Tool.
func (h *HookDef) InputSchema() json.RawMessage { return json.RawMessage(hookDefInputSchema) }

// Execute implements tools.Tool.
func (h *HookDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if h.Store == nil {
		return errResult("HookDef tool: not configured (no Store backend)"), nil
	}
	// The operator surfaces call this with no run on ctx. A run id means an
	// agent reached the tool — which no wiring gives it — and an agent must not
	// write the hook that gates it, so refuse rather than trust the wiring.
	if tools.RunID(ctx) != "" {
		return errResult("HookDef is not available inside a run; hooks are written through the operator surfaces"), nil
	}
	var in hookDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	switch in.Op {
	case "create":
		return h.execCreate(ctx, in)
	case "fork":
		return h.execFork(ctx, in)
	case "get":
		return h.execGet(ctx, in)
	case "list":
		return h.execList(ctx, in)
	case "promote":
		return h.execPromote(ctx, in)
	case "retire":
		return h.execRetire(ctx, in)
	case "verify":
		return h.execVerify(ctx, in)
	case "delete":
		return h.execDelete(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, promote, retire, verify, delete)", in.Op)), nil
	}
}

func (h *HookDef) execCreate(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if err := hooks.ValidateDefName(in.Name); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	def, err := h.buildDefinition(nil, in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	return h.write(ctx, "create", in, "", def, true)
}

func (h *HookDef) execFork(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if err := hooks.ValidateDefName(in.Name); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	var parent store.HookDefRow
	if in.ParentDefID != "" {
		row, err := h.Store.HookDefGet(ctx, in.ParentDefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: parent_def_id %q not found", in.ParentDefID)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		// Own tenant only, admin included: a fork copies the parent's body,
		// which stays with the tenant that wrote it, and a lineage that crossed
		// tenants would pin the parent's rows against its own tenant's delete.
		if row.TenantID != tenantID {
			return errResult(fmt.Sprintf("fork: parent_def_id %q not found", in.ParentDefID)), nil
		}
		if row.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", in.ParentDefID, row.Name, in.Name)), nil
		}
		parent = row
	} else {
		row, err := h.Store.HookDefGetActive(ctx, tenantID, in.Name)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: no parent — hook %q has no active version in this tenant (use create, or pass parent_def_id)", in.Name)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		parent = row
	}
	def, err := h.buildDefinition(parent.Definition, in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	return h.write(ctx, "fork", in, parent.DefID, def, false)
}

// write validates the merged definition and stores it as a new version,
// promoting it when asked (defaultPromote when the caller did not say).
func (h *HookDef) write(ctx context.Context, op string, in hookDefInput, parentDefID string, def hooks.Def, defaultPromote bool) (tools.Result, error) {
	def.Normalize()
	if err := def.Validate(); err != nil {
		return errResult(fmt.Sprintf("%s: %s", op, err)), nil
	}
	if def.Body.Kind == hooks.BodyKindCode {
		if h.CompileCode == nil {
			return errResult(fmt.Sprintf("%s: code hooks are not enabled on this server (set LOOMCYCLE_CODE_HOOKS_ENABLED=1)", op)), nil
		}
		if err := h.CompileCode(def.Body.Code); err != nil {
			return errResult(fmt.Sprintf("%s: body.code: %s", op, err)), nil
		}
	}
	if h.MaxDescriptionBytes > 0 && len(in.Description) > h.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("%s: description (%d bytes) exceeds the limit (%d)", op, len(in.Description), h.MaxDescriptionBytes)), nil
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("%s: marshal: %s", op, err)), nil
	}
	ident := tools.RunIdentity(ctx)
	created, err := h.Store.HookDefCreate(ctx, store.HookDefRow{
		DefID:            mintHookDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    hooks.SignDef(in.Name, def),
		TenantID:         ident.TenantID,
	})
	if err != nil {
		return errResult(fmt.Sprintf("%s: %s", op, err)), nil
	}
	promote := defaultPromote
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := h.Store.HookDefSetActive(ctx, ident.TenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("%s: promote: %s", op, err)), nil
		}
	}
	return okJSON(hookDefRowResponse(created, promote))
}

// buildDefinition merges an overlay onto a parent definition: each top-level
// field the overlay carries replaces the parent's. A null decodes to the
// field's zero value, so it clears the field.
func (h *HookDef) buildDefinition(parent json.RawMessage, overlay json.RawMessage) (hooks.Def, error) {
	merged := map[string]json.RawMessage{}
	if len(parent) > 0 {
		if err := json.Unmarshal(parent, &merged); err != nil {
			return hooks.Def{}, fmt.Errorf("parse parent definition: %w", err)
		}
	}
	if len(overlay) > 0 {
		var ov map[string]json.RawMessage
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return hooks.Def{}, fmt.Errorf("parse overlay: %w", err)
		}
		for k, v := range ov {
			merged[k] = v
		}
	}
	buf, err := json.Marshal(merged)
	if err != nil {
		return hooks.Def{}, err
	}
	var def hooks.Def
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return hooks.Def{}, fmt.Errorf("overlay: %w", err)
	}
	return def, nil
}

func (h *HookDef) execGet(ctx context.Context, in hookDefInput) (tools.Result, error) {
	tenantID := tools.RunIdentity(ctx).TenantID
	var row store.HookDefRow
	var err error
	switch {
	case in.DefID != "":
		row, err = h.Store.HookDefGet(ctx, in.DefID)
		if err == nil && !defCallerIsAdmin(ctx) && row.TenantID != tenantID {
			err = &store.ErrNotFound{}
		}
	case in.Name != "":
		row, err = h.Store.HookDefGetActive(ctx, tenantID, in.Name)
	default:
		return errResult("get: missing required field: def_id or name"), nil
	}
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			if in.DefID != "" {
				return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
			}
			return errResult(fmt.Sprintf("get: hook %q has no active version", in.Name)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	return okJSON(hookDefRowResponse(row, false))
}

func (h *HookDef) execList(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	rows, err := h.Store.HookDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, hookDefRowResponseMap(r))
	}
	return okJSONCount(map[string]any{"name": in.Name, "versions": out}, len(out))
}

// ownRow reads a def by id and hides another tenant's as not found.
func (h *HookDef) ownRow(ctx context.Context, op, defID string) (store.HookDefRow, *tools.Result) {
	if defID == "" {
		r := errResult(fmt.Sprintf("%s: missing required field: def_id", op))
		return store.HookDefRow{}, &r
	}
	row, err := h.Store.HookDefGet(ctx, defID)
	if err != nil {
		var nf *store.ErrNotFound
		var r tools.Result
		if errors.As(err, &nf) {
			r = errResult(fmt.Sprintf("%s: def_id %q not found", op, defID))
		} else {
			r = errResult(fmt.Sprintf("%s: %s", op, err))
		}
		return store.HookDefRow{}, &r
	}
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		r := errResult(fmt.Sprintf("%s: def_id %q not found", op, defID))
		return store.HookDefRow{}, &r
	}
	return row, nil
}

func (h *HookDef) execPromote(ctx context.Context, in hookDefInput) (tools.Result, error) {
	row, refused := h.ownRow(ctx, "promote", in.DefID)
	if refused != nil {
		return *refused, nil
	}
	// Promote within the def's own tenant: an admin promoting another tenant's
	// def moves that tenant's pointer, never its own.
	ident := tools.RunIdentity(ctx)
	if err := h.Store.HookDefSetActive(ctx, row.TenantID, row.Name, row.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
}

func (h *HookDef) execRetire(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	if _, refused := h.ownRow(ctx, "retire", in.DefID); refused != nil {
		return *refused, nil
	}
	if err := h.Store.HookDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

func (h *HookDef) execVerify(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	row, err := h.Store.HookDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return okJSON(map[string]any{
				"matches": false, "current_sha256": "", "current_def_id": "",
				"version": 0, "name": in.Name, "deployed": false,
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

func (h *HookDef) execDelete(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("delete: missing required field: name"), nil
	}
	deleted, err := h.Store.HookDefDelete(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("delete: %s", err)), nil
	}
	if !deleted {
		return errResult(fmt.Sprintf("delete: hook %q not found", in.Name)), nil
	}
	return okJSON(map[string]any{"name": in.Name, "deleted": true})
}

func hookDefRowResponse(row store.HookDefRow, promoted bool) map[string]any {
	m := hookDefRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func hookDefRowResponseMap(row store.HookDefRow) map[string]any {
	return map[string]any{
		"def_id":              row.DefID,
		"name":                row.Name,
		"version":             row.Version,
		"parent_def_id":       row.ParentDefID,
		"description":         row.Description,
		"created_at":          row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"created_by_agent_id": row.CreatedByAgentID,
		"retired":             row.Retired,
		"content_sha256":      row.ContentSHA256,
		"tenant_id":           row.TenantID,
		"definition":          row.Definition,
	}
}

// mintHookDefID returns a fresh opaque id, prefixed "hdf_" so hook defs never
// collide with other defs in logs.
func mintHookDefID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "hdf_" + hex.EncodeToString(b[:])
}

// HookDefLookup reads HookDef versions from st for hooks.Resolve: the active
// version when version is 0, a pinned one otherwise. A retired version is not
// served, as a retired AgentDef is not.
func HookDefLookup(st store.Store) hooks.LookupDef {
	return func(ctx context.Context, tenant, name string, version int) (hooks.Def, string, error) {
		if st == nil {
			return hooks.Def{}, "", errors.New("no store: HookDefs are not available")
		}
		var (
			row store.HookDefRow
			err error
		)
		if version == 0 {
			row, err = st.HookDefGetActive(ctx, tenant, name)
		} else {
			row, err = st.HookDefGetByNameVersion(ctx, tenant, name, version)
		}
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				if version > 0 {
					return hooks.Def{}, "", fmt.Errorf("no HookDef %s@%d", name, version)
				}
				return hooks.Def{}, "", fmt.Errorf("no HookDef %q", name)
			}
			return hooks.Def{}, "", err
		}
		if row.Retired {
			return hooks.Def{}, "", fmt.Errorf("HookDef %s v%d is retired", name, row.Version)
		}
		var def hooks.Def
		if err := json.Unmarshal(row.Definition, &def); err != nil {
			return hooks.Def{}, "", fmt.Errorf("HookDef %s: %w", name, err)
		}
		return def, row.DefID, nil
	}
}
