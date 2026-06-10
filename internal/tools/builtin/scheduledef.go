package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/robfig/cron/v3"
)

// ScheduleDef is the v1.x RFC E built-in tool that lets agents
// author, fork, retire, and inspect schedule definitions at runtime.
// Yaml `scheduled_runs.<name>:` entries remain the operator-blessed
// root; this tool produces the DERIVED layer of orchestrator-authored
// per-user forks.
//
// Five operations dispatched off the `op` field:
//
//	create  — declare a brand-new schedule name with a v1 definition.
//	          Refused if `name` matches a static cfg.ScheduledRuns
//	          entry (operators expect their yaml to be ground truth;
//	          use `fork` to derive a new version).
//	fork    — make a new version from an existing parent (by def_id,
//	          or by-name from the active pointer / yaml template).
//	          The JobEmber-primary op. Auto-promotes by default per
//	          RFC E's worked example (fork-with-new-credentials chain
//	          where v2 should immediately replace v1 as active).
//	get     — fetch one row by def_id.
//	list    — list versions for a name (version DESC). Drives
//	          JobEmber's "enumerate all forks of job-search-template"
//	          query.
//	retire  — flip the retired flag. Lineage stays visible; sweeper
//	          skips retired rows.
//
// No `promote` op (vs AgentDef): schedules' fork-auto-promote model
// makes a separate promote step unnecessary. No `verify` op: no
// content_sha256 surface in v1.x.
//
// No AllowedTools ceiling enforcement (vs AgentDef): schedules don't
// expose tools to agents, they spawn runs. The capability check is
// `schedule_def_scopes` (default-deny) — operators grant orchestrator
// agents the scopes they need.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity). The model NEVER supplies these.
type ScheduleDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the
	// operator-blessed root (cfg.ScheduledRuns[name]) for the
	// static-name-replace refusal and the bootstrap-from-yaml path.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON.
	// 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field.
	// 0 = no cap.
	MaxDescriptionBytes int
}

const scheduleDefDescription = `Author, fork, retire, and inspect schedule definitions at runtime. ` +
	`Static scheduled_runs.<name>: yaml entries remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of orchestrator-authored per-user forks. ` +
	`Operations: create, fork, get, list, retire, add_hook, remove_hook.`

const scheduleDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire","add_hook","remove_hook"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Schedule name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire; required for add_hook/remove_hook to identify the parent version to fork from)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of ScheduledRun for create/fork (agent, prompt, schedule/user_tier_schedules, timezone, enabled, catch_up_max, max_fires, user_id, user_tier, user_credentials, user_credentials_from_env, on_complete, metadata, tenant_id). max_fires N>0 auto-retires the def after its Nth fire (1 = one-shot; 0 = unbounded). Immutable / server-set fields are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create + fork both default true (schedules' fork-versioning model expects new versions to replace old). Pass false to leave the existing active pointer in place."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."},
    "hook": {
      "type": "object",
      "description": "Required for add_hook — the hook body. {kind: 'channel.publish'|'mcp.call'|'memory.set', + kind-specific fields (channel|server+tool|scope+key) + payload/args}.",
      "additionalProperties": true
    },
    "hook_index":    {"type": "integer", "description": "Required for remove_hook — 0-indexed position in the parent's on_complete list."}
  },
  "required": ["op"]
}`

type scheduleDefInput struct {
	Op          string          `json:"op"`
	Name        string          `json:"name,omitempty"`
	DefID       string          `json:"def_id,omitempty"`
	ParentDefID string          `json:"parent_def_id,omitempty"`
	Overlay     json.RawMessage `json:"overlay,omitempty"`
	Description string          `json:"description,omitempty"`
	Promote     *bool           `json:"promote,omitempty"`
	Retired     *bool           `json:"retired,omitempty"`
	// add_hook / remove_hook fields (v1.x).
	Hook      *mergedScheduleHook `json:"hook,omitempty"`
	HookIndex *int                `json:"hook_index,omitempty"`
}

// Name implements tools.Tool.
func (s *ScheduleDef) Name() string { return "ScheduleDef" }

// Description implements tools.Tool.
func (s *ScheduleDef) Description() string { return scheduleDefDescription }

// InputSchema implements tools.Tool.
func (s *ScheduleDef) InputSchema() json.RawMessage { return json.RawMessage(scheduleDefInputSchema) }

// Execute implements tools.Tool.
func (s *ScheduleDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("ScheduleDef tool: not configured (no Store backend)"), nil
	}
	if s.Cfg == nil {
		return errResult("ScheduleDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	var in scheduleDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.ScheduleDefPolicy(ctx)

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
	case "add_hook":
		return s.execAddHook(ctx, policy, in)
	case "remove_hook":
		return s.execRemoveHook(ctx, policy, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire, add_hook, remove_hook)", in.Op)), nil
	}
}

// ---- create ----

func (s *ScheduleDef) execCreate(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if _, ok := s.Cfg.ScheduledRuns[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.ScheduledRuns entry — use `fork` to derive a new version", in.Name)), nil
	}

	def, err := s.buildDefinition(in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := validateScheduleDef(def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	if s.MaxDefinitionBytes > 0 && len(defJSON) > s.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("create: definition (%d bytes) exceeds max %d", len(defJSON), s.MaxDefinitionBytes)), nil
	}
	if s.MaxDescriptionBytes > 0 && len(in.Description) > s.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("create: description (%d bytes) exceeds max %d", len(in.Description), s.MaxDescriptionBytes)), nil
	}

	// RFC N: stamp the def under the caller's authoritative tenant.
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID
	row := store.ScheduleDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		TenantID:         tenantID,
	}
	created, err := s.Store.ScheduleDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.ScheduleDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
		// Seed schedule_run_state so the sweeper's due-query JOIN
		// returns this def. Without this, ScheduleRunStateListDue
		// returns nothing for the def and the sweeper never fires
		// it — the def_id exists, the active pointer exists, but
		// the state row is missing.
		_ = s.Store.ScheduleRunStateSeed(ctx, created.DefID, computeInitialNextRunAt(def, time.Now()))
	}
	return okJSON(scheduleRowResponse(created, promote))
}

// ---- fork ----

func (s *ScheduleDef) execFork(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// RFC N: fork resolves the parent within the caller's own tenant first
	// (then the shared "" base); the new version is ALWAYS stamped under the
	// caller's own tenant.
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID

	// Resolve the parent. Paths (mirror AgentDef):
	//   1. parent_def_id supplied → pin (refuse another tenant's private def)
	//   2. parent_def_id empty + own-tenant active pointer → use it
	//   3. else shared "" active pointer (for non-"" tenants) → use it
	//   4. neither → name must have a yaml template; bootstrap v1
	parentDefID := in.ParentDefID
	var parent store.ScheduleDefRow
	if parentDefID != "" {
		row, err := s.Store.ScheduleDefGet(ctx, parentDefID)
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
		// Allow forking the shared "" base or the caller's own def; refuse
		// another tenant's private def unless substrate:admin.
		if row.TenantID != "" && row.TenantID != tenantID && !defCallerIsAdmin(ctx) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q belongs to another tenant, refusing", parentDefID)), nil
		}
		parent = row
	} else {
		row, err := s.Store.ScheduleDefGetActive(ctx, tenantID, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No own-tenant active pointer. Fall back to the SHARED ("") base
			// (same precedence lookup.Schedule walks); the fork still lands
			// under the caller's tenant. Skip when tenantID is already "".
			if tenantID != "" {
				if shared, serr := s.Store.ScheduleDefGetActive(ctx, "", in.Name); serr == nil {
					parent = shared
					parentDefID = shared.DefID
				} else if !errors.As(serr, &nf) {
					return errResult(fmt.Sprintf("fork: %s", serr)), nil
				}
			}
			// Still no parent → bootstrap from yaml, else refuse.
			if parentDefID == "" {
				static, ok := s.Cfg.ScheduledRuns[in.Name]
				if !ok {
					return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version (own tenant or shared \"\") nor a static cfg.ScheduledRuns entry", in.Name)), nil
				}
				bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
				if berr != nil {
					// Concurrent first-fork may have already bootstrapped v1;
					// re-read own-tenant active pointer before propagating.
					if row2, gerr := s.Store.ScheduleDefGetActive(ctx, tenantID, in.Name); gerr == nil {
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

	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}

	def, err := s.buildDefinition(in.Name, string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := validateScheduleDef(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// required_credentials check — the template's manifest must be
	// satisfied by the merged credentials map. Forks without all
	// required keys are loud-refused per the RFC E sharp edge.
	if err := assertRequiredCredentials(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}

	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("fork: marshal: %s", err)), nil
	}
	if s.MaxDefinitionBytes > 0 && len(defJSON) > s.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("fork: definition (%d bytes) exceeds max %d", len(defJSON), s.MaxDefinitionBytes)), nil
	}
	if s.MaxDescriptionBytes > 0 && len(in.Description) > s.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("fork: description (%d bytes) exceeds max %d", len(in.Description), s.MaxDescriptionBytes)), nil
	}

	row := store.ScheduleDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		TenantID:         tenantID,
	}
	created, err := s.Store.ScheduleDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	// Schedules' fork-auto-promote default: each new fork version
	// REPLACES the prior active version. Pass promote:false to leave
	// the existing pointer in place (rare; the model use-case is
	// "stage a fork, evaluate, decide whether to swap").
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.ScheduleDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
		// Seed schedule_run_state — see commentary in execCreate.
		_ = s.Store.ScheduleRunStateSeed(ctx, created.DefID, computeInitialNextRunAt(def, time.Now()))
	}
	return okJSON(scheduleRowResponse(created, promote))
}

// ---- get / list ----

func (s *ScheduleDef) execGet(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.ScheduleDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	// RFC N: refuse cross-tenant reads with the same opaque not-found a
	// missing def returns.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
	}
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(scheduleRowResponse(row, false))
}

func (s *ScheduleDef) execList(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := s.Store.ScheduleDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	// RFC N: filter to the caller's own tenant (names are per-tenant now);
	// a substrate:admin sees all.
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, scheduleRowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire ----

func (s *ScheduleDef) execRetire(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := s.Store.ScheduleDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// RFC N: refuse cross-tenant retire (global by-def_id mutation).
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
	}
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := s.Store.ScheduleDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

// ---- add_hook / remove_hook ----
//
// Both ops are syntactic sugar over fork: fetch the parent definition,
// mutate its on_complete list, persist as a NEW VERSION with full
// lineage + auto-promote. Operators could already do this manually via
// `op: fork` with a full on_complete overlay, but that requires fetch
// → JSON-merge → fork dance from the caller. These ops collapse the
// pattern into one round-trip and use a per-hook input shape so the
// model doesn't have to reconstruct the entire list every time.
//
// Validation invariants preserved (same as fork):
//   - validateScheduleDef on the merged def
//   - required_credentials manifest still enforced
//   - MaxDefinitionBytes still enforced
//   - default-deny scope still enforced (checkScopeForName)
//
// Versioning: each add/remove creates a new fork version (parent_def_id
// linked to the existing active def). The substrate's monotonic
// version counter advances. Lineage is preserved across hook edits.

func (s *ScheduleDef) execAddHook(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("add_hook: missing required field: def_id (target parent version)"), nil
	}
	if in.Hook == nil {
		return errResult("add_hook: missing required field: hook"), nil
	}
	parent, mergedDef, err := s.loadParentForHookOp(ctx, policy, "add_hook", in.DefID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Append the new hook then re-validate. Validation refuses unknown
	// kinds + missing kind-specific fields (channel|server+tool|scope+key)
	// — the existing validateScheduleDef walks every hook in the slice,
	// so the appended one gets checked along with any pre-existing ones.
	mergedDef.OnComplete = append(mergedDef.OnComplete, *in.Hook)
	return s.persistForkFromHookEdit(ctx, parent, mergedDef, "add_hook")
}

func (s *ScheduleDef) execRemoveHook(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("remove_hook: missing required field: def_id (target parent version)"), nil
	}
	if in.HookIndex == nil {
		return errResult("remove_hook: missing required field: hook_index"), nil
	}
	idx := *in.HookIndex
	parent, mergedDef, err := s.loadParentForHookOp(ctx, policy, "remove_hook", in.DefID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if idx < 0 || idx >= len(mergedDef.OnComplete) {
		return errResult(fmt.Sprintf("remove_hook: hook_index %d out of range [0..%d)", idx, len(mergedDef.OnComplete))), nil
	}
	// Slice out the indexed hook. Preserves order of the survivors —
	// callers may rely on index stability for subsequent removes.
	mergedDef.OnComplete = append(mergedDef.OnComplete[:idx], mergedDef.OnComplete[idx+1:]...)
	return s.persistForkFromHookEdit(ctx, parent, mergedDef, "remove_hook")
}

// loadParentForHookOp resolves the parent def + decodes its definition
// into mergedScheduleDef. Returns a typed error string for the caller
// to wrap. Checks scope on the parent's name (so an agent with
// `named:weekly-digest` can't edit hooks on `daily-digest`).
func (s *ScheduleDef) loadParentForHookOp(ctx context.Context, policy tools.ScheduleDefPolicyValue, opLabel, defID string) (store.ScheduleDefRow, mergedScheduleDef, error) {
	parent, err := s.Store.ScheduleDefGet(ctx, defID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return store.ScheduleDefRow{}, mergedScheduleDef{}, fmt.Errorf("%s: def_id %q not found", opLabel, defID)
		}
		return store.ScheduleDefRow{}, mergedScheduleDef{}, fmt.Errorf("%s: %s", opLabel, err)
	}
	// RFC N: a hook edit may only target the caller's own tenant's def.
	// Opaque not-found — don't leak another tenant's def existence.
	if !defCallerIsAdmin(ctx) && parent.TenantID != tools.RunIdentity(ctx).TenantID {
		return store.ScheduleDefRow{}, mergedScheduleDef{}, fmt.Errorf("%s: def_id %q not found", opLabel, defID)
	}
	if err := s.checkScopeForName(policy, parent.Name); err != nil {
		return store.ScheduleDefRow{}, mergedScheduleDef{}, err
	}
	var def mergedScheduleDef
	if err := json.Unmarshal(parent.Definition, &def); err != nil {
		return store.ScheduleDefRow{}, mergedScheduleDef{}, fmt.Errorf("%s: decode parent definition: %s", opLabel, err)
	}
	return parent, def, nil
}

// persistForkFromHookEdit serialises the mutated def, runs the same
// validation chain fork uses, persists as a new version, and
// auto-promotes. Returns the row response with promoted=true.
func (s *ScheduleDef) persistForkFromHookEdit(ctx context.Context, parent store.ScheduleDefRow, def mergedScheduleDef, opLabel string) (tools.Result, error) {
	if err := validateScheduleDef(def); err != nil {
		return errResult(fmt.Sprintf("%s: %s", opLabel, err)), nil
	}
	if err := assertRequiredCredentials(def); err != nil {
		return errResult(fmt.Sprintf("%s: %s", opLabel, err)), nil
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("%s: marshal: %s", opLabel, err)), nil
	}
	if s.MaxDefinitionBytes > 0 && len(defJSON) > s.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("%s: definition (%d bytes) exceeds max %d", opLabel, len(defJSON), s.MaxDefinitionBytes)), nil
	}
	ident := tools.RunIdentity(ctx)
	row := store.ScheduleDefRow{
		DefID:            mintDefID(),
		Name:             parent.Name,
		ParentDefID:      parent.DefID,
		Definition:       defJSON,
		Description:      fmt.Sprintf("%s edit (parent v%d)", opLabel, parent.Version),
		CreatedByAgentID: ident.AgentID,
		// RFC N: the hook-edit version stays in the parent's tenant
		// (loadParentForHookOp already refused a cross-tenant parent).
		TenantID: parent.TenantID,
	}
	created, err := s.Store.ScheduleDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("%s: %s", opLabel, err)), nil
	}
	// Hook edits always auto-promote — there's no "stage and review"
	// use case for a hook addition the way there might be for a major
	// definition rewrite. Operators wanting the staged pattern can
	// use the regular `fork` op with explicit promote:false.
	if err := s.Store.ScheduleDefSetActive(ctx, parent.TenantID, parent.Name, created.DefID, ident.AgentID); err != nil {
		return errResult(fmt.Sprintf("%s: promote: %s", opLabel, err)), nil
	}
	_ = s.Store.ScheduleRunStateSeed(ctx, created.DefID, computeInitialNextRunAt(def, time.Now()))
	return okJSON(scheduleRowResponse(created, true))
}

// ---- helpers ----

func (s *ScheduleDef) checkScopeForName(policy tools.ScheduleDefPolicyValue, name string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("ScheduleDef tool: agent has no schedule_def_scopes (default-deny); add `schedule_def_scopes: [...]` to the agent yaml")
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
			// Same KNOWN GAP as AgentDef's "descendants" — accept on
			// presence; tighten when RunIdentity gains the parent
			// lineage walk surface.
			return nil
		default:
			if strings.HasPrefix(sc, "named:") {
				if strings.TrimPrefix(sc, "named:") == name {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("ScheduleDef tool: name %q not in this agent's schedule_def_scopes (%v)", name, policy.Scopes)
}

func (s *ScheduleDef) buildDefinition(name, parentJSON string, overlay json.RawMessage) (mergedScheduleDef, error) {
	base := mergedScheduleDef{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mergedScheduleDef{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if static, ok := s.Cfg.ScheduledRuns[name]; ok {
		// Create-with-static-name path is REFUSED in execCreate; this
		// branch handles fork's bootstrap-from-static when no parent
		// JSON yet but a static entry exists.
		base = staticToMergedScheduleDef(static)
	}

	if len(overlay) > 0 {
		var ov mergedScheduleDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedScheduleDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	return base, nil
}

func (s *ScheduleDef) bootstrapStatic(ctx context.Context, name string, static config.ScheduledRun) (store.ScheduleDefRow, error) {
	def := staticToMergedScheduleDef(static)
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.ScheduleDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.ScheduleDefRow{
		DefID:                  mintDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static cfg.ScheduledRuns",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
		// RFC N: bootstrap lands in the caller's tenant ("" for the boot
		// scheduler-bootstrap identity → the shared base).
		TenantID: ident.TenantID,
	}
	created, err := s.Store.ScheduleDefCreate(ctx, row)
	if err != nil {
		return store.ScheduleDefRow{}, err
	}
	if err := s.Store.ScheduleDefSetActive(ctx, ident.TenantID, name, created.DefID, ident.AgentID); err != nil {
		// Bootstrap succeeded but couldn't promote — return the row;
		// the next fork iteration finds it via the active pointer
		// retry. (Same posture as AgentDef.)
		return created, fmt.Errorf("promote bootstrap: %w", err)
	}
	// Seed schedule_run_state — see commentary in execCreate.
	_ = s.Store.ScheduleRunStateSeed(ctx, created.DefID, computeInitialNextRunAt(def, time.Now()))
	return created, nil
}

// BootstrapStaticSchedules materializes every static cfg.ScheduledRuns entry
// that lacks an active substrate version into the substrate (create v1 +
// promote + seed schedule_run_state). This is what lets a yaml-declared
// `scheduled_runs:` entry fire AUTONOMOUSLY — symmetric with a dynamically-
// created ScheduleDef, which already seeds run_state on promoted create. The
// sweeper's due-query is substrate-only (the schedule_run_state ⨝ active ⨝
// defs JOIN), so without this a yaml-only schedule has no run_state row and
// silently never fires until something forks/run-now's it.
//
// Idempotent + fork-respecting: a name that already has an active version is
// left untouched — whether that version is a prior bootstrap (so restarts
// don't re-seed / reset next_run_at) or an operator fork (so yaml never
// clobbers a deliberate override). A name's active pointer + run_state row
// persist across restarts, so only newly-added yaml schedules are seeded on a
// subsequent boot.
//
// Intended to run ONCE at boot, before the sweeper starts. Returns the count
// bootstrapped this call. nil Store/Cfg → no-op.
//
// NOTE (intentional scope): editing a yaml schedule's cron after it has been
// materialized does NOT auto-apply on restart (the active version wins);
// re-fork via the substrate tool to change it. Removing a yaml entry does not
// retire its substrate row. Those reconciliation refinements are deferred —
// this method closes the "static schedules never fire" gap only.
func (s *ScheduleDef) BootstrapStaticSchedules(ctx context.Context) (int, error) {
	if s.Store == nil || s.Cfg == nil {
		return 0, nil
	}
	names := make([]string, 0, len(s.Cfg.ScheduledRuns))
	for name := range s.Cfg.ScheduledRuns {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order for logs + tests
	// RFC N: boot bootstrap operates on the caller's tenant — the
	// scheduler-bootstrap identity carries "" (the shared base every tenant
	// inherits). bootstrapStatic stamps the same tenant.
	tenantID := tools.RunIdentity(ctx).TenantID
	seeded := 0
	for _, name := range names {
		_, err := s.Store.ScheduleDefGetActive(ctx, tenantID, name)
		if err == nil {
			continue // already has an active version (prior bootstrap or fork) — leave it
		}
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return seeded, fmt.Errorf("bootstrap %q: get active: %w", name, err)
		}
		if _, berr := s.bootstrapStatic(ctx, name, s.Cfg.ScheduledRuns[name]); berr != nil {
			return seeded, fmt.Errorf("bootstrap %q: %w", name, berr)
		}
		seeded++
	}
	return seeded, nil
}

// validateScheduleDef performs the substrate-side cron + on_complete
// computeInitialNextRunAt picks the cron from the def + computes the
// first fire moment strictly after `now`. Returns now+1h on resolve
// failure (matching the sweeper's same fallback in scheduler.fireOne)
// so a malformed def that somehow passes validation still parks
// instead of either crashing or re-firing every tick.
//
// This MUST be called after every ScheduleDefSetActive in the tool's
// create / fork / bootstrap paths — without seeding schedule_run_state,
// the sweeper's three-way JOIN query returns no rows for the def and
// nothing fires, regardless of `enabled` or cron syntax.
func computeInitialNextRunAt(def mergedScheduleDef, now time.Time) time.Time {
	fallback := now.Add(1 * time.Hour)
	expr := def.Schedule
	if expr == "" {
		// user_tier_schedules path: pick by def.UserTier if set; if
		// no tier supplied (which is itself a fork-time bug), park.
		if def.UserTier == "" || len(def.UserTierSchedules) == 0 {
			return fallback
		}
		var ok bool
		expr, ok = def.UserTierSchedules[def.UserTier]
		if !ok {
			return fallback
		}
	}
	parsed, err := cron.ParseStandard(expr)
	if err != nil {
		return fallback
	}
	loc := time.UTC
	if def.Timezone != "" {
		if l, err := time.LoadLocation(def.Timezone); err == nil {
			loc = l
		}
	}
	return parsed.Next(now.In(loc))
}

// validation that complements the boot-time config validator. Catches
// runtime-supplied overlays that would otherwise produce broken
// schedule rows the sweeper can't fire.
func validateScheduleDef(def mergedScheduleDef) error {
	if def.Agent == "" {
		return fmt.Errorf("agent: required")
	}
	if def.Schedule != "" && len(def.UserTierSchedules) > 0 {
		return fmt.Errorf("cannot set both schedule and user_tier_schedules (pick one)")
	}
	if def.Schedule != "" {
		if _, err := cron.ParseStandard(def.Schedule); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", def.Schedule, err)
		}
	}
	for tier, cronExpr := range def.UserTierSchedules {
		if _, err := cron.ParseStandard(cronExpr); err != nil {
			return fmt.Errorf("user_tier_schedules.%s: invalid cron %q: %w", tier, cronExpr, err)
		}
	}
	if def.CatchUpMax < 0 {
		return fmt.Errorf("catch_up_max must be >= 0")
	}
	for i, h := range def.OnComplete {
		switch h.Kind {
		case "channel.publish":
			if h.Channel == "" {
				return fmt.Errorf("on_complete[%d]: channel required for channel.publish", i)
			}
		case "mcp.call":
			if h.Server == "" || h.Tool == "" {
				return fmt.Errorf("on_complete[%d]: server + tool required for mcp.call", i)
			}
		case "memory.set":
			if h.Scope == "" || h.Key == "" {
				return fmt.Errorf("on_complete[%d]: scope + key required for memory.set", i)
			}
		default:
			return fmt.Errorf("on_complete[%d]: unknown kind %q", i, h.Kind)
		}
	}
	return nil
}

// assertRequiredCredentials checks the def's required_credentials
// manifest against the credentials map present in the merged def.
// Fork-time enforcement: a fork against a template that declares
// required_credentials: [jobs, slack] but supplies user_credentials:
// {jobs: ...} only is refused. RFC E sharp edge: loud-fail at fork
// time, not silent ingestion-time failure when the sweeper fires.
func assertRequiredCredentials(def mergedScheduleDef) error {
	if len(def.RequiredCredentials) == 0 {
		return nil
	}
	for _, key := range def.RequiredCredentials {
		v, ok := def.UserCredentials[key]
		if !ok || v == "" {
			return fmt.Errorf("required credential %q missing from user_credentials (template required: %v)", key, def.RequiredCredentials)
		}
	}
	return nil
}

// ---- response shape ----

func scheduleRowResponse(row store.ScheduleDefRow, promoted bool) map[string]any {
	m := scheduleRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func scheduleRowResponseMap(row store.ScheduleDefRow) map[string]any {
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
		"definition":               row.Definition,
	}
}

// ---- mergedScheduleDef: the JSON-tagged persistence shape ----
//
// Same conceptual fields as config.ScheduledRun but with JSON tags
// (snake_case) for the substrate-write path. The lookup.SubstrateScheduleDef
// adapter mirrors this exactly for read-side round-trip; a drift
// test pins parity.
//
// One additional field beyond config.ScheduledRun: UserCredentials.
// Forks need a place to store the per-fork credentials map (the
// counterpart to RFC F's RunInput.UserCredentials at the wire). The
// scheduler reads this when building RunInput from the schedule row.
type mergedScheduleDef struct {
	Agent               string                    `json:"agent,omitempty"`
	Prompt              []mergedSchedulePromptSeg `json:"prompt,omitempty"`
	Schedule            string                    `json:"schedule,omitempty"`
	UserTierSchedules   map[string]string         `json:"user_tier_schedules,omitempty"`
	RequiredCredentials []string                  `json:"required_credentials,omitempty"`
	Timezone            string                    `json:"timezone,omitempty"`
	// Enabled is *bool (not bool) because the overlay-merge path
	// distinguishes "overlay omits the field" (leave parent's value
	// alone) from "overlay sets enabled:false explicitly" (disable
	// the fork). With a plain bool, the JSON zero value (false)
	// silently clobbered the parent's enabled:true on any partial
	// overlay — fix landed alongside TestScheduleDefTool_Fork
	// PreservesEnabledWhenOverlayOmits.
	Enabled    *bool `json:"enabled,omitempty"`
	CatchUpMax int   `json:"catch_up_max,omitempty"`
	// MaxFires is the lifetime fire-count cap (RFC S / F36). *int (not int,
	// like Enabled is *bool) so a fork overlay can DISTINGUISH "field
	// omitted → inherit the parent's cap" from "explicitly {max_fires:0} →
	// reset to unbounded". nil = unbounded; non-nil 0 = explicitly
	// unbounded; N > 0 auto-retires the def after its Nth fire. The
	// read-side mirrors (SubstrateScheduleDef / scheduler.scheduleDef) keep
	// plain int — nil and 0 are both "unbounded" once stored, so they need
	// no such distinction.
	MaxFires *int   `json:"max_fires,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	// UserTier is the fork-time tier pick for templates with
	// user_tier_schedules. The scheduler's ResolveCron uses it to
	// select which cron expression to fire from the per-tier map.
	// Empty when the def has an explicit `schedule:` (no tier-pick
	// needed). The primary RFC E use case (JobEmber per-user fork
	// at high/middle/low tier) requires this field to round-trip
	// through the substrate — without it the sweeper can't resolve
	// which cron to use and parks the def with "tier missing" errors.
	UserTier               string               `json:"user_tier,omitempty"`
	UserCredentials        map[string]string    `json:"user_credentials,omitempty"`
	UserCredentialsFromEnv map[string]string    `json:"user_credentials_from_env,omitempty"`
	OnComplete             []mergedScheduleHook `json:"on_complete,omitempty"`
	// Metadata is NON-SECRET structured metadata passed to the agent as
	// TRUSTED (def-authored) via RunInput.Metadata. Per-fork (e.g. a repo
	// per fork) falls out of the overlay. Not for secrets (UserCredentials*).
	Metadata map[string]any `json:"metadata,omitempty"`
	// TenantID is the tenant the fired run EXECUTES as (RFC N follow-up).
	// Flows to RunInput.TenantID. Per-fork tenant falls out of the overlay.
	TenantID string `json:"tenant_id,omitempty"`
}

