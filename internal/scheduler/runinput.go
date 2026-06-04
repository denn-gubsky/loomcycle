package scheduler

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// scheduleDef is the wire shape the sweeper unmarshals from
// store.ScheduleDueRow.Definition. It mirrors mergedScheduleDef in
// internal/tools/builtin (write side) + lookup.SubstrateScheduleDef
// (read adapter side); the drift test in the builtin package pins
// parity so a field added on either side without the matching
// addition here fails CI loudly.
//
// We re-declare the shape here (vs importing the builtin's
// mergedScheduleDef) to avoid a builtin → scheduler dependency
// cycle: builtin's ScheduleDef tool already imports the store, and
// the scheduler imports the store + runner. A shared canonical type
// would require a new shared package; the duplication is small
// enough that locking it with the drift test is cheaper.
type scheduleDef struct {
	Agent                  string              `json:"agent,omitempty"`
	Prompt                 []schedulePromptSeg `json:"prompt,omitempty"`
	Schedule               string              `json:"schedule,omitempty"`
	UserTierSchedules      map[string]string   `json:"user_tier_schedules,omitempty"`
	RequiredCredentials    []string            `json:"required_credentials,omitempty"`
	Timezone               string              `json:"timezone,omitempty"`
	Enabled                *bool               `json:"enabled,omitempty"`
	CatchUpMax             int                 `json:"catch_up_max,omitempty"`
	UserID                 string              `json:"user_id,omitempty"`
	UserCredentials        map[string]string   `json:"user_credentials,omitempty"`
	UserCredentialsFromEnv map[string]string   `json:"user_credentials_from_env,omitempty"`
	UserTier               string              `json:"user_tier,omitempty"`
	OnComplete             []scheduleHook      `json:"on_complete,omitempty"`
	Metadata               map[string]any      `json:"metadata,omitempty"`
	TenantID               string              `json:"tenant_id,omitempty"`
}

type schedulePromptSeg struct {
	Role    string                  `json:"role,omitempty"`
	Content []schedulePromptContent `json:"content,omitempty"`
}

type schedulePromptContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// scheduleHook mirrors mergedScheduleHook on the substrate side. Three
// closed-set kinds: channel.publish / mcp.call / memory.set. Anything
// else is refused at validation time on the write side; the sweeper
// re-validates defensively in dispatch.go.
type scheduleHook struct {
	Kind    string                 `json:"kind,omitempty"`
	Channel string                 `json:"channel,omitempty"`
	Server  string                 `json:"server,omitempty"`
	Tool    string                 `json:"tool,omitempty"`
	Scope   string                 `json:"scope,omitempty"`
	Key     string                 `json:"key,omitempty"`
	Args    map[string]interface{} `json:"args,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// unmarshalDef converts the store-side JSON body into the runtime
// shape. Returns a typed error so the sweeper can record the def
// as "failed" rather than crash on a malformed row.
func unmarshalDef(body []byte) (scheduleDef, error) {
	var def scheduleDef
	if err := json.Unmarshal(body, &def); err != nil {
		return scheduleDef{}, fmt.Errorf("decode schedule definition: %w", err)
	}
	if def.Agent == "" {
		return def, fmt.Errorf("schedule definition missing required `agent` field")
	}
	return def, nil
}

// buildRunInput converts a unmarshaled schedule definition into the
// RunInput the runner consumes. Credential resolution happens here:
// the def's `user_credentials` map (explicit fork-time values) is
// merged with `user_credentials_from_env` (resolved against os.Getenv).
// Explicit values win when both are set for the same key, with a
// warning logged via the supplied logger.
//
// envAllowlist gates which env vars are readable. Operators who set
// LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST gate this; the empty allowlist
// (default) means UserCredentialsFromEnv is ignored entirely — a
// safe-by-default posture that requires opt-in to surface env-var
// values to scheduled runs.
func buildRunInput(def scheduleDef, envAllowlist map[string]bool, logf func(format string, args ...any)) runner.RunInput {
	creds := make(map[string]string)
	for k, envName := range def.UserCredentialsFromEnv {
		if !envAllowlist[envName] {
			if logf != nil {
				logf("scheduler: env var %q for credential key %q not in allowlist — skipping", envName, k)
			}
			continue
		}
		v := os.Getenv(envName)
		if v == "" {
			if logf != nil {
				logf("scheduler: env var %q for credential key %q is empty — skipping", envName, k)
			}
			continue
		}
		creds[k] = v
	}
	for k, v := range def.UserCredentials {
		if _, hadEnv := creds[k]; hadEnv && logf != nil {
			logf("scheduler: credential key %q has both explicit + env source — explicit value wins", k)
		}
		creds[k] = v
	}

	segs := make([]loop.PromptSegment, 0, len(def.Prompt))
	for _, p := range def.Prompt {
		out := loop.PromptSegment{Role: p.Role}
		for _, c := range p.Content {
			out.Content = append(out.Content, loop.PromptContentBlock{
				Type: c.Type,
				Text: c.Text,
			})
		}
		segs = append(segs, out)
	}

	return runner.RunInput{
		Agent:           def.Agent,
		Segments:        segs,
		UserID:          def.UserID,
		UserTier:        def.UserTier,
		UserCredentials: creds,
		// Non-secret, operator-authored → TRUSTED. The scheduler has no
		// external inbound body, so there is no PayloadMetadata here.
		Metadata: def.Metadata,
		// TenantID comes from the def ONLY (operator-authored) — the run
		// executes as this tenant, resolving its agents/skills/MCP and
		// isolating memory/runs. "" = shared/default (RFC N follow-up).
		TenantID: def.TenantID,
	}
}
