package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentDefNameRe is the AgentDef substrate name floor — the same charset as
// the HTTP validIdent / RegisterAgent / library_admin checks, so every
// agent-name entry point is consistent. It matters doubly for code-js: an
// agent name is also a path segment (agent_code/<name>/index.js), so
// rejecting "/", "\\", "..", etc. here (plus the compiler.load floor) keeps a
// substrate-authored name from escaping CodeRoot.
var agentDefNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AgentDef is the v0.8.5 built-in tool that lets agents author, fork,
// promote, retire, and inspect agent definitions at runtime. Static
// `<name>.md` files remain the operator-blessed root; this tool
// produces the DERIVED layer of agent-authored versions.
//
// Six operations dispatched off the `op` field:
//
//	create   — declare a brand-new agent name with a v1 definition.
//	           Refused if `name` matches a static cfg.Agents entry
//	           (operators expect their MDs to be ground truth).
//	fork     — make a new version from an existing parent (DB-resolved
//	           by def_id, or by-name from the active pointer). Default
//	           does NOT auto-promote — orchestrators promote
//	           explicitly after evaluation.
//	get      — fetch one row by def_id.
//	list     — list versions for a name, version DESC.
//	retire   — flip the retired flag on a def. Row stays visible in
//	           lineage; resolver skips retired when picking active.
//	promote  — set the active pointer for a name to a specific def_id.
//
// Tools enforcement: forks may NARROW but NEVER widen. The
// operator-blessed root (static cfg.Agents[name].Tools if it
// exists, else the v1 row's Tools) is the permanent capability
// ceiling. A fork that tries to add a tool not in the root is
// rejected with a typed error.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity), created_by_run_id (from ctx). The model
// NEVER supplies these.
type AgentDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the
	// operator-blessed root (cfg.Agents[name]) for Tools
	// ceiling enforcement and to refuse `create` over a static name.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON
	// (LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES). 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field
	// (LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES). 0 = no cap.
	MaxDescriptionBytes int

	// MaxCodeBytes caps an inline code-js `code_body` overlay (RFC J).
	// A dedicated cap (vs the whole-definition MaxDefinitionBytes) gives
	// a clearer error and a tighter default for executable source.
	// 0 = no cap.
	MaxCodeBytes int
}

const agentDefDescription = `Author, fork, promote, retire, and inspect agent definitions at runtime. ` +
	`Static <name>.md files remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of agent-authored versions. Tools may be NARROWED ` +
	`on forks but never WIDENED — operator-blessed root is the permanent capability ceiling. ` +
	`Operations: create, fork, get, list, retire, promote.`

const agentDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","promote"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Agent name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire/promote)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of AgentDef for create/fork (snake_case keys). Common fields: provider, model, tier, effort, system_prompt, code_body, tools, skills, providers, models, max_tokens, max_iterations, max_concurrent_children, run_timeout_seconds, memory_scopes, memory_quota_bytes, memory_backend, retry_attempts. Interactive / multi-agent config also round-trips and is content-identifying: channels ({publish:[...],subscribe:[...]}), evaluation_scopes ([...]), interruption ({enabled,kinds:[...],max_pending}). Immutable / server-set fields (def_id, version, parent_def_id, created_*, bootstrapped_from_static) are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create defaults true, fork defaults false. When true, sets the active pointer for the name to the new def."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
  },
  "required": ["op"]
}`

type agentDefInput struct {
	Op          string          `json:"op"`
	Name        string          `json:"name,omitempty"`
	DefID       string          `json:"def_id,omitempty"`
	ParentDefID string          `json:"parent_def_id,omitempty"`
	Overlay     json.RawMessage `json:"overlay,omitempty"`
	Description string          `json:"description,omitempty"`
	Promote     *bool           `json:"promote,omitempty"`
	Retired     *bool           `json:"retired,omitempty"`
	// ContentSHA256 — input for `op: verify`. Operator passes the
	// hash they computed locally (via `loomcycle hash agent`); the
	// tool compares against the active row's content_sha256 and
	// returns { matches, current_sha256, current_def_id, version }.
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

// Name implements tools.Tool.
func (a *AgentDef) Name() string { return "AgentDef" }

// Description implements tools.Tool.
func (a *AgentDef) Description() string { return agentDefDescription }

// InputSchema implements tools.Tool.
func (a *AgentDef) InputSchema() json.RawMessage { return json.RawMessage(agentDefInputSchema) }

// Execute implements tools.Tool.
func (a *AgentDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if a.Store == nil {
		return errResult("AgentDef tool: not configured (no Store backend)"), nil
	}
	if a.Cfg == nil {
		return errResult("AgentDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	var in agentDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.AgentDefPolicy(ctx)

	switch in.Op {
	case "create":
		return a.execCreate(ctx, policy, in)
	case "fork":
		return a.execFork(ctx, policy, in)
	case "get":
		return a.execGet(ctx, policy, in)
	case "list":
		return a.execList(ctx, policy, in)
	case "retire":
		return a.execRetire(ctx, policy, in)
	case "promote":
		return a.execPromote(ctx, policy, in)
	case "verify":
		return a.execVerify(ctx, policy, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, promote, verify)", in.Op)), nil
	}
}

// ---- create ----

// defCallerIsAdmin reports whether the ctx principal holds substrate:admin —
// the super-admin who crosses tenant boundaries BY DESIGN (RFC L, mirroring
// sessionOwnershipOK / tenantVisible). The def-tool read guards consult it so
// the role-aware Web UI library (admin) can view EVERY tenant's def bodies
// (the get/list ops back the library + lineage panel), while a non-admin
// (tenant-scoped) caller — e.g. an agent calling AgentDef.list mid-run — still
// sees only its own tenant. Without this, an admin viewing the library got
// empty bodies for every def outside its own principal tenant (notably the
// shared "" tenant where bootstrapped/legacy defs live).
func defCallerIsAdmin(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return ok && auth.HasScope(p.Scopes, auth.ScopeAdmin)
}

// operatorKeyRestrictedFromCtx computes the RFC AX operator-key restriction for
// the principal AUTHORING a trigger def, from the live ctx principal + the
// deployment gate. It is SERVER authority — a run-triggering def (Schedule /
// Webhook / A2A) captures it so the scheduler/webhook/A2A executor can stamp the
// fired run without a token on ctx (anti-bypass: a restricted principal can't
// launder an unrestricted run through a trigger). The model must NOT be able to
// set this via the overlay, so write sites stamp it unconditionally after
// applying the overlay. Fail-open (false) when the gate is off / no principal /
// legacy / scope-present, matching auth.OperatorKeyRestricted.
func operatorKeyRestrictedFromCtx(ctx context.Context, cfg *config.Config) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	gate := cfg != nil && cfg.Env.OperatorKeyRestriction
	return auth.OperatorKeyRestricted(p, ok, gate)
}

func (a *AgentDef) execCreate(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if !agentDefNameRe.MatchString(in.Name) {
		return errResult(fmt.Sprintf("create: name %q invalid (must match [A-Za-z0-9_-]{1,128})", in.Name)), nil
	}
	if err := a.checkScopeForName(policy, in.Name, ""); err != nil {
		return errResult(err.Error()), nil
	}
	// Static-name-replace refusal — operator-blessed MD is ground truth.
	// Anyone wanting to evolve a static agent must fork, not create.
	if _, ok := a.Cfg.Agents[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.Agents entry — use `fork` to derive a new version", in.Name)), nil
	}

	def, err := a.buildDefinition(ctx, in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// Tools ceiling on `create`: the caller's own effective
	// tools is the ceiling for any new agent it mints. Without
	// this check an agent with narrow tools could call
	// `create` with overlay.tools = [the entire universe] and
	// then spawn the resulting agent — a capability-escalation path.
	// Mirror of the subset check in `fork`.
	//
	// AgentTools(ctx) returns nil in test contexts; we refuse to
	// create with a non-empty Tools overlay when the ceiling
	// is unknown rather than silently allowing widening.
	callerTools := tools.AgentTools(ctx)
	if len(def.Tools) > 0 {
		if callerTools == nil {
			return errResult("create: caller's effective tools not on ctx (runtime misconfiguration); refuse rather than risk silent widening"), nil
		}
		if err := assertToolsSubset(def.Tools, callerTools); err != nil {
			return errResult(fmt.Sprintf("create: %s", err)), nil
		}
	}
	if err := a.validateInlineCode("create", def); err != nil {
		return errResult(err.Error()), nil
	}
	def.normalize()
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	if a.MaxDefinitionBytes > 0 && len(defJSON) > a.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("create: definition (%d bytes) exceeds max %d", len(defJSON), a.MaxDefinitionBytes)), nil
	}
	if a.MaxDescriptionBytes > 0 && len(in.Description) > a.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("create: description (%d bytes) exceeds max %d", len(in.Description), a.MaxDescriptionBytes)), nil
	}

	ident := tools.RunIdentity(ctx)
	// RFC N: the tenant comes from the authoritative run identity in ctx
	// (the AgentDef tool always runs inside a run whose RunIdentity
	// carries the principal-derived tenant), never from tool input. ""
	// = shared/legacy tenant. Used for the dedup probe, the row stamp,
	// and the promote — all scoped to the agent's own tenant.
	tenantID := ident.TenantID

	contentSHA := signFromMergedDef(in.Name, def)
	// Idempotent create: if the active def already carries this exact content,
	// return it as a no-op instead of minting a byte-identical new version. A
	// consumer that blindly re-registers on every restart (the TS client's
	// ensureCodeAgent flow) no longer spams the lineage. Mirror of
	// MCPServerDef.execCreate; compared only against the ACTIVE row, so
	// re-creating content that matches a non-active version still mints +
	// promotes (re-activation is a real state change).
	if active, gerr := a.Store.AgentDefGetActive(ctx, tenantID, in.Name); gerr == nil && active.ContentSHA256 == contentSHA {
		resp := rowResponse(active, true)
		resp["deduplicated"] = true
		return okJSON(resp)
	}

	row := store.AgentDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    contentSHA,
		TenantID:         tenantID,
		// CreatedByRunID stays empty here — there's no run_id on
		// RunIdentityValue today; carried via the run ctx separately.
	}
	created, err := a.Store.AgentDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := a.Store.AgentDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(rowResponse(created, promote))
}

// ---- fork ----

func (a *AgentDef) execFork(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}
	if !agentDefNameRe.MatchString(in.Name) {
		return errResult(fmt.Sprintf("fork: name %q invalid (must match [A-Za-z0-9_-]{1,128})", in.Name)), nil
	}

	// Resolve the parent. Paths, in order:
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + OWN-tenant active pointer → use it
	//   3. else + a SHARED ("") active pointer (when tenantID != "") →
	//      fork the shared base into the caller's tenant. Mirrors the
	//      run-time lookup.Agent precedence (own-tenant → static → shared
	//      "") and fixes the admin-`list`-sees-it-but-`fork`-can't gap for
	//      registries seeded under the legacy "" tenant.
	//   4. neither → name must have a static MD; bootstrap v1 snapshot.
	// RFC N: fork resolves the parent within the agent's own tenant first
	// (then the shared "" base), but the new version is ALWAYS stamped under
	// the caller's own tenant (from the authoritative run identity, never
	// tool input).
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID

	parentDefID := in.ParentDefID
	var parent store.AgentDefRow
	if parentDefID != "" {
		row, err := a.Store.AgentDefGet(ctx, parentDefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: parent_def_id %q not found", parentDefID)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		if row.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, row.Name, in.Name)), nil
		}
		// A def_id is a global handle. A caller may fork the SHARED ("")
		// base — the common ground every tenant builds on (e.g. migrate a
		// pre-RFC-N / bootstrapped def to code-js) — or its OWN tenant's def;
		// the fork lands under the caller's tenant. Refuse only forking
		// ANOTHER specific tenant's private def (which would copy that
		// tenant's body across the boundary), unless the caller is a
		// substrate:admin (crosses tenants by design, RFC L). Without the ""
		// allowance, a legacy/default or tenant principal could not fork the
		// shared base at all.
		if row.TenantID != "" && row.TenantID != tenantID && !defCallerIsAdmin(ctx) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q belongs to another tenant, refusing", parentDefID)), nil
		}
		parent = row
	} else {
		// Try the active pointer in the caller's OWN tenant first.
		row, err := a.Store.AgentDefGetActive(ctx, tenantID, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No own-tenant active pointer. Before bootstrapping, fall back
			// to the SHARED ("") base — the same precedence the run-time
			// resolver walks (lookup.Agent: own-tenant dynamic → static →
			// shared "" dynamic). Without this, a per-tenant principal whose
			// registry was seeded under the legacy "" tenant could SEE a name
			// via the cross-tenant admin `list` yet be unable to fork it:
			// `list` reports "deployed", fork reports "no parent". Forking the
			// shared base lands the new version under the CALLER's tenant
			// (the row stamp below sets TenantID = tenantID), mirroring the
			// explicit-parent_def_id branch above which already permits
			// forking the "" base across the boundary. Skip when tenantID is
			// already "" — that lookup is identical to the one we just did.
			if tenantID != "" {
				if shared, serr := a.Store.AgentDefGetActive(ctx, "", in.Name); serr == nil {
					parent = shared
					parentDefID = shared.DefID
				} else if !errors.As(serr, &nf) {
					return errResult(fmt.Sprintf("fork: %s", serr)), nil
				}
			}
			// Still no parent (own-tenant AND shared "" both missed) →
			// bootstrap from static MD, else refuse.
			if parentDefID == "" {
				static, ok := a.Cfg.Agents[in.Name]
				if !ok {
					return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version (own tenant or shared \"\") nor a static cfg.Agents entry", in.Name)), nil
				}
				bootstrap, berr := a.bootstrapStatic(ctx, in.Name, static)
				if berr != nil {
					// A concurrent first-fork may have already bootstrapped
					// v1 between our AgentDefGetActive check and our own
					// bootstrap insert. The store's per-name lock guarantees
					// monotonic versions but our caller has no way to know
					// "another goroutine just won v1." Re-read the active
					// pointer once before propagating the error — if it's
					// now set, use that as our parent instead of failing.
					if row2, gerr := a.Store.AgentDefGetActive(ctx, tenantID, in.Name); gerr == nil {
						parent = row2
						parentDefID = row2.DefID
					} else {
						return errResult(fmt.Sprintf("fork: bootstrap static: %s", berr)), nil
					}
				} else {
					parent = bootstrap
					parentDefID = bootstrap.DefID
				}
			}
		}
	}

	if err := a.checkScopeForName(policy, in.Name, parentDefID); err != nil {
		return errResult(err.Error()), nil
	}

	def, err := a.buildDefinition(ctx, in.Name, string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// Tools ceiling enforcement — fork may narrow, never widen.
	root, err := a.resolveToolsRoot(ctx, in.Name, parent)
	if err != nil {
		return errResult(fmt.Sprintf("fork: resolve root: %s", err)), nil
	}
	if err := assertToolsSubset(def.Tools, root); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := a.validateInlineCode("fork", def); err != nil {
		return errResult(err.Error()), nil
	}

	def.normalize()
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("fork: marshal: %s", err)), nil
	}
	if a.MaxDefinitionBytes > 0 && len(defJSON) > a.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("fork: definition (%d bytes) exceeds max %d", len(defJSON), a.MaxDefinitionBytes)), nil
	}
	if a.MaxDescriptionBytes > 0 && len(in.Description) > a.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("fork: description (%d bytes) exceeds max %d", len(in.Description), a.MaxDescriptionBytes)), nil
	}

	row := store.AgentDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromMergedDef(in.Name, def),
		TenantID:         tenantID,
	}
	created, err := a.Store.AgentDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := false
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := a.Store.AgentDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(rowResponse(created, promote))
}

// ---- get / list ----

func (a *AgentDef) execGet(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := a.Store.AgentDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	// RFC N: def_id is a global handle but a def is owned by exactly one
	// tenant. checkScopeForName is tenant-blind, so guard here: a caller
	// in tenant T cannot read another tenant's def. Return the SAME opaque
	// not-found a missing def returns — never leak existence/body of a
	// cross-tenant row.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
	}
	if err := a.checkScopeForName(policy, row.Name, row.DefID); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(rowResponse(row, false))
}

func (a *AgentDef) execList(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := a.checkScopeForName(policy, in.Name, ""); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := a.Store.AgentDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	// RFC N: AgentDefListByName returns rows across ALL tenants for a
	// name (names are per-tenant now). Filter to the caller's own tenant
	// so a tenant lists only its own versions.
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, rowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire / promote ----

func (a *AgentDef) execRetire(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := a.Store.AgentDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// RFC N: refuse cross-tenant retire. AgentDefSetRetired is a global
	// by-def_id mutation; without this guard a caller in tenant T could
	// retire another tenant's def. Opaque not-found — don't leak existence.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
	}
	if err := a.checkScopeForName(policy, row.Name, row.DefID); err != nil {
		return errResult(err.Error()), nil
	}
	if err := a.Store.AgentDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

func (a *AgentDef) execPromote(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("promote: missing required field: def_id"), nil
	}
	row, err := a.Store.AgentDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("promote: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	if err := a.checkScopeForName(policy, row.Name, row.DefID); err != nil {
		return errResult(err.Error()), nil
	}
	ident := tools.RunIdentity(ctx)
	// RFC N: promote within the agent's own tenant. AgentDefSetActive
	// refuses when ident.TenantID ≠ row.TenantID, so a caller in tenant T
	// cannot point at (or clobber) another tenant's active pointer — even
	// though def_id is a global handle.
	if err := a.Store.AgentDefSetActive(ctx, ident.TenantID, row.Name, row.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
}

// execVerify answers "is the supplied content_sha256 the hash of the
// currently-active agent definition with this name?" Operators with
// Docker-bundled agents compute the hash of their local source via
// `loomcycle hash agent <path>` and pass it here; matches=false +
// the returned current_sha256 + current_def_id tells them they should
// re-push via `AgentDef set`. matches=true is the no-op signal.
//
// Returns matches=false + empty current_sha256 + empty current_def_id
// when the name doesn't exist at all. Doesn't fall back to the static
// cfg.Agents row — the question is specifically "what's IN THE DB?"
// since that's what loomcycle actually loads from.
func (a *AgentDef) execVerify(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	if err := a.checkScopeForName(policy, in.Name, ""); err != nil {
		return errResult(err.Error()), nil
	}
	// RFC N: verify against the agent's own tenant active pointer.
	row, err := a.Store.AgentDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			// Name not promoted → no deployed version → never matches.
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

// checkScopeForName enforces the agent's agent_def_scopes against a
// proposed (name, def_id) target. Default-deny when policy.Scopes
// is empty. The def_id arg is used for "descendants" path
// (currently treated as "any" since lineage-walk on every check is
// too expensive — flagged TODO; refines in v0.9.x).
func (a *AgentDef) checkScopeForName(policy tools.AgentDefPolicyValue, name, _ string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("AgentDef tool: agent has no agent_def_scopes (default-deny); add `agent_def_scopes: [...]` to the agent yaml")
	}
	for _, sc := range policy.Scopes {
		switch sc {
		case "any":
			return nil
		case "self":
			if name == policy.SelfName {
				return nil
			}
		case "descendants":
			// KNOWN GAP (TODO v0.9.x): `descendants` is intended to
			// permit mutation of agents in the calling agent's spawn
			// tree. Today the tool has no efficient way to verify
			// lineage against every check (cfg.Agents is the static
			// boundary; a DB-only descendant tree is N AgentDefGet
			// round-trips deep). For v0.8.5 we accept on "descendants"
			// present, which makes the scope behaviourally equivalent
			// to "any". Operators wanting strict descendant gating
			// must grant `named:<child>` per child instead. See
			// TestAgentDefTool_DescendantsScopeIsCurrentlyEquivalentToAny
			// for the pinned-but-undesired current behaviour.
			return nil
		default:
			if strings.HasPrefix(sc, "named:") {
				if strings.TrimPrefix(sc, "named:") == name {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("AgentDef tool: name %q not in this agent's agent_def_scopes (%v)", name, policy.Scopes)
}

// buildDefinition takes the base definition (parent's JSON for fork;
// the static cfg.Agents entry for create; empty for create-of-new-
// name), applies the overlay, and returns the merged AgentDef.
//
// Immutable + server-set fields (def_id, version, parent_def_id,
// created_*, bootstrapped_from_static) are SILENTLY DROPPED from the
// overlay — the model can supply them; we ignore. The tool layer
// stamps them server-side.
//
// Returns a config.AgentDef shape but as a generic JSON-decodable
// map so the store layer doesn't need the config import.
func (a *AgentDef) buildDefinition(ctx context.Context, name, parentJSON string, overlay json.RawMessage) (mergedDef, error) {
	base := mergedDef{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mergedDef{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if static, ok := a.Cfg.Agents[name]; ok {
		// Create-with-static-name path is REFUSED in execCreate; this
		// branch handles fork's bootstrap-from-static when there's no
		// parent JSON yet but a static entry exists.
		base = staticToMergedDef(static)
	}

	if len(overlay) > 0 {
		var ov mergedDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	return base, nil
}

// resolveToolsRoot returns the operator-blessed Tools
// ceiling for a name + parent chain. For names with a static MD,
// that's cfg.Agents[name].Tools — the operator's permanent
// authority. For names that exist only in DB (created via `create`),
// it's the v1 row's Tools (the root of the DB-only lineage).
func (a *AgentDef) resolveToolsRoot(ctx context.Context, name string, parent store.AgentDefRow) ([]string, error) {
	if static, ok := a.Cfg.Agents[name]; ok {
		return static.Tools, nil
	}
	// Walk parent chain to the v1 row. Hard cap at 100 hops as a
	// defense against cyclic / corrupt lineage. If we exhaust the cap
	// without reaching the root (ParentDefID still non-empty), refuse
	// rather than treat the mid-chain row as the ceiling — using a
	// non-root row would silently weaken the Tools security
	// invariant for sufficiently deep or cyclic chains.
	cur := parent
	const maxHops = 100
	reachedRoot := false
	for i := 0; i < maxHops; i++ {
		if cur.ParentDefID == "" {
			reachedRoot = true
			break
		}
		next, err := a.Store.AgentDefGet(ctx, cur.ParentDefID)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	if !reachedRoot {
		return nil, fmt.Errorf("lineage depth exceeds %d hops — possible cycle or corrupt chain for name %q (last def_id walked: %q)", maxHops, name, cur.DefID)
	}
	var rootDef mergedDef
	if err := json.Unmarshal(cur.Definition, &rootDef); err != nil {
		return nil, fmt.Errorf("parse root definition: %w", err)
	}
	return rootDef.Tools, nil
}

// assertToolsSubset returns nil iff every tool in `proposed`
// also appears in `root`. Empty proposed = empty subset = OK (narrowing
// to zero tools is allowed). Empty root = no permitted tools — proposed
// must also be empty.
//
// Wildcard: a root containing "*" accepts any proposed list. Used by
// the substrate-admin HTTP context (substrateAdminCtx) where the
// operator's bearer-auth is the security boundary, not a per-agent
// tools ceiling. Lineage roots (the v1 row's actual tool
// list) never contain "*" — they're real tool names persisted by an
// earlier `create`, so the fork narrowing check at lines 305 / 328
// is unaffected.
func assertToolsSubset(proposed, root []string) error {
	for _, t := range root {
		if t == "*" {
			return nil
		}
	}
	rootSet := make(map[string]bool, len(root))
	for _, t := range root {
		rootSet[t] = true
	}
	for _, t := range proposed {
		if !rootSet[t] {
			return fmt.Errorf("Tools cannot widen — %q is not in the operator-blessed root %v", t, root)
		}
	}
	return nil
}

// bootstrapStatic snapshots the cfg.Agents[name] static MD into a v1
// DB row with bootstrapped_from_static=TRUE. Called by fork when no
// parent exists yet but the name has a static entry. The snapshot is
// the immortal lineage root.
func (a *AgentDef) bootstrapStatic(ctx context.Context, name string, static config.AgentDef) (store.AgentDefRow, error) {
	def := staticToMergedDef(static)
	def.normalize()
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.AgentDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.AgentDefRow{
		DefID:                  mintDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static cfg.Agents",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
		ContentSHA256:          signFromMergedDef(name, def),
		// RFC N: the bootstrapped lineage root lives in the forking
		// caller's tenant (static cfg.Agents is the shared base; the
		// fork that triggers bootstrap is per-tenant). "" = shared.
		TenantID: ident.TenantID,
	}
	created, err := a.Store.AgentDefCreate(ctx, row)
	if err != nil {
		return store.AgentDefRow{}, err
	}
	return created, nil
}

// mergedDef is the JSON shape stored in agent_defs.definition. It
// mirrors the mutable subset of config.AgentDef — keeps the DB
// layer config-agnostic.
type mergedDef struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Code is the inline code-js orchestrator source (RFC J). Mirrors
	// config.AgentDef.Code; persisted in agent_defs.definition so a code
	// agent ingests through the substrate with no host filesystem bind.
	// "" = the provider falls back to agent_code/<name>/index.js. Gated
	// by LOOMCYCLE_CODE_AGENTS_ENABLED at create/fork (see execCreate).
	Code      string `json:"code_body,omitempty"`
	Tier      string `json:"tier,omitempty"`
	Effort    string `json:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// Sampling: per-agent LLM sampling params. Round-trips as the `sampling`
	// object; applyOverlay merges it PER FIELD (a fork that sets only
	// temperature keeps the parent's top_p). Content-identifying (hashed).
	Sampling *config.Sampling `json:"sampling,omitempty"`
	// Compaction: per-agent context-compaction settings. Same PER-FIELD overlay +
	// content-identifying treatment as Sampling.
	Compaction *config.Compaction `json:"compaction,omitempty"`
	// MaxIterations caps the loop at N provider calls before
	// terminating with stop_reason="max_iterations". 0 = use the
	// loop default (16). Set higher for discovery-style forks
	// whose workflow is intrinsically iterative — same knob the
	// yaml frontmatter exposes via PR #168's `max_iterations` field.
	MaxIterations int `json:"max_iterations,omitempty"`
	// UnboundedIterations lifts the MaxIterations soft-cap for an LLM agent
	// (interactive / terminal runs; the 1<<20 backstop still applies).
	// Mirrors config.AgentDef.UnboundedIterations.
	UnboundedIterations bool `json:"unbounded_iterations,omitempty"`
	// MaxConcurrentChildren caps how many sub-agents this agent may
	// spawn in parallel via Agent.parallel_spawn (v0.11.8+). Zero =
	// use the runtime default (DefaultMaxConcurrentChildren = 4).
	// Sequential Agent.spawn calls are unaffected; the cap only
	// applies to a single parallel_spawn op's `spawns` array.
	MaxConcurrentChildren int `json:"max_concurrent_children,omitempty"`
	// RunTimeoutSeconds is the per-agent code-js wall-clock budget (RFC J),
	// overriding the global default. 0 = global. See config.AgentDef.
	RunTimeoutSeconds int    `json:"run_timeout_seconds,omitempty"`
	SystemPrompt      string `json:"system_prompt,omitempty"`
	// SystemPromptBase is the pre-skill-bake snapshot of SystemPrompt.
	// Persisted alongside SystemPrompt so the v0.8.22 SkillDef per-run
	// resolver (`resolveSkillBodiesForRun` in api/http/server.go) can
	// rebuild the effective prompt from this base + each skill body
	// when DB-active SkillDef rows shadow the static body.
	//
	// For statically-yaml-defined agents, `resolveSkills` sets this at
	// config-load. For agents persisted via `AgentDef.create` /
	// `AgentDef.fork`, `normalize()` below copies SystemPrompt into it
	// when not explicitly supplied. The read-side
	// `lookup.NormalizeAgentDef` is defense-in-depth for legacy rows
	// that pre-date this field. See PR #186 for the production bug.
	SystemPromptBase string                            `json:"system_prompt_base,omitempty"`
	Tools            []string                          `json:"tools,omitempty"`
	Skills           []string                          `json:"skills,omitempty"`
	Providers        []string                          `json:"providers,omitempty"`
	Models           map[string][]config.TierCandidate `json:"models,omitempty"`
	MemoryScopes     []string                          `json:"memory_scopes,omitempty"`
	MemoryQuotaBytes int                               `json:"memory_quota_bytes,omitempty"`
	// MemoryBackend mirrors config.AgentDef.MemoryBackend — the named
	// memory backend this agent routes through. "" = operator default.
	// RFC I MR-3b.
	MemoryBackend string `json:"memory_backend,omitempty"`
	Description   string `json:"description,omitempty"`
	// RetryAttempts mirrors config.AgentDef.RetryAttempts — same-
	// provider retry budget override. *int so the substrate-write
	// path can persist an explicit 0 ("force no retries") as
	// distinct from "field not set" (use the tier default).
	// Without the pointer, AgentDef.create/fork on a high-stakes
	// agent would silently strip the static yaml's "force 0".
	RetryAttempts *int `json:"retry_attempts,omitempty"`
	// Channels / EvaluationScopes / Interruption mirror config.AgentDef so an
	// agent authored over MCP/HTTP (agentdef create/fork) can be a COMPLETE
	// interactive/multi-agent agent, not just tool-bearing (F14). Kept in sync
	// with lookup.SubstrateAgentDef (the drift test pins it).
	Channels         config.AgentChannelACL      `json:"channels,omitempty"`
	EvaluationScopes []string                    `json:"evaluation_scopes,omitempty"`
	Interruption     config.AgentInterruptionACL `json:"interruption,omitempty"`
	// The *_def_scopes capability gates (F40) — the substrate-def slice of the
	// F14 closure. Without these a runtime-authored meta-agent (one that
	// forks/promotes/schedules other agents) comes back default-deny and can't
	// author anything. NOT part of content_sha256 (the *Scopes ACLs are
	// deliberately excluded from agents.AgentContent — authority, not content).
	// (The SkillDef def-scope gate was removed in RFC BA — skill authoring is
	// governed by the unified `skills:` pattern allowlist, not a def-scope gate.)
	AgentDefScopes         []string `json:"agent_def_scopes,omitempty"`
	ScheduleDefScopes      []string `json:"schedule_def_scopes,omitempty"`
	A2AServerCardDefScopes []string `json:"a2a_server_card_def_scopes,omitempty"`
	A2AAgentDefScopes      []string `json:"a2a_agent_def_scopes,omitempty"`
	// VolumeDefScopes is the RFC AH Phase 2a slice of the F40 closure —
	// without it a runtime-authored ensemble launcher that provisions
	// dynamic volumes comes back default-deny on reload. Same exclusion
	// from content_sha256 (authority, not content).
	VolumeDefScopes []string `json:"volume_def_scopes,omitempty"`
}

func (d *mergedDef) applyOverlay(ov mergedDef) {
	if ov.Provider != "" {
		d.Provider = ov.Provider
	}
	if ov.Model != "" {
		d.Model = ov.Model
	}
	if ov.Code != "" {
		d.Code = ov.Code
	}
	if ov.Tier != "" {
		d.Tier = ov.Tier
	}
	if ov.Effort != "" {
		d.Effort = ov.Effort
	}
	// Sampling merges PER FIELD: a fork that sets only temperature keeps the
	// parent's top_p (MergeSampling overlays non-nil fields onto the base).
	if !ov.Sampling.IsZero() {
		d.Sampling = config.MergeSampling(d.Sampling, ov.Sampling)
	}
	// Compaction merges PER FIELD, same as Sampling.
	if !ov.Compaction.IsZero() {
		d.Compaction = config.MergeCompaction(d.Compaction, ov.Compaction)
	}
	if ov.MaxTokens != 0 {
		d.MaxTokens = ov.MaxTokens
	}
	if ov.MaxIterations != 0 {
		d.MaxIterations = ov.MaxIterations
	}
	// Bool overlay: a fork can ENABLE unbounded iterations; omitting the
	// field inherits the parent's value (a fork can't flip it back to false —
	// author a new def for that, same limitation as other bool overlays).
	if ov.UnboundedIterations {
		d.UnboundedIterations = true
	}
	if ov.MaxConcurrentChildren != 0 {
		d.MaxConcurrentChildren = ov.MaxConcurrentChildren
	}
	if ov.RunTimeoutSeconds != 0 {
		d.RunTimeoutSeconds = ov.RunTimeoutSeconds
	}
	if ov.SystemPrompt != "" {
		d.SystemPrompt = ov.SystemPrompt
	}
	if ov.SystemPromptBase != "" {
		d.SystemPromptBase = ov.SystemPromptBase
	}
	if ov.Tools != nil {
		d.Tools = ov.Tools
	}
	if ov.Skills != nil {
		d.Skills = ov.Skills
	}
	if ov.Providers != nil {
		d.Providers = ov.Providers
	}
	if ov.Models != nil {
		d.Models = ov.Models
	}
	if ov.MemoryScopes != nil {
		d.MemoryScopes = ov.MemoryScopes
	}
	if ov.MemoryQuotaBytes != 0 {
		d.MemoryQuotaBytes = ov.MemoryQuotaBytes
	}
	if ov.MemoryBackend != "" {
		d.MemoryBackend = ov.MemoryBackend
	}
	if ov.Description != "" {
		d.Description = ov.Description
	}
	if ov.RetryAttempts != nil {
		d.RetryAttempts = ov.RetryAttempts
	}
	// Channel ACL: a non-nil publish or subscribe list signals "set".
	if ov.Channels.Publish != nil || ov.Channels.Subscribe != nil {
		d.Channels = ov.Channels
	}
	if ov.EvaluationScopes != nil {
		d.EvaluationScopes = ov.EvaluationScopes
	}
	// Interruption: any non-zero field signals the operator set the block
	// (Enabled true, or kinds/max_pending supplied). A fork that wants to
	// DISABLE an inherited interruption block is an edge the create path
	// doesn't need — overlays build up, they don't blank.
	if ov.Interruption.Enabled || len(ov.Interruption.Kinds) > 0 || ov.Interruption.MaxPending != 0 {
		d.Interruption = ov.Interruption
	}
	// *_def_scopes capability gates (F40): slice-set-if-supplied, same idiom as
	// EvaluationScopes — overlays build up, they don't blank.
	if ov.AgentDefScopes != nil {
		d.AgentDefScopes = ov.AgentDefScopes
	}
	if ov.ScheduleDefScopes != nil {
		d.ScheduleDefScopes = ov.ScheduleDefScopes
	}
	if ov.A2AServerCardDefScopes != nil {
		d.A2AServerCardDefScopes = ov.A2AServerCardDefScopes
	}
	if ov.A2AAgentDefScopes != nil {
		d.A2AAgentDefScopes = ov.A2AAgentDefScopes
	}
	if ov.VolumeDefScopes != nil {
		d.VolumeDefScopes = ov.VolumeDefScopes
	}
}

