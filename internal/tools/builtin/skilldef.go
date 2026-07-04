package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SkillDef is the v0.8.22 built-in tool that lets agents author,
// fork, promote, retire, and inspect SKILL definitions at runtime.
// Mirror of the AgentDef tool — same six operations, same lineage
// model, same append-only storage. Static SKILL.md files (loaded
// at boot from LOOMCYCLE_SKILLS_ROOT into a skills.Set) remain the
// operator-blessed root; this tool produces the DERIVED layer of
// agent-authored versions.
//
// Six operations dispatched off the `op` field:
//
//	create   — declare a brand-new skill name with a v1 definition.
//	           Refused if `name` matches an existing static skill in
//	           the loaded skills.Set (operators expect their MDs to be
//	           ground truth; use `fork` to derive a new version).
//	fork     — make a new version from an existing parent (DB-resolved
//	           by def_id, or by-name from the active pointer, or
//	           bootstrapped from the static SKILL.md if neither
//	           exists). Default does NOT auto-promote.
//	get      — fetch one row by def_id.
//	list     — list versions for a name, version DESC.
//	retire   — flip the retired flag. Row stays visible.
//	promote  — set the active pointer for a name to a specific def_id.
//
// Runtime consumption: when this skill name is in an agent's
// `skills:` list, the run-creation handler resolves SkillDefGetActive
// at session start (per-run, not per-config-load). Existing
// in-flight runs keep their locked system prompt — there is no
// mid-run skill body swap.
//
// Tools enforcement: forks may NARROW but NEVER widen. The
// operator-blessed root (static skill.Tools if it exists,
// else the v1 row's tools) is the permanent capability
// ceiling. Mirror of the AgentDef rule.
//
// Validation specific to skills:
//   - `body` is required on create/fork (empty / whitespace-only is
//     rejected; a zero-body skill is silent prompt corruption).
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity). The model NEVER supplies these.
type SkillDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Set is the static skill registry loaded at boot from
	// LOOMCYCLE_SKILLS_ROOT. Used for the static-name guard on
	// `create` and as the bootstrap source on `fork` when neither
	// a DB row nor an active pointer exists yet. Nil = no static
	// names exist (deployment without LOOMCYCLE_SKILLS_ROOT); the
	// tool still operates, just with no static-name guard or
	// bootstrap source.
	Set *skills.Set

	// MaxBodyBytes caps the overlay.body field
	// (LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES). 0 = no cap.
	MaxBodyBytes int

	// MaxDescriptionBytes caps the description field
	// (LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES). 0 = no cap.
	MaxDescriptionBytes int
}

const skillDefDescription = `Author, fork, promote, retire, and inspect skill definitions at runtime. ` +
	`Static SKILL.md files (LOOMCYCLE_SKILLS_ROOT) remain the operator's immutable ground truth; ` +
	`this tool produces the DERIVED layer of agent-authored versions. Tools may be NARROWED ` +
	`on forks but never WIDENED. Promotion is explicit — selection is policy, not runtime. ` +
	`Operations: create, fork, get, list, retire, promote.`

const skillDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","promote"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Skill name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire/promote)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, falling back to the static SKILL.md bootstrap)."},
    "overlay": {
      "type": "object",
      "description": "Skill content + metadata. Fields: body (required string for create/fork), description (string), tools (array). Server-set fields (def_id, version, parent_def_id, created_*, bootstrapped_from_static) are silently ignored if supplied.",
      "properties": {
        "body":          {"type": "string", "description": "Skill markdown body (required, non-empty)."},
        "description":   {"type": "string", "description": "Skill self-description for discovery."},
        "tools": {"type": "array", "items": {"type": "string"}, "description": "Tools this skill needs. Must be a subset of the calling agent's effective tools."}
      },
      "additionalProperties": false
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork (distinct from overlay.description, which is the skill's own self-description)."},
    "promote":       {"type": "boolean", "description": "create defaults true, fork defaults false."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
  },
  "required": ["op"]
}`

type skillDefInput struct {
	Op            string          `json:"op"`
	Name          string          `json:"name,omitempty"`
	DefID         string          `json:"def_id,omitempty"`
	ParentDefID   string          `json:"parent_def_id,omitempty"`
	Overlay       json.RawMessage `json:"overlay,omitempty"`
	Description   string          `json:"description,omitempty"`
	Promote       *bool           `json:"promote,omitempty"`
	Retired       *bool           `json:"retired,omitempty"`
	ContentSHA256 string          `json:"content_sha256,omitempty"` // input for op: verify
}

// skillDefOverlay is the JSON shape of overlay + the persisted
// `definition` column for skill_defs rows.
type skillDefOverlay struct {
	Body        string   `json:"body,omitempty"`
	Description string   `json:"description,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

func (d *skillDefOverlay) applyOverlay(ov skillDefOverlay) {
	if ov.Body != "" {
		d.Body = ov.Body
	}
	if ov.Description != "" {
		d.Description = ov.Description
	}
	if ov.Tools != nil {
		d.Tools = ov.Tools
	}
}

// Name implements tools.Tool.
func (s *SkillDef) Name() string { return "SkillDef" }

// Description implements tools.Tool.
func (s *SkillDef) Description() string { return skillDefDescription }

// InputSchema implements tools.Tool.
func (s *SkillDef) InputSchema() json.RawMessage { return json.RawMessage(skillDefInputSchema) }

// Execute implements tools.Tool.
func (s *SkillDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("SkillDef tool: not configured (no Store backend)"), nil
	}
	var in skillDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.SkillDefPolicy(ctx)

	switch in.Op {
	case "create":
		return s.execCreate(ctx, policy, in)
	case "fork":
		return s.execFork(ctx, policy, in)
	case "get":
		return s.execGet(ctx, policy, in)
	case "list":
		return s.execList(ctx, policy, in)
	case "retire":
		return s.execRetire(ctx, policy, in)
	case "promote":
		return s.execPromote(ctx, policy, in)
	case "verify":
		return s.execVerify(ctx, policy, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, promote, verify)", in.Op)), nil
	}
}

// ---- create ----

func (s *SkillDef) execCreate(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name, ""); err != nil {
		return errResult(err.Error()), nil
	}
	// Static-name-replace refusal — operator-blessed SKILL.md is
	// ground truth. Use fork to derive a new version.
	if s.Set != nil {
		if _, ok := s.Set.Get(in.Name); ok {
			return errResult(fmt.Sprintf("create: name %q matches a static SKILL.md entry — use `fork` to derive a new version", in.Name)), nil
		}
	}

	def, err := s.buildDefinition(in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := s.validateBody(def.Body); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// Tools ceiling on `create`: caller's effective tools.
	callerTools := tools.AgentTools(ctx)
	if len(def.Tools) > 0 {
		if callerTools == nil {
			return errResult("create: caller's effective tools not on ctx (runtime misconfiguration); refuse rather than risk silent widening"), nil
		}
		if err := assertToolsSubset(def.Tools, callerTools); err != nil {
			return errResult(fmt.Sprintf("create: %s", err)), nil
		}
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	if err := s.checkSizeCaps(defJSON, def.Body, in.Description); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}

	ident := tools.RunIdentity(ctx)
	// RFC N: the tenant comes from the authoritative run identity in ctx
	// (the SkillDef tool always runs inside a run whose RunIdentity
	// carries the principal-derived tenant), never from tool input. ""
	// = shared/legacy tenant. Used for the row stamp + the promote — both
	// scoped to the skill's own tenant.
	tenantID := ident.TenantID
	row := store.SkillDefRow{
		DefID:            mintSkillDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromSkillDef(in.Name, def),
		TenantID:         tenantID,
	}
	created, err := s.Store.SkillDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.SkillDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(skillDefRowResponse(created, promote))
}

