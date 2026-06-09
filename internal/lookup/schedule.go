package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ScheduleStore is the subset of store.Store the schedule resolver
// uses. Declared here so tests + callers can mock without depending
// on the full store interface. RFC N: the substrate lookup carries a
// tenantID.
type ScheduleStore interface {
	ScheduleDefGetActive(ctx context.Context, tenantID, name string) (store.ScheduleDefRow, error)
}

// Schedule resolves a schedule NAME to its effective config.ScheduledRun
// within the caller's tenant, walking the lookup chain in precedence order
// (mirrors lookup.Agent):
//
//  1. (tenantID != "") tenant-scoped substrate (schedule_def_active
//     WHERE tenant_id=tenantID)
//  2. static cfg.ScheduledRuns (yaml-defined, the shared operator base)
//  3. shared substrate (tenant_id="")
//
// For the default tenant "" step 1 is skipped, collapsing to
// static-cfg → shared-substrate — identical to the pre-RFC-N behavior.
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false).
//
// Normalization: every dynamic path applies NormalizeScheduleDef before
// returning, equalizing the runtime shape with what config-load would
// produce for the same yaml content. Static cfg.ScheduledRuns entries
// already went through config-load's validation pass; the cfg path returns
// directly without re-normalizing.
func Schedule(ctx context.Context, s ScheduleStore, cfg *config.Config, tenantID, name string) (config.ScheduledRun, bool) {
	// 1. Tenant-scoped substrate (skipped for the shared "" tenant).
	if tenantID != "" {
		if sr, ok := resolveScheduleSubstrate(ctx, s, tenantID, name); ok {
			return sr, true
		}
	}
	// 2. Static cfg.ScheduledRuns — the shared operator base.
	if cfg != nil {
		if sr, ok := cfg.ScheduledRuns[name]; ok {
			return sr, true
		}
	}
	// 3. Shared substrate (tenant_id="").
	return resolveScheduleSubstrate(ctx, s, "", name)
}

// resolveScheduleSubstrate reads the schedule_def_active overlay for one
// tenant pass, applying NormalizeScheduleDef. Returns (zero, false) on nil
// store, no active pointer for that tenant, or malformed row JSON.
func resolveScheduleSubstrate(ctx context.Context, s ScheduleStore, tenantID, name string) (config.ScheduledRun, bool) {
	if s == nil {
		return config.ScheduledRun{}, false
	}
	activeRow, err := s.ScheduleDefGetActive(ctx, tenantID, name)
	if err != nil {
		return config.ScheduledRun{}, false
	}
	var sd SubstrateScheduleDef
	if uerr := json.Unmarshal(activeRow.Definition, &sd); uerr != nil {
		return config.ScheduledRun{}, false
	}
	sr := sd.ToConfigDef()
	NormalizeScheduleDef(&sr)
	return sr, true
}