// normalize fills derived fields that the static config-load path
// applies via `resolveSkills` but the substrate write path would
// otherwise skip. Called at every write site (execCreate / execFork /
// bootstrapStatic) right before json.Marshal so persisted rows match
// what `cfg.Agents[name]` would have looked like after boot
// normalization for the same content.
//
// Today the only field this fills is SystemPromptBase — see the field
// doc on the struct. Future static-path normalizers added to
// resolveSkills should grow a sibling assignment here.
//
// The read-side `lookup.NormalizeAgentDef` does the same work
// defensively for legacy rows that pre-date this method; together,
// the read + write normalizers form belt-and-suspenders against the
// drift PR #186 fixed.
func (d *mergedDef) normalize() {
	if d.SystemPromptBase == "" {
		d.SystemPromptBase = d.SystemPrompt
	}
}

func staticToMergedDef(s config.AgentDef) mergedDef {
	return mergedDef{
		Provider:              s.Provider,
		Model:                 s.Model,
		Code:                  s.Code,
		Tier:                  s.Tier,
		Effort:                s.Effort,
		Sampling:              s.Sampling.Clone(),
		Compaction:            s.Compaction.Clone(),
		MaxTokens:             s.MaxTokens,
		MaxIterations:         s.MaxIterations,
		UnboundedIterations:   s.UnboundedIterations,
		MaxConcurrentChildren: s.MaxConcurrentChildren,
		RunTimeoutSeconds:     s.RunTimeoutSeconds,
		SystemPrompt:          s.SystemPrompt,
		SystemPromptBase:      s.SystemPromptBase,
		Tools:                 s.Tools,
		Skills:                s.Skills,
		Providers:             s.Providers,
		Models:                s.Models,
		MemoryScopes:          s.MemoryScopes,
		MemoryQuotaBytes:      s.MemoryQuotaBytes,
		MemoryBackend:         s.MemoryBackend,
		RetryAttempts:         s.RetryAttempts,
		// F14: a static agent bootstrapped into the substrate keeps its
		// interactive/multi-agent config (and so its content hash matches a
		// hand-authored fork of the same shape). Same config types, direct copy.
		Channels:         s.Channels,
		EvaluationScopes: s.EvaluationScopes,
		Interruption:     s.Interruption,
		// F40: a static meta-agent bootstrapped into the substrate keeps its
		// capability gates, so a fork of it inherits them.
		AgentDefScopes:         s.AgentDefScopes,
		ScheduleDefScopes:      s.ScheduleDefScopes,
		A2AServerCardDefScopes: s.A2AServerCardDefScopes,
		A2AAgentDefScopes:      s.A2AAgentDefScopes,
		VolumeDefScopes:        s.VolumeDefScopes,
	}
}

