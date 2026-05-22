package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

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
// AllowedTools enforcement: forks may NARROW but NEVER widen. The
// operator-blessed root (static cfg.Agents[name].AllowedTools if it
// exists, else the v1 row's AllowedTools) is the permanent capability
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
	// operator-blessed root (cfg.Agents[name]) for AllowedTools
	// ceiling enforcement and to refuse `create` over a static name.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON
	// (LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES). 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field
	// (LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES). 0 = no cap.
	MaxDescriptionBytes int
}

const agentDefDescription = `Author, fork, promote, retire, and inspect agent definitions at runtime. ` +
	`Static <name>.md files remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of agent-authored versions. AllowedTools may be NARROWED ` +
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
      "description": "Mutable subset of AgentDef for create/fork. Immutable / server-set fields (def_id, version, parent_def_id, created_*, bootstrapped_from_static) are silently ignored if supplied.",
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
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, promote)", in.Op)), nil
	}
}

// ---- create ----

func (a *AgentDef) execCreate(ctx context.Context, policy tools.AgentDefPolicyValue, in agentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
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
	// AllowedTools ceiling on `create`: the caller's own effective
	// allowed_tools is the ceiling for any new agent it mints. Without
	// this check an agent with narrow allowed_tools could call
	// `create` with overlay.allowed_tools = [the entire universe] and
	// then spawn the resulting agent — a capability-escalation path.
	// Mirror of the subset check in `fork`.
	//
	// AgentTools(ctx) returns nil in test contexts; we refuse to
	// create with a non-empty AllowedTools overlay when the ceiling
	// is unknown rather than silently allowing widening.
	callerTools := tools.AgentTools(ctx)
	if len(def.AllowedTools) > 0 {
		if callerTools == nil {
			return errResult("create: caller's effective allowed_tools not on ctx (runtime misconfiguration); refuse rather than risk silent widening"), nil
		}
		if err := assertAllowedToolsSubset(def.AllowedTools, callerTools); err != nil {
			return errResult(fmt.Sprintf("create: %s", err)), nil
		}
	}
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
	row := store.AgentDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromMergedDef(in.Name, def),
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
		if err := a.Store.AgentDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
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

	// Resolve the parent. Three paths:
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + active pointer exists → use it
	//   3. neither → name must have a static MD; bootstrap v1
	//      snapshot as the parent
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
		parent = row
	} else {
		// Try the active pointer first.
		row, err := a.Store.AgentDefGetActive(ctx, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No active pointer → must bootstrap from static MD.
			static, ok := a.Cfg.Agents[in.Name]
			if !ok {
				return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version nor a static cfg.Agents entry", in.Name)), nil
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
				if row2, gerr := a.Store.AgentDefGetActive(ctx, in.Name); gerr == nil {
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

	if err := a.checkScopeForName(policy, in.Name, parentDefID); err != nil {
		return errResult(err.Error()), nil
	}

	def, err := a.buildDefinition(ctx, in.Name, string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// AllowedTools ceiling enforcement — fork may narrow, never widen.
	root, err := a.resolveAllowedToolsRoot(ctx, in.Name, parent)
	if err != nil {
		return errResult(fmt.Sprintf("fork: resolve root: %s", err)), nil
	}
	if err := assertAllowedToolsSubset(def.AllowedTools, root); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

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

	ident := tools.RunIdentity(ctx)
	row := store.AgentDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		ContentSHA256:    signFromMergedDef(in.Name, def),
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
		if err := a.Store.AgentDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
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
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
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
	if err := a.Store.AgentDefSetActive(ctx, row.Name, row.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("promote: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": row.DefID, "name": row.Name, "promoted": true})
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

// resolveAllowedToolsRoot returns the operator-blessed AllowedTools
// ceiling for a name + parent chain. For names with a static MD,
// that's cfg.Agents[name].AllowedTools — the operator's permanent
// authority. For names that exist only in DB (created via `create`),
// it's the v1 row's AllowedTools (the root of the DB-only lineage).
func (a *AgentDef) resolveAllowedToolsRoot(ctx context.Context, name string, parent store.AgentDefRow) ([]string, error) {
	if static, ok := a.Cfg.Agents[name]; ok {
		return static.AllowedTools, nil
	}
	// Walk parent chain to the v1 row. Hard cap at 100 hops as a
	// defense against cyclic / corrupt lineage. If we exhaust the cap
	// without reaching the root (ParentDefID still non-empty), refuse
	// rather than treat the mid-chain row as the ceiling — using a
	// non-root row would silently weaken the AllowedTools security
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
	return rootDef.AllowedTools, nil
}

// assertAllowedToolsSubset returns nil iff every tool in `proposed`
// also appears in `root`. Empty proposed = empty subset = OK (narrowing
// to zero tools is allowed). Empty root = no permitted tools — proposed
// must also be empty.
func assertAllowedToolsSubset(proposed, root []string) error {
	rootSet := make(map[string]bool, len(root))
	for _, t := range root {
		rootSet[t] = true
	}
	for _, t := range proposed {
		if !rootSet[t] {
			return fmt.Errorf("AllowedTools cannot widen — %q is not in the operator-blessed root %v", t, root)
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
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Tier      string `json:"tier,omitempty"`
	Effort    string `json:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// MaxIterations caps the loop at N provider calls before
	// terminating with stop_reason="max_iterations". 0 = use the
	// loop default (16). Set higher for discovery-style forks
	// whose workflow is intrinsically iterative — same knob the
	// yaml frontmatter exposes via PR #168's `max_iterations` field.
	MaxIterations    int                               `json:"max_iterations,omitempty"`
	SystemPrompt     string                            `json:"system_prompt,omitempty"`
	AllowedTools     []string                          `json:"allowed_tools,omitempty"`
	Skills           []string                          `json:"skills,omitempty"`
	Providers        []string                          `json:"providers,omitempty"`
	Models           map[string][]config.TierCandidate `json:"models,omitempty"`
	MemoryScopes     []string                          `json:"memory_scopes,omitempty"`
	MemoryQuotaBytes int                               `json:"memory_quota_bytes,omitempty"`
	Description      string                            `json:"description,omitempty"`
}

func (d *mergedDef) applyOverlay(ov mergedDef) {
	if ov.Provider != "" {
		d.Provider = ov.Provider
	}
	if ov.Model != "" {
		d.Model = ov.Model
	}
	if ov.Tier != "" {
		d.Tier = ov.Tier
	}
	if ov.Effort != "" {
		d.Effort = ov.Effort
	}
	if ov.MaxTokens != 0 {
		d.MaxTokens = ov.MaxTokens
	}
	if ov.MaxIterations != 0 {
		d.MaxIterations = ov.MaxIterations
	}
	if ov.SystemPrompt != "" {
		d.SystemPrompt = ov.SystemPrompt
	}
	if ov.AllowedTools != nil {
		d.AllowedTools = ov.AllowedTools
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
	if ov.Description != "" {
		d.Description = ov.Description
	}
}

func staticToMergedDef(s config.AgentDef) mergedDef {
	return mergedDef{
		Provider:         s.Provider,
		Model:            s.Model,
		Tier:             s.Tier,
		Effort:           s.Effort,
		MaxTokens:        s.MaxTokens,
		MaxIterations:    s.MaxIterations,
		SystemPrompt:     s.SystemPrompt,
		AllowedTools:     s.AllowedTools,
		Skills:           s.Skills,
		Providers:        s.Providers,
		Models:           s.Models,
		MemoryScopes:     s.MemoryScopes,
		MemoryQuotaBytes: s.MemoryQuotaBytes,
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
	return agents.Sign(agents.AgentContent{
		Name:             name,
		Description:      def.Description,
		Provider:         def.Provider,
		Model:            def.Model,
		Tier:             def.Tier,
		Effort:           def.Effort,
		MaxTokens:        def.MaxTokens,
		MaxIterations:    def.MaxIterations,
		AllowedTools:     def.AllowedTools,
		Skills:           def.Skills,
		SystemPrompt:     def.SystemPrompt,
		Providers:        def.Providers,
		Models:           models,
		MemoryScopes:     def.MemoryScopes,
		MemoryQuotaBytes: def.MemoryQuotaBytes,
	})
}

// mintDefID returns a fresh opaque ID for a new row. 16 hex chars
// = 64 bits entropy — matches the existing newID pattern in
// internal/store/sqlite/sqlite.go. Format: "def_<hex>".
func mintDefID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "def_" + hex.EncodeToString(b[:])
}