// SubstrateScheduleDef mirrors the JSON shape `ScheduleDef.create` /
// `ScheduleDef.fork` persists in `schedule_defs.definition` (snake_case
// JSON tags via the `mergedScheduleDef` adapter in
// internal/tools/builtin/scheduledef.go). The runtime consumer
// (`config.ScheduledRun`) carries ONLY yaml tags — unmarshalling
// substrate JSON directly into it silently drops every field because
// json.Unmarshal then matches against Go field names instead.
//
// This adapter + ToConfigDef is the seam. Kept in sync with
// `mergedScheduleDef`; TestMergedScheduleDef_DriftDetection_VsLookupSubstrateScheduleDef
// in the builtin package pins the field set so a future field added
// to either side without the matching addition here fails CI.
type SubstrateScheduleDef struct {
	Agent               string                   `json:"agent,omitempty"`
	Prompt              []SubstratePromptSegment `json:"prompt,omitempty"`
	Schedule            string                   `json:"schedule,omitempty"`
	UserTierSchedules   map[string]string        `json:"user_tier_schedules,omitempty"`
	RequiredCredentials []string                 `json:"required_credentials,omitempty"`
	Timezone            string                   `json:"timezone,omitempty"`
	// Enabled is *bool with omitempty (matching the write-side
	// mergedScheduleDef shape so the JSON round-trip is lossless).
	// A nil pointer means "the substrate row didn't explicitly set
	// enabled" — callers default to true in that case (a fork created
	// without explicit enabled should run, unless the operator
	// disables it). v1.x review-fix: previously this was `bool` with
	// no omitempty, which silently coerced missing-fields to false on
	// the lookup path even when the write side intended true.
	Enabled    *bool  `json:"enabled,omitempty"`
	CatchUpMax int    `json:"catch_up_max,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	// UserTier is the fork-time tier pick — see the matching field
	// commentary on builtin.mergedScheduleDef. Required for sweeper-
	// side cron resolution on templates with user_tier_schedules.
	UserTier               string                  `json:"user_tier,omitempty"`
	UserCredentialsFromEnv map[string]string       `json:"user_credentials_from_env,omitempty"`
	OnComplete             []SubstrateScheduleHook `json:"on_complete,omitempty"`
	Metadata               map[string]any          `json:"metadata,omitempty"`
	TenantID               string                  `json:"tenant_id,omitempty"`
}

// SubstratePromptSegment mirrors config.ScheduledRunSegment with
// JSON tags.
type SubstratePromptSegment struct {
	Role    string                          `json:"role,omitempty"`
	Content []SubstratePromptSegmentContent `json:"content,omitempty"`
}

// SubstratePromptSegmentContent mirrors config.ScheduledRunSegmentContent.
type SubstratePromptSegmentContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// SubstrateScheduleHook mirrors config.ScheduledRunHook with JSON tags.
type SubstrateScheduleHook struct {
	Kind    string                 `json:"kind,omitempty"`
	Channel string                 `json:"channel,omitempty"`
	Server  string                 `json:"server,omitempty"`
	Tool    string                 `json:"tool,omitempty"`
	Scope   string                 `json:"scope,omitempty"`
	Key     string                 `json:"key,omitempty"`
	Args    map[string]interface{} `json:"args,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto config.ScheduledRun
// for the runtime to consume. Pure data shuffling; no normalization
// happens here (NormalizeScheduleDef is called afterward by Schedule).
func (s SubstrateScheduleDef) ToConfigDef() config.ScheduledRun {
	// Enabled is *bool on the substrate side; default to true when
	// absent so a freshly-created fork without an explicit enabled
	// setting runs. The yaml-side default is the operator's choice;
	// the substrate-side default (when the field is missing) is
	// run-by-default since that matches the v1.x semantics where
	// every successful fork is expected to fire on its cron.
	enabled := true
	if s.Enabled != nil {
		enabled = *s.Enabled
	}
	out := config.ScheduledRun{
		Agent:                  s.Agent,
		Schedule:               s.Schedule,
		UserTierSchedules:      s.UserTierSchedules,
		RequiredCredentials:    s.RequiredCredentials,
		Timezone:               s.Timezone,
		Enabled:                enabled,
		CatchUpMax:             s.CatchUpMax,
		UserID:                 s.UserID,
		UserCredentialsFromEnv: s.UserCredentialsFromEnv,
		Metadata:               s.Metadata,
		TenantID:               s.TenantID,
	}
	if len(s.Prompt) > 0 {
		out.Prompt = make([]config.ScheduledRunSegment, len(s.Prompt))
		for i, seg := range s.Prompt {
			out.Prompt[i].Role = seg.Role
			if len(seg.Content) > 0 {
				out.Prompt[i].Content = make([]config.ScheduledRunSegmentContent, len(seg.Content))
				for j, c := range seg.Content {
					out.Prompt[i].Content[j] = config.ScheduledRunSegmentContent{Type: c.Type, Text: c.Text}
				}
			}
		}
	}
	if len(s.OnComplete) > 0 {
		out.OnComplete = make([]config.ScheduledRunHook, len(s.OnComplete))
		for i, h := range s.OnComplete {
			out.OnComplete[i] = config.ScheduledRunHook{
				Kind: h.Kind, Channel: h.Channel,
				Server: h.Server, Tool: h.Tool,
				Scope: h.Scope, Key: h.Key,
				Args: h.Args, Payload: h.Payload,
			}
		}
	}
	return out
}

// NormalizeScheduleDef applies the boot-time normalization chain that
// config validation applies to yaml-defined entries. Called by
// Schedule() on every dynamic-load path so substrate-resolved
// schedules reach the runtime with the same effective shape as
// yaml-resolved ones.
//
// Today this is a placeholder for future normalizers (timezone
// defaults, on_complete payload shape, etc.). Mirrors NormalizeAgentDef's
// shape so the pattern is consistent across substrates.
func NormalizeScheduleDef(sr *config.ScheduledRun) {
	if sr == nil {
		return
	}
	// Empty timezone → UTC (per RFC E sharp edge "Timezone optional,
	// UTC default"). This is the only normalizer today.
	if sr.Timezone == "" {
		sr.Timezone = "UTC"
	}
}