// rowResponse + rowResponseMap shape the tool's reply envelope. Both
// use the same fields so consumers can parse with one shape.
func rowResponse(row store.AgentDefRow, promoted bool) map[string]any {
	m := rowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func rowResponseMap(row store.AgentDefRow) map[string]any {
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
		// v0.9.x Library v2: include the persisted definition body so
		// the UI's inline-content view + side panel can render it
		// without a second round-trip. json.RawMessage embeds verbatim;
		// nil renders as `null` which the renderer treats as empty.
		"definition": row.Definition,
	}
}

// signFromMergedDef computes the v0.9.x content_sha256 for a row
// whose Definition shape is the substrate's mergedDef. Maps the
// content-bearing subset onto agents.AgentContent (same name +
// every mutable field) and delegates to agents.Sign. The mapping
// is explicit (vs marshal+unmarshal) so future field additions on
// mergedDef get caught at compile time when they're added here.
func signFromMergedDef(name string, def mergedDef) string {
	// Convert config.TierCandidate map to agents.TierCandidate map so
	// the agents package stays free of any config import.
	var models map[string][]agents.TierCandidate
	if len(def.Models) > 0 {
		models = make(map[string][]agents.TierCandidate, len(def.Models))
		for k, v := range def.Models {
			out := make([]agents.TierCandidate, 0, len(v))
			for _, tc := range v {
				out = append(out, agents.TierCandidate{Provider: tc.Provider, Model: tc.Model})
			}
			models[k] = out
		}
	}
	c := agents.AgentContent{
		Name:                  name,
		Description:           def.Description,
		Provider:              def.Provider,
		Model:                 def.Model,
		CodeBody:              def.Code,
		Tier:                  def.Tier,
		Effort:                def.Effort,
		MaxTokens:             def.MaxTokens,
		MaxIterations:         def.MaxIterations,
		UnboundedIterations:   def.UnboundedIterations,
		MaxConcurrentChildren: def.MaxConcurrentChildren,
		Tools:                 def.Tools,
		// RFC BA: skills: is the agent's pattern-allowlist ACL (authority, not
		// content) — excluded from content_sha256 like the *_def_scopes gates.
		SystemPrompt:     def.SystemPrompt,
		Providers:        def.Providers,
		Models:           models,
		MemoryScopes:     def.MemoryScopes,
		MemoryQuotaBytes: def.MemoryQuotaBytes,
		MemoryBackend:    def.MemoryBackend,
		EvaluationScopes: def.EvaluationScopes,
	}
	// F14: include the interactive/multi-agent ACLs in the hash so a fork
	// that changes ONLY channels/interruption/evaluation_scopes mints a new
	// version instead of being deduped as identical content. Mirror
	// config→agents (the agents package stays config-free); nil when empty
	// so a no-ACL agent hashes exactly as pre-F14 (normalize() also
	// collapses defensively).
	if len(def.Channels.Publish) > 0 || len(def.Channels.Subscribe) > 0 {
		c.Channels = &agents.AgentChannelACL{Publish: def.Channels.Publish, Subscribe: def.Channels.Subscribe}
	}
	if def.Interruption.Enabled || len(def.Interruption.Kinds) > 0 || def.Interruption.MaxPending != 0 {
		c.Interruption = &agents.AgentInterruptionACL{Enabled: def.Interruption.Enabled, Kinds: def.Interruption.Kinds, MaxPending: def.Interruption.MaxPending}
	}
	// Sampling is content-identifying: a fork that only changes temperature
	// must mint a new content_sha256. Map config→agents (config-free agents pkg);
	// nil/empty omits so a no-sampling agent hashes exactly as pre-feature.
	if s := def.Sampling; !s.IsZero() {
		c.Sampling = &agents.Sampling{
			Temperature:      s.Temperature,
			TopP:             s.TopP,
			TopK:             s.TopK,
			FrequencyPenalty: s.FrequencyPenalty,
			PresencePenalty:  s.PresencePenalty,
			Seed:             s.Seed,
			Stop:             s.Stop,
		}
	}
	// Compaction is content-identifying, same as Sampling.
	if cp := def.Compaction; !cp.IsZero() {
		c.Compaction = &agents.Compaction{
			Enabled:          cp.Enabled,
			TargetPercentage: cp.TargetPercentage,
			KeepLastN:        cp.KeepLastN,
			KeepFirst:        cp.KeepFirst,
			AutoCompactAtPct: cp.AutoCompactAtPct,
			Model:            cp.Model,
		}
	}
	return agents.Sign(c)
}