// mergedSchedulePromptSeg mirrors config.ScheduledRunSegment with JSON tags.
type mergedSchedulePromptSeg struct {
	Role    string                           `json:"role,omitempty"`
	Content []mergedSchedulePromptSegContent `json:"content,omitempty"`
}

// mergedSchedulePromptSegContent mirrors config.ScheduledRunSegmentContent.
type mergedSchedulePromptSegContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// mergedScheduleHook mirrors config.ScheduledRunHook.
type mergedScheduleHook struct {
	Kind    string                 `json:"kind,omitempty"`
	Channel string                 `json:"channel,omitempty"`
	Server  string                 `json:"server,omitempty"`
	Tool    string                 `json:"tool,omitempty"`
	Scope   string                 `json:"scope,omitempty"`
	Key     string                 `json:"key,omitempty"`
	Args    map[string]interface{} `json:"args,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

func (d *mergedScheduleDef) applyOverlay(ov mergedScheduleDef) {
	if ov.Agent != "" {
		d.Agent = ov.Agent
	}
	if ov.Prompt != nil {
		d.Prompt = ov.Prompt
	}
	if ov.Schedule != "" {
		d.Schedule = ov.Schedule
		// Mutual-exclusion: explicit schedule clears tier schedules.
		d.UserTierSchedules = nil
	}
	if ov.UserTierSchedules != nil {
		d.UserTierSchedules = ov.UserTierSchedules
		d.Schedule = ""
	}
	if ov.RequiredCredentials != nil {
		d.RequiredCredentials = ov.RequiredCredentials
	}
	if ov.Timezone != "" {
		d.Timezone = ov.Timezone
	}
	if ov.Enabled != nil {
		d.Enabled = ov.Enabled
	}
	if ov.CatchUpMax != 0 {
		d.CatchUpMax = ov.CatchUpMax
	}
	if ov.MaxFires != nil {
		// Non-nil (incl. an explicit 0) overrides; nil = overlay omitted
		// the field → inherit the parent's cap. This is what lets a fork
		// LIFT a cap with {max_fires:0}, per the tool's documented contract.
		d.MaxFires = ov.MaxFires
	}
	if ov.UserID != "" {
		d.UserID = ov.UserID
	}
	if ov.UserTier != "" {
		d.UserTier = ov.UserTier
	}
	// Credentials maps: partial-merge so fork-with-{slack: "<new>"}
	// preserves the parent's {jobs, telegram}. Matches RFC E's
	// worked-example step 6 ("substrate MERGES with the parent fork's
	// credential map").
	if len(ov.UserCredentials) > 0 {
		if d.UserCredentials == nil {
			d.UserCredentials = make(map[string]string, len(ov.UserCredentials))
		}
		for k, v := range ov.UserCredentials {
			d.UserCredentials[k] = v
		}
	}
	if len(ov.UserCredentialsFromEnv) > 0 {
		if d.UserCredentialsFromEnv == nil {
			d.UserCredentialsFromEnv = make(map[string]string, len(ov.UserCredentialsFromEnv))
		}
		for k, v := range ov.UserCredentialsFromEnv {
			d.UserCredentialsFromEnv[k] = v
		}
	}
	if ov.OnComplete != nil {
		d.OnComplete = ov.OnComplete
	}
	if ov.Metadata != nil {
		d.Metadata = ov.Metadata
	}
	if ov.TenantID != "" {
		d.TenantID = ov.TenantID
	}
}

func staticToMergedScheduleDef(sr config.ScheduledRun) mergedScheduleDef {
	enabled := sr.Enabled
	out := mergedScheduleDef{
		Agent:                  sr.Agent,
		Schedule:               sr.Schedule,
		UserTierSchedules:      sr.UserTierSchedules,
		RequiredCredentials:    sr.RequiredCredentials,
		Timezone:               sr.Timezone,
		Enabled:                &enabled,
		CatchUpMax:             sr.CatchUpMax,
		UserID:                 sr.UserID,
		UserCredentialsFromEnv: sr.UserCredentialsFromEnv,
		Metadata:               sr.Metadata,
		TenantID:               sr.TenantID,
	}
	// MaxFires is *int on the write side; carry the yaml value as a pointer
	// only when set (0 = unbounded → leave nil so the Definition JSON omits it).
	if sr.MaxFires != 0 {
		mf := sr.MaxFires
		out.MaxFires = &mf
	}
	if len(sr.Prompt) > 0 {
		out.Prompt = make([]mergedSchedulePromptSeg, len(sr.Prompt))
		for i, seg := range sr.Prompt {
			out.Prompt[i].Role = seg.Role
			if len(seg.Content) > 0 {
				out.Prompt[i].Content = make([]mergedSchedulePromptSegContent, len(seg.Content))
				for j, c := range seg.Content {
					out.Prompt[i].Content[j] = mergedSchedulePromptSegContent{Type: c.Type, Text: c.Text}
				}
			}
		}
	}
	if len(sr.OnComplete) > 0 {
		out.OnComplete = make([]mergedScheduleHook, len(sr.OnComplete))
		for i, h := range sr.OnComplete {
			out.OnComplete[i] = mergedScheduleHook{
				Kind: h.Kind, Channel: h.Channel,
				Server: h.Server, Tool: h.Tool,
				Scope: h.Scope, Key: h.Key,
				Args: h.Args, Payload: h.Payload,
			}
		}
	}
	return out
}
