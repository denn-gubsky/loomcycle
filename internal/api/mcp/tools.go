package mcp

import (
	"encoding/json"

	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// toolDescriptors returns the v0.8.15 catalogue of 20 MCP tools.
// Each descriptor carries name + description + input schema; the
// schemas are intentionally minimal — full input validation lives
// at the connector layer (or, for builtin wrappers, at the
// underlying tool's discriminated-op schema).
//
// Naming convention: flat `verb_noun` for actions; single-word for
// builtin wrappers (memory, channel, agentdef, evaluation, context)
// whose inner `op` field already discriminates the operation.
func toolDescriptors() []loommcp.ToolDescriptor {
	return []loommcp.ToolDescriptor{
		// --- Run lifecycle ---
		{
			Name:        "spawn_run",
			Description: "Spawn an agent run. Blocks until completion; final text + usage returned. When the session opted into runEvents via initialize.capabilities.loomcycle.runEvents=true, intermediate events stream as notifications/loomcycle/run_event during the call.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["agent", "segments"],
				"properties": {
					"agent":            {"type": "string", "description": "Registered agent name (static or dynamic)."},
					"segments":         {"type": "array",  "description": "Prompt segments (matches /v1/runs segments field)."},
					"session_id":       {"type": "string", "description": "Optional — set to continue an existing session."},
					"tenant_id":        {"type": "string"},
					"user_id":          {"type": "string"},
					"agent_id":         {"type": "string", "description": "Optional caller-supplied tracking handle."},
					"user_tier":        {"type": "string"},
					"user_bearer":      {"type": "string", "description": "Per-run MCP bearer (substituted into ${run.user_bearer} in mcp_servers.*.headers)."},
					"allowed_tools":    {"type": "array", "items": {"type": "string"}},
					"allowed_hosts":    {"type": "array", "items": {"type": "string"}},
					"web_search_filter": {"type": "string", "enum": ["drop", "keep"]}
				}
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
			Name:        "evaluation",
			Description: "Evaluation tool ops (submit/get/list_for_run/list_for_def/aggregate). Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "context",
			Description: "Context tool ops (self/tools/doc/permissions/agents/lineage/evaluations/channels/history/help). Pass-through.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},

		// --- Pause/Resume (PREVIEW in v0.8.15) ---
		{
			Name:        "pause_runtime",
			Description: "PREVIEW (v0.8.15): wire shape stable; real implementation in v0.8.16+. Currently returns placeholder data — does NOT actually pause the runtime.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {"timeout_ms": {"type": "integer", "default": 30000}}
			}`),
		},
		{
			Name:        "resume_runtime",
			Description: "PREVIEW (v0.8.15): wire shape stable; real implementation in v0.8.16+. Currently a no-op.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "get_runtime_state",
			Description: "PREVIEW (v0.8.15): always returns {status: running, feature_status: preview} in v0.8.15.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},

		// --- Snapshot (PREVIEW in v0.8.15) ---
		{
			Name:        "create_snapshot",
			Description: "PREVIEW (v0.8.15): wire shape stable; returns a placeholder snapshot_id with feature_status=preview. Does NOT write a snapshot.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"include_history": {"type": "boolean"},
					"since_ts":        {"type": "string", "format": "date-time"},
					"description":     {"type": "string"}
				}
			}`),
		},
		{
			Name:        "list_snapshots",
			Description: "PREVIEW (v0.8.15): always returns an empty list (mocks don't persist).",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "export_snapshot",
			Description: "PREVIEW (v0.8.15): not implemented; returns a tool error.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "restore_snapshot",
			Description: "PREVIEW (v0.8.15): not implemented; returns a tool error.",
			InputSchema: rawJSON(`{
				"type": "object",
				"oneOf": [
					{"required": ["snapshot_id"]},
					{"required": ["file_path"]}
				],
				"properties": {
					"snapshot_id": {"type": "string"},
					"file_path":   {"type": "string"}
				}
			}`),
		},
		{
			Name:        "delete_snapshot",
			Description: "PREVIEW (v0.8.15): not implemented; returns a tool error.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
	}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }
