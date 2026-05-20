package mcp

import (
	"encoding/json"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// toolDescriptors returns the MCP tool catalogue. Count is asserted
// by TestServer_ToolsList in server_test.go — let that test be the
// authoritative source of "how many tools" rather than restating it
// here (the comment would otherwise drift on every new addition).
// Each descriptor carries name + description + input schema; the
// schemas are intentionally minimal — full input validation lives
// at the connector layer (or, for builtin wrappers, at the
// underlying tool's discriminated-op schema).
//
// Naming convention: flat `verb_noun` for actions; single-word for
// builtin wrappers (memory, channel, agentdef, skilldef, evaluation, context)
// whose inner `op` field already discriminates the operation.
func toolDescriptors() []loommcp.ToolDescriptor {
	return []loommcp.ToolDescriptor{
		// --- Run lifecycle ---
		{
			Name:        "spawn_run",
			Description: "Spawn an agent run. Blocks until completion; final text + usage returned. When the session opted into runEvents via initialize.capabilities.loomcycle.runEvents=true, intermediate events stream as notifications/loomcycle/run_event during the call. Exactly one of `agent` (fresh run against a registered agent) or `session_id` (continuation of an existing session) must be supplied.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"agent":            {"type": "string", "description": "Registered agent name. Required for fresh runs; ignored for continuations (session's stored agent is authoritative)."},
					"segments":         {"type": "array",  "description": "Prompt segments. Typically required for fresh runs; continuations may omit when the caller has nothing new to add."},
					"session_id":       {"type": "string", "description": "Set to continue an existing session. When set, agent is ignored."},
					"tenant_id":        {"type": "string"},
					"user_id":          {"type": "string"},
					"agent_id":         {"type": "string", "description": "Optional caller-supplied tracking handle."},
					"user_tier":        {"type": "string"},
					"user_bearer":      {"type": "string", "description": "Per-run MCP bearer (substituted into ${run.user_bearer} in mcp_servers.*.headers)."},
					"allowed_tools":    {"type": "array", "items": {"type": "string"}},
					"allowed_hosts":    {"type": "array", "items": {"type": "string"}, "description": "OMIT for no narrowing (operator's static allowlist applies). Pass empty array [] to DENY ALL outbound HTTP. Pass non-empty array to intersect with operator's list."},
					"web_search_filter": {"type": "string", "enum": ["drop", "keep"]}
				},
				"anyOf": [
					{"required": ["agent"]},
					{"required": ["session_id"]}
				]
			}`),
		},
		{
			Name:        "cancel_run",
			Description: "Cancel a running agent by agent_id. Cascades to sub-agents. Idempotent.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["agent_id"],
				"properties": {
					"agent_id": {"type": "string"},
					"reason":   {"type": "string"}
				}
			}`),
		},
		{
			Name:        "get_run",
			Description: "Return the latest status snapshot for a tracked agent_id.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["agent_id"],
				"properties": {"agent_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "list_runs",
			Description: "Enumerate runs. user_id filter is required in v0.8.15.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["user_id"],
				"properties": {
					"user_id": {"type": "string"},
					"status":  {"type": "string", "enum": ["running", "completed", "failed", "cancelled"]},
					"limit":   {"type": "integer", "minimum": 1, "maximum": 200}
				}
			}`),
		},

		// --- Agent management ---
		{
			Name:        "register_agent",
			Description: "Register a dynamic agent at runtime. Survives until TTL expires or unregister_agent is called. Bash/Write/Edit are stripped from allowed_tools unless the operator set LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["name", "system_prompt", "allowed_tools"],
				"properties": {
					"name":          {"type": "string", "pattern": "^[A-Za-z0-9_-]{1,64}$"},
					"system_prompt": {"type": "string", "maxLength": 65536},
					"allowed_tools": {"type": "array", "items": {"type": "string"}, "minItems": 1},
					"tier":          {"type": "string"},
					"provider":      {"type": "string"},
					"model":         {"type": "string"},
					"effort":        {"type": "string", "enum": ["minimal", "low", "medium", "high"]},
					"max_tokens":    {"type": "integer", "minimum": 1},
					"memory_scopes": {"type": "array", "items": {"type": "string"}},
					"description":   {"type": "string"},
					"ttl_seconds":   {"type": "integer", "description": "TTL in seconds. 0 = env default (24h). -1 = no expiry."}
				}
			}`),
		},
		{
			Name:        "unregister_agent",
			Description: "Remove a dynamic agent. Cannot unregister static (yaml-defined) agents. Idempotent on missing names.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["name"],
				"properties": {"name": {"type": "string"}}
			}`),
		},
		{
			Name:        "list_agents",
			Description: "List all agents — static (from yaml) and dynamic (TTL-active rows from dynamic_agents).",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"include_dynamic": {"type": "boolean", "default": true}
				}
			}`),
		},

		// --- Builtin wrappers ---
		// Each delegates 1:1 to the underlying builtin tool's
		// discriminated-op input schema. We don't restate the inner
		// schema here — the loomcycle agent loop already validates it.
		{
			Name:        "memory",
			Description: "Memory tool ops (get/set/delete/list/incr). Pass-through to the underlying Memory builtin.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "channel",
			Description: "Channel tool ops (publish/subscribe/ack/peek/list_channels). Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "agentdef",
			Description: "AgentDef tool ops (create/fork/get/list/promote/retire). Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "skilldef",
			Description: "SkillDef tool ops (create/fork/get/list/promote/retire). Runtime-mutable skill substrate; mirror of AgentDef for skills. Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "evaluation",
			Description: "Evaluation tool ops (submit/get/list_for_run/list_for_def/aggregate). Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "context",
			Description: "Context tool ops (self/tools/doc/permissions/agents/lineage/evaluations/channels/history/help). Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},

		// --- Pause/Resume (v0.8.17 primitives, exposed via Connector in v0.8.18) ---
		{
			Name:        "pause_runtime",
			Description: "Quiesce the runtime. Idempotent tools cancel immediately; non-idempotent + external tools get a grace window (default 30 s) then force-cancel. New /v1/runs return 503 while paused. Returns {status, duration_ms, force_cancelled_count, paused_runs_count, warnings?}. 409 when the runtime is already pausing or paused.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {"timeout_ms": {"type": "integer", "default": 30000}}
			}`),
		},
		{
			Name:        "resume_runtime",
			Description: "Release the runtime quiesce. Each previously-paused run's pause_state flips back to 'running'; the runner goroutines re-enter their loops. Returns {status, resumed_run_count, warnings?}. 409 when the runtime is not paused.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "get_runtime_state",
			Description: "Return the current runtime quiesce state. Returns {status: 'running'|'pausing'|'paused', paused_run_count, snapshots_count}.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},

		// --- Snapshot (v0.8.17 primitives, exposed via Connector in v0.8.18) ---
		{
			Name:        "create_snapshot",
			Description: "Capture running-state into a per-section-semver JSON envelope (agent_defs, agent_def_active, memory, channels, evaluations, paused_runs, optional interaction_history). Returns a SnapshotDescriptor; the envelope is persisted in the snapshots table and retrievable via get_snapshot / export_snapshot.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"include_history": {"type": "boolean"},
					"since_ts":        {"type": "string", "format": "date-time"},
					"description":     {"type": "string"},
					"max_bytes":       {"type": "integer"}
				}
			}`),
		},
		{
			Name:        "list_snapshots",
			Description: "List captured snapshots (most-recent first, capped at 200). Returns metadata only; use get_snapshot / export_snapshot to fetch the JSON envelope.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "get_snapshot",
			Description: "Return the full snapshot envelope including JSON content (v0.8.18+). Distinct from export_snapshot, which is operator-facing 'where did this land on the host' semantics. Returns 404-equivalent error when no snapshot matches.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "export_snapshot",
			Description: "Return the canonical envelope bytes for a snapshot id. Transports that stream large exports (HTTP /v1/_snapshots/{id}/export) write raw_json directly to the response body. Returns 404-equivalent error when no snapshot matches.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "restore_snapshot",
			Description: "Restore from a same-instance snapshot_id OR cross-instance raw_json. Idempotent: ON CONFLICT DO NOTHING per row. Counters reflect rows actually written. paused_runs reference session_ids; restore synthesizes a session row when needed (counted as synthesized_sessions). 422-equivalent error on snapshot version newer than reader supports.",
			InputSchema: rawJSON(`{
				"type": "object",
				"oneOf": [
					{"required": ["snapshot_id"]},
					{"required": ["raw_json"]}
				],
				"properties": {
					"snapshot_id":     {"type": "string"},
					"raw_json":        {"type": "object", "description": "Inline JSON envelope — pass the same JSON object you'd get from get_snapshot.json_content. Per v0.8.18, raw_json is a JSON object on the MCP wire (not a base64 string), so export_snapshot.raw_json → restore_snapshot.raw_json round-trips natively."},
					"include_history": {"type": "boolean"}
				}
			}`),
		},
		{
			Name:        "delete_snapshot",
			Description: "Delete a snapshot. Idempotent — succeeds whether or not the row existed (mirrors HTTP DELETE /v1/_snapshots/{id} = 204).",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		// --- Interruption (v0.8.16) — the 21st meta-tool ---
		{
			Name:        "interruption_resolve",
			Description: "Resolve a pending Interruption.ask from outside the agent loop. Lets an external orchestrator (Claude Code, custom dashboard) act as the human answerer when the operator yaml configures `interruption.backend: mcp_server:...` or when the orchestrator wants to take over the webui default. Writes the answer, wakes the blocked agent loop, publishes _system/interrupts/resolved for downstream consumers. Returns 409-equivalent error on already-resolved / timed-out / cancelled rows.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["run_id", "interrupt_id", "answer"],
				"properties": {
					"run_id":       {"type": "string", "description": "The run that owns the pending interrupt."},
					"interrupt_id": {"type": "string", "description": "The intr_... id surfaced via _system/interrupts/pending or EventInterruptionPending."},
					"kind":         {"type": "string", "enum": ["question"], "description": "Discriminator. v0.8.16 supports only 'question'. Optional; defaults to 'question'."},
					"answer":       {"type": "string", "description": "The human's answer. When the original ask declared options, MUST be one of them (server-side validated)."},
					"resolved_by":  {"type": "string", "description": "Audit attribution for who resolved it (free-form). Defaults to 'mcp' when surfaced via this tool."}
				}
			}`),
		},
		// --- Hook management (hooks-connector series, PR B) ---
		{
			Name:        "register_hook",
			Description: "Register a pre- or post-tool webhook. The callback_url must be an http:// or https:// endpoint the consumer runs — loomcycle POSTs PreHookCall/PostHookCall payloads to it. Returns {id}. Re-registering the same (owner, name) replaces the prior entry with a fresh id (idempotent app-restart contract). Use the id with delete_hook.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["owner", "name", "phase", "callback_url"],
				"properties": {
					"owner":        {"type": "string", "description": "App UID; (owner, name) is the identity tuple."},
					"name":         {"type": "string"},
					"phase":        {"type": "string", "enum": ["pre", "post"]},
					"agents":       {"type": "array", "items": {"type": "string"}, "description": "Agent name globs (exact or 'prefix*'). Empty = match all."},
					"tools":        {"type": "array", "items": {"type": "string"}, "description": "Tool name globs (same syntax). Empty = match all."},
					"callback_url": {"type": "string", "description": "http:// or https:// URL loomcycle POSTs to."},
					"fail_mode":    {"type": "string", "enum": ["open", "closed"], "description": "open (default) = errors pass through; closed = errors fail the tool call."},
					"timeout_ms":   {"type": "integer", "minimum": 0, "description": "Per-call timeout. 0 = registry default (5 s)."}
				}
			}`),
		},
		{
			Name:        "list_hooks",
			Description: "List every currently-registered hook in registration order. Returns {hooks: [Hook, ...]}. In-memory only — empty after a loomcycle restart.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "delete_hook",
			Description: "Delete a hook by id. Returns {deleted: id} on success. Errors with 'not found' text when no hook has that id.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["id"],
				"properties": {"id": {"type": "string"}}
			}`),
		},
	}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }
