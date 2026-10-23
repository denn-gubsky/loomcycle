package builtin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
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

	// Credentials answers whether the credential an http body's signing_secret
	// names exists in the author's tenant (requireAuthorBoundSigningSecret).
	// nil = no credential store: a non-admin's signing_secret is refused.
	Credentials *PeerCredentials

	// Cfg holds the operator's static agents, whose hook references resolve
	// in the shared layer. Nil = none.
	Cfg *config.Config

	// Audit records an operator changing a HookDef that a no-trace agent
	// runs (LOOMCYCLE_AUDIT_LOG_PATH). Nil = no audit log: the process log
	// line is then the record.
	Audit audit.Sink
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
        "event":       {"type": "string", "enum": ["pre","post","post_failure","agent_start","agent_stop","subagent_start","subagent_stop","pre_compact","post_compact","run_end","channel_publish"], "description": "What the hook answers. pre/post/post_failure wrap a tool call; channel_publish decides on each message of a channel that attaches it (release, rewrite, drop or hold); the others are about the run."},
        "match":       {"type": "object", "properties": {"tools": {"type": "array", "items": {"type": "string"}, "description": "Exact tool names or trailing-* globs; empty = every tool. Tool events only."}}, "additionalProperties": false},
        "body":        {"type": "object", "properties": {"kind": {"type": "string", "enum": ["code-js","http"]}, "code": {"type": "string", "description": "code-js: a script defining function hook(ev) that returns the decision."}, "url": {"type": "string", "description": "http: the webhook URL."}, "headers": {"type": "object", "additionalProperties": {"type": "string"}, "description": "http: headers sent with each call. A value may name a credential as $cred:<name>, resolved for the run when the hook is called — keep secrets there, not in the URL or a literal value. X-Loomcycle-Signature, X-Loomcycle-Hook and X-Loomcycle-Delivery are reserved."}, "signing_secret": {"type": "string", "description": "http: sign every call with this credential, named as $cred:<name> (a literal secret is refused). Each call then carries X-Loomcycle-Signature: t=<unix>,v1=<hex HMAC-SHA256 of t + \".\" + the raw body>, and its body a delivery_id, so the receiver can tell the call came from here; it dedupes on the body's delivery_id, not on the unsigned X-Loomcycle-Delivery header. The credential must exist in your tenant and, if it is restricted to URLs, cover url. A call whose secret cannot be resolved is not sent; fail_mode decides."}}, "required": ["kind"], "additionalProperties": false},
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
	// RFC EZ §1.2: the op's own verb, asked of the hook definition it names.
	if res, refused := tools.AuthorizeDefOp(ctx, "hook", in.Op, defOpNames(ctx, in.Name, func(ctx context.Context, id string) (string, string, error) {
		row, err := h.Store.HookDefGet(ctx, id)
		return row.Name, row.TenantID, err
	}, in.DefID, in.ParentDefID)...); refused {
		return res, nil
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
	if err := requireAuthorBoundSigningSecret(ctx, h.Credentials, def.Body); err != nil {
		return errResult(fmt.Sprintf("%s: %s", op, err)), nil
	}
	if h.MaxDescriptionBytes > 0 && len(in.Description) > h.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("%s: description (%d bytes) exceeds the limit (%d)", op, len(in.Description), h.MaxDescriptionBytes)), nil
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("%s: marshal: %s", op, err)), nil
	}
	ident := tools.RunIdentity(ctx)
	if err := h.admitNoTraceHookDefChange(ctx, op, ident.TenantID, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
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

// requireAuthorBoundSigningSecret limits the credential a NON-ADMIN author may
// name as an http body's signing_secret to a tenant-level credential of the
// author's own tenant — the tenant the definition is written in — that a call
// to the body's url could resolve: unrestricted, or restricted by allowed_urls
// to a list covering url. The secret is never sent, but every call carries an
// HMAC keyed by it to a URL the author chose; naming a credential the author's
// tenant does not hold, or one restricted away from url, would make the hook a
// way to obtain MACs under a key the author cannot read.
//
// Refused for an author with no tenant: that definition lands in the shared
// layer, where the reference would resolve in every tenant that runs it.
//
// An admin, and an open-mode or stdio operator (callerHasOperatorAuthority),
// may name any credential, including one created after the definition — as an
// admin's api_key_env "$cred:" is not checked. A snapshot restore is an admin
// action, so its validator checks the reference's shape only (hooks.Def.Validate).
//
// A fork re-checks the merged definition: a credential deleted or restricted
// since the parent was written is refused even when the fork leaves the field
// as it was, because the version written must be one that can be used.
func requireAuthorBoundSigningSecret(ctx context.Context, creds *PeerCredentials, body hooks.DefBody) error {
	if body.SigningSecret == "" || callerHasOperatorAuthority(ctx) {
		return nil
	}
	name, ok := credential.ParseRef(body.SigningSecret)
	if !ok {
		return fmt.Errorf("body.signing_secret must name a credential as \"$cred:<name>\"") // Validate refuses it first
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	if tenantID == "" {
		return fmt.Errorf("body.signing_secret %q may not be set by an author with no tenant: the definition would be shared by every tenant", body.SigningSecret)
	}
	found, usable, err := creds.ExistsFor(ctx, tenantID, name, body.URL)
	if err != nil {
		return fmt.Errorf("body.signing_secret %q: could not check the credential: %v", body.SigningSecret, err)
	}
	if !found {
		return fmt.Errorf("body.signing_secret %q names no tenant-level credential in tenant %q. "+
			"Create it first with CredentialDef (op=create, scope=tenant, name=%q)", body.SigningSecret, tenantID, name)
	}
	if !usable {
		return fmt.Errorf("body.signing_secret %q is restricted by allowed_urls to URLs that do not cover body.url %q", body.SigningSecret, body.URL)
	}
	return nil
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
	if err := h.admitNoTraceHookDefChange(ctx, "promote", row.TenantID, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := h.Store.HookDefSetActive(ctx, row.TenantID, row.Name, row.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
}

func (h *HookDef) execRetire(ctx context.Context, in hookDefInput) (tools.Result, error) {
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, refused := h.ownRow(ctx, "retire", in.DefID)
	if refused != nil {
		return *refused, nil
	}
	// Retiring the active version, or bringing one back, changes what a
	// reference without a version resolves to.
	if err := h.admitNoTraceHookDefChange(ctx, "retire", row.TenantID, row.Name); err != nil {
		return errResult(err.Error()), nil
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
	if err := h.admitNoTraceHookDefChange(ctx, "delete", tools.RunIdentity(ctx).TenantID, in.Name); err != nil {
		return errResult(err.Error()), nil
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

// admitNoTraceHookDefChange refuses a change to which version of HookDef
// name, in tenant, a reference without a version resolves to — when a no-trace
// agent runs it that way — unless the caller is an operator (the AgentDef
// gate's rule, callerMayLiftNoTrace). A hook is sent what a run says; changing
// the one a no-trace agent runs is changing that agent's hooks. An operator's
// change is recorded, and refused when the record cannot be written.
func (h *HookDef) admitNoTraceHookDefChange(ctx context.Context, op, tenant, name string) error {
	// The shared layer is what every tenant's agents fall back to: changing
	// it is the operator's, whatever names it. Decided before anything is
	// read, so the answer depends on the caller alone and says nothing of any
	// tenant's agents.
	if tenant == "" && !callerMayLiftNoTrace(ctx) {
		return fmt.Errorf("%s: a shared hook %q may only be changed by an operator", op, name)
	}
	agents, err := h.noTraceAgentsNaming(ctx, tenant, name)
	if err != nil {
		return fmt.Errorf("%s: checking which agents run hook %q: %w", op, name, err)
	}
	if len(agents) == 0 {
		return nil
	}
	if !callerMayLiftNoTrace(ctx) {
		// The agents are named only to a caller in their own tenant. A
		// non-operator only ever writes its own tenant's HookDefs (ownRow
		// hides another tenant's), so this is belt and braces.
		if tenant != tools.RunIdentity(ctx).TenantID {
			return fmt.Errorf("%s: hook %q is run by a no-trace agent, which names it without a version; only an operator may change it", op, name)
		}
		return fmt.Errorf("%s: hook %q is run by no-trace agent(s) %s, which name it without a version; only an operator may change which version they run", op, name, strings.Join(agents, ", "))
	}
	subject := ""
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		subject = p.Subject
	}
	if h.Audit != nil {
		if err := h.Audit.Record(audit.Event{
			Action: "retention_none_hookdef_changed", TargetTenant: tenant, TargetName: name,
			ActorTenant: tools.RunIdentity(ctx).TenantID, ActorSubject: subject,
		}); err != nil {
			return fmt.Errorf("%s: hook %q was not changed: its audit record could not be written: %w", op, name, err)
		}
	}
	log.Printf("hookdef: %s by %q of hook %q in tenant %q, run by no-trace agent(s) %s", op, subject, name, tenant, strings.Join(agents, ", "))
	return nil
}

// noTraceAgentsNaming lists the no-trace agents of tenant — active or newest
// version "retention: none", or a mode the operator's config sets for the
// name — whose hooks name HookDef name without a version; for the shared
// layer (""), the operator's static agents too, which resolve there.
//
// Only the HookDef's own tenant: the answer reaches the caller (refused or
// not, and the names in the refusal), so an agent of another tenant must not
// change it. A tenant's agent that falls back to a shared HookDef is covered
// by the shared layer being the operator's to change at all.
func (h *HookDef) noTraceAgentsNaming(ctx context.Context, tenant, name string) ([]string, error) {
	var out []string
	staticNone := func(agent string) bool {
		if h.Cfg == nil {
			return false
		}
		a, ok := h.Cfg.Agents[agent]
		return ok && a.Retention == retentionmode.None
	}
	if tenant == "" && h.Cfg != nil {
		for agent, def := range h.Cfg.Agents {
			if def.Retention == retentionmode.None && namesUnpinned(def.Hooks, def.ToolHooks, name) {
				out = append(out, agent)
			}
		}
	}
	names, err := h.Store.AgentDefListNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if n.TenantID != tenant {
			continue
		}
		noTrace, current, err := noTraceLineage(ctx, h.Store, n.TenantID, n.Name)
		if err != nil {
			return nil, err
		}
		if (!noTrace && !staticNone(n.Name)) || current == nil {
			continue
		}
		var d struct {
			Hooks     hooks.EventHooks `json:"hooks"`
			ToolHooks hooks.ToolHooks  `json:"tool_hooks"`
		}
		// A definition whose hooks cannot be read cannot be shown not to
		// name this one.
		if json.Unmarshal(current, &d) != nil || namesUnpinned(d.Hooks, d.ToolHooks, name) {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// namesUnpinned reports whether agent or tool hooks reference HookDef name
// without a version.
func namesUnpinned(agent hooks.EventHooks, tool hooks.ToolHooks, name string) bool {
	in := func(e hooks.EventHooks) bool {
		for _, entries := range e {
			for _, entry := range entries {
				if entry.Inline != nil || entry.Ref == "" {
					continue
				}
				if n, v, err := hooks.ParseRef(entry.Ref); err == nil && n == name && v == 0 {
					return true
				}
			}
		}
		return false
	}
	if in(agent) {
		return true
	}
	for _, e := range tool {
		if in(e) {
			return true
		}
	}
	return false
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
					return hooks.Def{}, "", hooks.DefNotFound(fmt.Errorf("no HookDef %s@%d", name, version))
				}
				return hooks.Def{}, "", hooks.DefNotFound(fmt.Errorf("no HookDef %q", name))
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

// HookDefBodyValidator returns the authoring validation of a stored HookDef
// body, for a snapshot restore to run before writing one: it decodes strictly
// as a hook definition, passes the event and body-kind rules (an http body's
// url and the headers a call may send), and a code-js body compiles with
// compile — the host's code-hook runner. compile nil means code hooks are not
// enabled on this host, and a code-js body is refused, as a create here would
// refuse it.
func HookDefBodyValidator(compile func(src string) error) func(json.RawMessage) error {
	return func(body json.RawMessage) error {
		var def hooks.Def
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&def); err != nil {
			return fmt.Errorf("definition does not decode as a hook def: %w", err)
		}
		if err := def.Validate(); err != nil {
			return err
		}
		if def.Body.Kind != hooks.BodyKindCode {
			return nil
		}
		if compile == nil {
			return fmt.Errorf("a code-js body, and code hooks are not enabled on this host (LOOMCYCLE_CODE_HOOKS_ENABLED)")
		}
		if err := compile(def.Body.Code); err != nil {
			return fmt.Errorf("body.code: %w", err)
		}
		return nil
	}
}
