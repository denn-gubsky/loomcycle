package snapshot

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Guards on the envelope's TYPES (RFC DP §3.2 c, d): a field cannot join a
// snapshot entry without someone deciding it is safe in a portable file, and a
// store row type cannot be embedded so that its next column rides along.

var storePkgPath = reflect.TypeOf(store.Run{}).PkgPath()

// allowedStoreTypes are store types an entry may hold, each reviewed. A
// store row type is not here: a column added to it would flow into every
// envelope without anyone deciding it is safe to.
var allowedStoreTypes = map[reflect.Type]bool{
	reflect.TypeOf(store.ParentContext{}): true,
}

// envelopeStructs walks every struct type reachable from Sections: the
// snapshot package's own, and the allowlisted store types (whose fields are
// envelope content too). Returns them keyed by a stable name.
func envelopeStructs() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	snapPkg := reflect.TypeOf(Sections{}).PkgPath()
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			if rt.Kind() == reflect.Map {
				walk(rt.Key())
			}
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct {
			return
		}
		if rt.PkgPath() != snapPkg && !allowedStoreTypes[rt] {
			return
		}
		name := typeName(rt)
		if _, seen := out[name]; seen {
			return
		}
		out[name] = rt
		for i := 0; i < rt.NumField(); i++ {
			walk(rt.Field(i).Type)
		}
	}
	walk(reflect.TypeOf(Sections{}))
	return out
}

func typeName(rt reflect.Type) string {
	if rt.PkgPath() == storePkgPath {
		return "store." + rt.Name()
	}
	return rt.Name()
}

// TestSnapshotEntries_NeverEmbedStoreRows: no field of any envelope type is a
// store type (bare, pointer, slice, map or embedded) other than the reviewed
// allowlist.
func TestSnapshotEntries_NeverEmbedStoreRows(t *testing.T) {
	structs := envelopeStructs()
	if len(structs) < 30 {
		t.Fatalf("walked %d envelope types; the walk is not reaching the entries", len(structs))
	}
	var check func(owner, field string, rt reflect.Type, embedded bool)
	check = func(owner, field string, rt reflect.Type, embedded bool) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			if rt.Kind() == reflect.Map {
				check(owner, field, rt.Key(), false)
			}
			rt = rt.Elem()
		}
		if rt.PkgPath() != storePkgPath {
			return
		}
		if embedded || !allowedStoreTypes[rt] {
			t.Errorf("%s.%s has type %s from package store. Mirror the fields you mean to carry in "+
				"a snapshot type instead, so a new store column cannot reach the envelope unreviewed.",
				owner, field, typeName(rt))
		}
	}
	for name, rt := range structs {
		if rt.PkgPath() == storePkgPath {
			continue // an allowlisted store type's own fields are pinned below
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			check(name, f.Name, f.Type, f.Anonymous)
		}
	}
}

// pinnedEntryKeys is the reviewed JSON key set of every envelope type. A new
// field fails the test below until it is added here, which is the point: the
// person adding it must decide it is safe in a PORTABLE file that leaves this
// deployment. Do not add a key without that decision.
var pinnedEntryKeys = map[string][]string{
	"Sections": {"users", "token_limits", "agent_defs", "agent_def_active", "skill_defs", "skill_def_active", "team_defs",
		"team_def_active", "hook_defs", "hook_def_active", "mcp_server_defs", "mcp_server_def_active", "volume_defs",
		"memory_backend_defs", "memory_backend_def_active", "document_source_defs", "document_source_def_active",
		"a2a_agent_defs", "a2a_agent_def_active", "a2a_server_card_defs", "a2a_server_card_def_active",
		"memory", "memory_pending", "channels", "channel_defs", "webhook_defs", "webhook_def_active", "schedule_defs",
		"schedule_def_active", "evaluations", "paused_runs", "interaction_history", "sqlmem", "capture_findings"},

	"UsersSection":              {"version", "entries"},
	"TokenLimitsSection":        {"version", "entries", "usage_mtd"},
	"AgentDefsSection":          {"version", "entries"},
	"AgentDefActiveSection":     {"version", "entries"},
	"SkillDefsSection":          {"version", "entries"},
	"SkillDefActiveSection":     {"version", "entries"},
	"TeamDefsSection":           {"version", "entries"},
	"TeamDefActiveSection":      {"version", "entries"},
	"HookDefsSection":           {"version", "entries"},
	"HookDefActiveSection":      {"version", "entries"},
	"MCPServerDefsSection":      {"version", "entries"},
	"MCPServerDefActiveSection": {"version", "entries"},
	"MemorySection":             {"version", "entries"},
	"ChannelsSection":           {"version", "config", "messages", "cursors"},
	"ChannelDefsSection":        {"version", "entries"},
	"EvaluationsSection":        {"version", "entries"},
	"PausedRunsSection":         {"version", "entries"},
	"InteractionHistorySection": {"version", "since_ts", "events"},
	"SqlMemSection":             {"version", "tier", "scopes", "skipped_scopes"},
	"CaptureFindingsSection":    {"version", "entries"},

	"UserEntry": {"tenant_id", "subject", "display_name", "access_mode", "status", "created_at", "created_by"},
	"TokenLimitEntry": {"tenant_id", "scope", "scope_id", "soft_limit", "hard_limit", "updated_at",
		"updated_by"},
	// Budget state only: one total per (tenant, user). It must never gain a
	// cost, provider, model, key source or per-call time — that is billing data.
	"UsageMTDBlock": {"month", "entries"},
	"UsageMTDEntry": {"tenant_id", "user_id", "tokens"},

	"AgentDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static",
		"content_sha256", "operator_authored"},
	"SkillDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static",
		"content_sha256"},
	"TeamDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static",
		"content_sha256", "operator_authored"},
	"HookDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "content_sha256"},
	"MCPServerDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static",
		"content_sha256"},
	"AgentDefActiveEntry":     {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},
	"SkillDefActiveEntry":     {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},
	"TeamDefActiveEntry":      {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id", "promoter"},
	"TeamDefPromoterEntry":    {"operator_key_restricted", "isolated"},
	"HookDefActiveEntry":      {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},
	"MCPServerDefActiveEntry": {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},

	"WebhookDefsSection":      {"version", "entries"},
	"WebhookDefActiveSection": {"version", "entries"},
	// definition has its literal user_credentials values stripped at capture;
	// stripped_credentials lists KEYS only. It must never gain a field for a
	// credential value, nor one that re-projects the body through a struct.
	"WebhookDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static",
		"stripped_credentials"},
	"WebhookDefActiveEntry": {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},

	// A dynamic volume travels as name and mode. NO path and no definition:
	// lookup trusts a stored path, so the target derives it. Never add one.
	"VolumeDefsSection": {"version", "entries"},
	"VolumeDefEntry":    {"tenant_id", "name", "mode", "created_at", "updated_at"},

	// Memory-backend and document-source defs carry env var NAMES only. The
	// definition is the stored body; it must never gain a resolved value.
	"MemoryBackendDefsSection":      {"version", "entries"},
	"MemoryBackendDefActiveSection": {"version", "entries"},
	"MemoryBackendDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static"},
	"MemoryBackendDefActiveEntry": {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},
	"DocSourceDefsSection":        {"version", "entries"},
	"DocSourceDefActiveSection":   {"version", "entries"},
	"DocSourceDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static"},
	"DocSourceDefActiveEntry": {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},

	// A2A defs carry references only (a per-run credential key, an env name).
	"A2AAgentDefsSection":      {"version", "entries"},
	"A2AAgentDefActiveSection": {"version", "entries"},
	"A2AAgentDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static"},
	"A2AAgentDefActiveEntry":        {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},
	"A2AServerCardDefsSection":      {"version", "entries"},
	"A2AServerCardDefActiveSection": {"version", "entries"},
	"A2AServerCardDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static"},
	"A2AServerCardDefActiveEntry": {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},

	"ScheduleDefsSection":      {"version", "entries"},
	"ScheduleDefActiveSection": {"version", "entries"},
	// definition has its literal user_credentials values stripped at capture;
	// stripped_credentials lists KEYS only. It must never gain a field for a
	// credential value, nor one that re-projects the body through a struct.
	"ScheduleDefEntry": {"def_id", "tenant_id", "name", "version", "parent_def_id", "definition", "description",
		"created_at", "created_by_agent_id", "created_by_run_id", "retired", "bootstrapped_from_static",
		"stripped_credentials", "run_state"},
	"ScheduleRunStateEntry":  {"next_run_at", "last_run_at", "last_run_id", "last_status", "last_error", "paused_until", "fire_count"},
	"ScheduleDefActiveEntry": {"name", "tenant_id", "def_id", "promoted_at", "promoted_by_agent_id"},

	"MemoryEntry": {"tenant_id", "scope", "scope_id", "key", "value", "expires_at", "created_at", "updated_at",
		"observed_at", "valid_at", "invalid_at", "embedding"},
	"MemoryEmbeddingSnapshot": {"provider", "model", "dimension", "vector", "embed_text", "created_at"},

	// An undrained consolidation-queue row. The payload is the queued
	// conversation — user data, not an operator secret. NO drained_at (only
	// undrained rows travel) and NO lease or claim holder: a source replica's
	// claim must never come back on the target. Never add one.
	"MemoryPendingSection": {"version", "entries"},
	"MemoryPendingEntry": {"id", "tenant_id", "scope", "scope_id", "payload", "origin", "source_session_id",
		"source_run_id", "created_at"},

	"ChannelConfigEntry": {"name", "description", "scope", "ttl_seconds", "max_messages", "allowed_publishers"},
	"ChannelMessageEntry": {"id", "channel", "scope", "scope_id", "payload", "published_at", "expires_at",
		"visible_at", "published_by_user_id", "tenant_id", "origin", "hook_tenant", "requested_visible_at"},
	"ChannelCursorEntry": {"channel", "tenant_id", "scope", "scope_id", "cursor", "updated_at"},
	"ChannelDefEntry": {"name", "tenant_id", "description", "scope", "semantic", "default_ttl", "max_messages",
		"publisher", "period", "hold", "hooks", "created_at"},

	"EvaluationEntry": {"eval_id", "run_id", "def_id", "score", "dimensions", "judgement", "rationale",
		"emitter_role", "emitter_agent_id", "emitter_run_id", "created_at"},

	"PausedRunEntry": {"run_id", "agent_id", "parent_agent_id", "user_id", "user_tier", "agent", "agent_def_id",
		"session_id", "started_at", "model", "pause_state", "tenant_id", "parent_run_id", "interactive",
		"operator_key_restricted", "isolated", "parent_context", "run_config", "transcript_events",
		"transcript_error"},
	"TranscriptEvent": {"seq", "ts_ns", "type", "payload"},
	"store.ParentContext": {"root_agent_run_id", "function_key", "tier_at_run", "board_scope", "board_chunk_id",
		"board_document_id", "walk_id", "wave_id", "wave_index", "state", "state_visit"},

	"SqlMemScope":        {"tenant", "scope", "scope_id", "ddl", "post_ddl", "tables"},
	"SqlMemSkippedScope": {"tenant", "scope", "scope_id", "bytes"},
	"SqlMemTable":        {"name", "columns", "column_types", "rows"},

	// A finding names a location. It must never gain a key for the value, a
	// prefix, a length or a hash of it.
	"CaptureFindingEntry": {"section", "tenant_id", "name", "def_id", "field", "detector"},
}