// validateInlineCode gates + validates an inline code-js body (RFC J)
// on create/fork. No-op when the def carries no Code. When it does:
//
//   - GATE: refuse unless LOOMCYCLE_CODE_AGENTS_ENABLED — the single
//     switch that also registers the provider. Persisting a body the
//     runtime can't execute would be a silent footgun; refuse loudly.
//   - SIZE: enforce MaxCodeBytes (a tighter, clearer cap than the
//     whole-definition MaxDefinitionBytes).
//   - PARSE: compile via the shared codejs.Validate so a syntax error
//     is rejected at authorship — mirrors boot validateCodeAgents,
//     using the provider's exact compile flags (single source of truth).
func (a *AgentDef) validateInlineCode(op string, def mergedDef) error {
	if def.Code == "" {
		return nil
	}
	if a.Cfg == nil || !a.Cfg.Env.CodeAgentsEnabled {
		return fmt.Errorf("%s: inline code_body refused — code agents are disabled (set LOOMCYCLE_CODE_AGENTS_ENABLED=1)", op)
	}
	if a.MaxCodeBytes > 0 && len(def.Code) > a.MaxCodeBytes {
		return fmt.Errorf("%s: code_body (%d bytes) exceeds max %d", op, len(def.Code), a.MaxCodeBytes)
	}
	if _, err := codejs.Validate(def.Code); err != nil {
		return fmt.Errorf("%s: code_body does not compile: %s", op, err)
	}
	return nil
}

// mintDefID returns a fresh opaque ID for a new row. 16 hex chars
// = 64 bits entropy — matches the existing newID pattern in
// internal/store/sqlite/sqlite.go. Format: "def_<hex>".
func mintDefID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "def_" + hex.EncodeToString(b[:])
}