// ---- fork ----

func (s *SkillDef) execFork(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// Resolve the parent. Three paths, mirroring AgentDef:
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + active pointer exists → use it
	//   3. neither → name must have a static SKILL.md; bootstrap v1
	// RFC N: fork resolves + stamps within the skill's own tenant (from
	// the authoritative run identity, never tool input).
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID

	parentDefID := in.ParentDefID
	var parent store.SkillDefRow
	if parentDefID != "" {
		row, err := s.Store.SkillDefGet(ctx, parentDefID)
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
		// Allow forking the SHARED ("") base or the caller's own tenant (the
		// fork lands under the caller's tenant); refuse only another specific
		// tenant's private def, unless the caller is substrate:admin (crosses
		// tenants, RFC L). The "" allowance lets a legacy/default or tenant
		// principal migrate a pre-RFC-N / bootstrapped shared def.
		if row.TenantID != "" && row.TenantID != tenantID && !defCallerIsAdmin(ctx) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q belongs to another tenant, refusing", parentDefID)), nil
		}
		parent = row
	} else {
		row, err := s.Store.SkillDefGetActive(ctx, tenantID, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No own-tenant active pointer. Fall back to the SHARED ("")
			// base before bootstrapping — mirrors run-time lookup precedence
			// (own-tenant → static → shared "") and the explicit-parent
			// branch, so a per-tenant principal can fork a name seeded under
			// the legacy "" tenant. The fork lands under the caller's tenant.
			// Skip when tenantID is already "" (identical lookup).
			if tenantID != "" {
				if shared, serr := s.Store.SkillDefGetActive(ctx, "", in.Name); serr == nil {
					parent = shared
					parentDefID = shared.DefID
				} else if !errors.As(serr, &nf) {
					return errResult(fmt.Sprintf("fork: %s", serr)), nil
				}
			}
			if parentDefID == "" {
				// Still no parent (own-tenant AND shared "" missed) →
				// bootstrap from static SKILL.md, else refuse.
				if s.Set == nil {
					return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version (own tenant or shared \"\") nor a static SKILL.md entry (LOOMCYCLE_SKILLS_ROOT unset)", in.Name)), nil
				}
				static, ok := s.Set.Get(in.Name)
				if !ok {
					return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version (own tenant or shared \"\") nor a static SKILL.md entry", in.Name)), nil
				}
				bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
				if berr != nil {
					// Concurrent first-fork may have already bootstrapped
					// v1 between our GetActive and our own bootstrap insert.
					// Re-read active before propagating the error.
					if row2, gerr := s.Store.SkillDefGetActive(ctx, tenantID, in.Name); gerr == nil {
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

	if err := s.checkScopeForName(policy, in.Name, parentDefID); err != nil {
		return errResult(err.Error()), nil
	}

	def, err := s.buildDefinition(in.Name, string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := s.validateBody(def.Body); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// Tools ceiling — fork may narrow, never widen.
	root, err := s.resolveToolsRoot(ctx, in.Name, parent)
	if err != nil {
		return errResult(fmt.Sprintf("fork: resolve root: %s", err)), nil
	}
	if err := assertToolsSubset(def.Tools, root); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("fork: marshal: %s", err)), nil
	}
	if err := s.checkSizeCaps(defJSON, def.Body, in.Description); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

	row := store.SkillDefRow{
		DefID:            mintSkillDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromSkillDef(in.Name, def),
		TenantID:         tenantID,
	}
	created, err := s.Store.SkillDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := false
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.SkillDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(skillDefRowResponse(created, promote))
}

// ---- get / list ----

func (s *SkillDef) execGet(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.SkillDefGet(ctx, in.DefID)
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
	if err := s.checkScopeForName(policy, row.Name, row.DefID); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(skillDefRowResponse(row, false))
}

func (s *SkillDef) execList(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name, ""); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := s.Store.SkillDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	// RFC N: SkillDefListByName returns rows across ALL tenants for a name
	// (names are per-tenant now). Filter to the caller's own tenant so a
	// tenant lists only its own versions.
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, skillDefRowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire / promote ----

func (s *SkillDef) execRetire(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := s.Store.SkillDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// RFC N: refuse cross-tenant retire. SkillDefSetRetired is a global
	// by-def_id mutation; without this guard a caller in tenant T could
	// retire another tenant's def. Opaque not-found — don't leak existence.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
	}
	if err := s.checkScopeForName(policy, row.Name, row.DefID); err != nil {
		return errResult(err.Error()), nil
	}
	if err := s.Store.SkillDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

func (s *SkillDef) execPromote(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("promote: missing required field: def_id"), nil
	}
	row, err := s.Store.SkillDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("promote: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	if err := s.checkScopeForName(policy, row.Name, row.DefID); err != nil {
		return errResult(err.Error()), nil
	}
	ident := tools.RunIdentity(ctx)
	// RFC N: promote within the skill's own tenant. SkillDefSetActive
	// refuses when ident.TenantID ≠ row.TenantID, so a caller in tenant T
	// cannot point at (or clobber) another tenant's active pointer — even
	// though def_id is a global handle.
	if err := s.Store.SkillDefSetActive(ctx, ident.TenantID, row.Name, row.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
}

// execVerify — see agentdef.go execVerify for full doc. Same shape
// for skills: caller passes name + content_sha256 from a local
// hash, tool reads the active row + answers matches.
func (s *SkillDef) execVerify(ctx context.Context, policy tools.SkillDefPolicyValue, in skillDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("verify: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name, ""); err != nil {
		return errResult(err.Error()), nil
	}
	// RFC N: verify against the skill's own tenant active pointer.
	row, err := s.Store.SkillDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, in.Name)
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

// checkScopeForName enforces the agent's skill_def_scopes against a
// proposed (name, def_id) target. Default-deny when policy.Scopes
// is empty. Same shape as AgentDef.checkScopeForName minus the
// "self" branch.
func (s *SkillDef) checkScopeForName(policy tools.SkillDefPolicyValue, name, _ string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("SkillDef tool: agent has no skill_def_scopes (default-deny); add `skill_def_scopes: [...]` to the agent yaml")
	}
	for _, sc := range policy.Scopes {
		switch sc {
		case "any":
			return nil
		case "descendants":
			// KNOWN GAP (TODO v0.9.x): equivalent to "any" pending
			// lineage-walk implementation. Mirror of the AgentDef
			// caveat — same defer for the same reason.
			return nil
		default:
			if strings.HasPrefix(sc, "named:") {
				if strings.TrimPrefix(sc, "named:") == name {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("SkillDef tool: name %q not in this agent's skill_def_scopes (%v)", name, policy.Scopes)
}

// buildDefinition takes the base definition (parent's JSON for
// fork; the static Skill for fork-bootstrap; empty for create),
// applies the overlay, returns the merged shape.
func (s *SkillDef) buildDefinition(name, parentJSON string, overlay json.RawMessage) (skillDefOverlay, error) {
	base := skillDefOverlay{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return skillDefOverlay{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if s.Set != nil {
		if static, ok := s.Set.Get(name); ok {
			base = staticToSkillDefOverlay(static)
		}
	}
	if len(overlay) > 0 {
		var ov skillDefOverlay
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return skillDefOverlay{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	return base, nil
}

// validateBody refuses empty / whitespace-only bodies. A zero-body
// skill is silent prompt corruption — better to fail loud here.
func (s *SkillDef) validateBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("overlay.body is required and must contain non-whitespace content")
	}
	return nil
}

func (s *SkillDef) checkSizeCaps(defJSON []byte, body, description string) error {
	if s.MaxBodyBytes > 0 && len(body) > s.MaxBodyBytes {
		return fmt.Errorf("body (%d bytes) exceeds LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES (%d)", len(body), s.MaxBodyBytes)
	}
	if s.MaxDescriptionBytes > 0 && len(description) > s.MaxDescriptionBytes {
		return fmt.Errorf("description (%d bytes) exceeds LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES (%d)", len(description), s.MaxDescriptionBytes)
	}
	return nil
}

// resolveToolsRoot returns the operator-blessed Tools
// ceiling for a skill name. For names with a static SKILL.md,
// that's the static Tools. For DB-only lineages, it's the
// v1 row's tools.
func (s *SkillDef) resolveToolsRoot(ctx context.Context, name string, parent store.SkillDefRow) ([]string, error) {
	if s.Set != nil {
		if static, ok := s.Set.Get(name); ok {
			return static.Tools, nil
		}
	}
	cur := parent
	const maxHops = 100
	reachedRoot := false
	for i := 0; i < maxHops; i++ {
		if cur.ParentDefID == "" {
			reachedRoot = true
			break
		}
		next, err := s.Store.SkillDefGet(ctx, cur.ParentDefID)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	if !reachedRoot {
		return nil, fmt.Errorf("lineage depth exceeds %d hops — possible cycle or corrupt chain for name %q (last def_id walked: %q)", maxHops, name, cur.DefID)
	}
	var rootDef skillDefOverlay
	if err := json.Unmarshal(cur.Definition, &rootDef); err != nil {
		return nil, fmt.Errorf("parse root definition: %w", err)
	}
	return rootDef.Tools, nil
}

// bootstrapStatic snapshots the static skills.Set entry into a v1
// DB row with bootstrapped_from_static=TRUE. Called by fork when
// no DB parent exists yet but the name has a static SKILL.md.
func (s *SkillDef) bootstrapStatic(ctx context.Context, name string, static *skills.Skill) (store.SkillDefRow, error) {
	def := staticToSkillDefOverlay(static)
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.SkillDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.SkillDefRow{
		DefID:                  mintSkillDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static SKILL.md",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
		ContentSHA256:          signFromSkillDef(name, def),
		// RFC N: the bootstrapped lineage root lives in the forking
		// caller's tenant (static skills.Set is the shared base; the fork
		// that triggers bootstrap is per-tenant). "" = shared.
		TenantID: ident.TenantID,
	}
	return s.Store.SkillDefCreate(ctx, row)
}

func staticToSkillDefOverlay(sk *skills.Skill) skillDefOverlay {
	if sk == nil {
		return skillDefOverlay{}
	}
	return skillDefOverlay{
		Body:        sk.Body,
		Description: sk.Description,
		Tools:       sk.Tools,
	}
}

// skillDefRowResponse + Map shape the tool's reply envelope.
func skillDefRowResponse(row store.SkillDefRow, promoted bool) map[string]any {
	m := skillDefRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func skillDefRowResponseMap(row store.SkillDefRow) map[string]any {
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
		// v0.9.x Library v2: include the persisted body so the UI can
		// render skill content inline + in the side panel without a
		// second round-trip.
		"definition": row.Definition,
	}
}

// signFromSkillDef computes the v0.9.x content_sha256 from the
// substrate's skillDefOverlay shape. Same explicit-mapping pattern
// as signFromMergedDef in agentdef.go.
func signFromSkillDef(name string, def skillDefOverlay) string {
	return skills.Sign(skills.SkillContent{
		Name:        name,
		Description: def.Description,
		Body:        def.Body,
		Tools:       def.Tools,
	})
}

// mintSkillDefID returns a fresh opaque ID for a new row. Same
// 64-bit-entropy shape as mintDefID but with the "sdf_" prefix so
// skill defs never collide with agent defs in logs / grep output.
func mintSkillDefID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sdf_" + hex.EncodeToString(b[:])
}