// TestSnapshotEntries_KeySetsArePinned: every envelope type's JSON key set is
// exactly its reviewed pin, and every pin is for a type the envelope still
// reaches.
func TestSnapshotEntries_KeySetsArePinned(t *testing.T) {
	structs := envelopeStructs()
	for name, rt := range structs {
		pin, ok := pinnedEntryKeys[name]
		if !ok {
			t.Errorf("envelope type %s has no key-set pin. Review each of its fields for a portable, "+
				"secret-free envelope, then pin them.", name)
			continue
		}
		want := map[string]bool{}
		for _, k := range pin {
			want[k] = true
		}
		have := map[string]bool{}
		for i := 0; i < rt.NumField(); i++ {
			if k := jsonKey(rt.Field(i)); k != "" {
				have[k] = true
			}
		}
		for k := range have {
			if !want[k] {
				t.Errorf("%s carries an unreviewed key %q. Pin it only after deciding it is safe to put "+
					"in a portable envelope — the question this test exists to force.", name, k)
			}
		}
		for k := range want {
			if !have[k] {
				t.Errorf("%s no longer has pinned key %q; update the pin", name, k)
			}
		}
	}
	for name := range pinnedEntryKeys {
		if _, ok := structs[name]; !ok {
			t.Errorf("pin for %s names a type the envelope no longer reaches; remove it", name)
		}
	}
}
