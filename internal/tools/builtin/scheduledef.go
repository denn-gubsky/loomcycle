package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	`Operations: create, fork, get, list, retire.`

const scheduleDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Schedule name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of ScheduledRun for create/fork. Immutable / server-set fields are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create + fork both default true (schedules' fork-versioning model expects new versions to replace old). Pass false to leave the existing active pointer in place."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
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
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire)", in.Op)), nil
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

	ident := tools.RunIdentity(ctx)
	row := store.ScheduleDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
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
		if err := s.Store.ScheduleDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(scheduleRowResponse(created, promote))
}

// ---- fork ----

func (s *ScheduleDef) execFork(ctx context.Context, policy tools.ScheduleDefPolicyValue, in scheduleDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// Resolve the parent. Three paths (mirror AgentDef):
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + active pointer exists → use it
	//   3. neither → name must have a yaml template; bootstrap v1
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
		parent = row
	} else {
		row, err := s.Store.ScheduleDefGetActive(ctx, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No active pointer → must bootstrap from yaml.
			static, ok := s.Cfg.ScheduledRuns[in.Name]
			if !ok {
				return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version nor a static cfg.ScheduledRuns entry", in.Name)), nil
			}
			bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
			if berr != nil {
				// Concurrent first-fork may have already bootstrapped v1;
				// re-read active pointer before propagating.
				if row2, gerr := s.Store.ScheduleDefGetActive(ctx, in.Name); gerr == nil {
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

	ident := tools.RunIdentity(ctx)
	row := store.ScheduleDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
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
		if err := s.Store.ScheduleDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
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
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
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
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := s.Store.ScheduleDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
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
	}
	created, err := s.Store.ScheduleDefCreate(ctx, row)
	if err != nil {
		return store.ScheduleDefRow{}, err
	}
	if err := s.Store.ScheduleDefSetActive(ctx, name, created.DefID, ident.AgentID); err != nil {
		// Bootstrap succeeded but couldn't promote — return the row;
		// the next fork iteration finds it via the active pointer
		// retry. (Same posture as AgentDef.)
		return created, fmt.Errorf("promote bootstrap: %w", err)
	}
	return created, nil
}

// validateScheduleDef performs the substrate-side cron + on_complete
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
	Enabled                *bool                `json:"enabled,omitempty"`
	CatchUpMax             int                  `json:"catch_up_max,omitempty"`
	UserID                 string               `json:"user_id,omitempty"`
	UserCredentials        map[string]string    `json:"user_credentials,omitempty"`
	UserCredentialsFromEnv map[string]string    `json:"user_credentials_from_env,omitempty"`
	OnComplete             []mergedScheduleHook `json:"on_complete,omitempty"`
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
	if ov.UserID != "" {
		d.UserID = ov.UserID
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
