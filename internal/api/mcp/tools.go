package mcp

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// toolDescriptors returns the MCP tool catalogue. Count is asserted
// by TestServer_ToolsList in server_test.go — let that test be the
// authoritative source of "how many tools" rather than restating it
// here (the comment would otherwise drift on every new addition).
// Each descriptor carries name + description + input schema. Builtin
// wrappers source their schema from the underlying tool via
// builtinSchema(), so the advertised inputSchema is the tool's real
// discriminated-op schema and can't drift from it; the remaining
// connector-backed descriptors carry hand-written schemas validated at
// the connector layer.
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
					"segments":         {"type": "array",  "description": "Prompt segments — each {role, content:[blocks]}. Typically required for fresh runs; continuations may omit when the caller has nothing new to add.", "items": {"type": "object", "required": ["role", "content"], "properties": {"role": {"type": "string", "enum": ["system", "user"]}, "content": {"type": "array", "items": {"type": "object", "required": ["type"], "properties": {"type": {"type": "string", "enum": ["trusted-text", "untrusted-block", "image"]}, "text": {"type": "string"}, "cacheable": {"type": "boolean"}, "kind": {"type": "string", "description": "untrusted-block source label (e.g. web_content)"}, "media_type": {"type": "string", "enum": ["image/png", "image/jpeg", "image/gif", "image/webp"], "description": "image blocks only (RFC AT); valid only in a user segment"}, "data": {"type": "string", "description": "image blocks only: base64-encoded image bytes, NO data: prefix"}}}}}}},
					"session_id":       {"type": "string", "description": "Set to continue an existing session. When set, agent is ignored."},
					"tenant_id":        {"type": "string"},
					"user_id":          {"type": "string"},
					"agent_id":         {"type": "string", "description": "Optional caller-supplied tracking handle."},
					"user_tier":        {"type": "string"},
					"user_bearer":      {"type": "string", "description": "Per-run MCP bearer (substituted into ${run.user_bearer} in mcp_servers.*.headers)."},
					"user_credentials": {"type": "object", "additionalProperties": {"type": "string"}, "description": "v1.x RFC F per-tool named credentials map. Keys [a-zA-Z0-9_-]{1,64}; values arbitrary strings. Substituted into ${run.credentials.<name>} in mcp_servers.*.headers. Coexists with user_bearer (legacy promotes to user_credentials.default for back-compat)."},
					"tools":    {"type": "array", "items": {"type": "string"}},
					"allowed_hosts":    {"type": "array", "items": {"type": "string"}, "description": "OMIT for no narrowing (operator's static allowlist applies). Pass empty array [] to DENY ALL outbound HTTP. Pass non-empty array to intersect with operator's list."},
					"web_search_filter": {"type": "string", "enum": ["drop", "keep"]},
					"parent_context":   {"type": "object", "description": "v0.12.x opaque caller-tracking lineage carried verbatim, inherited by every sub-agent, and echoed on the per-agent report surfaces so a consumer can attribute a child sub-agent's usage to the user-initiated request.", "properties": {"root_agent_run_id": {"type": "string"}, "function_key": {"type": "string"}, "tier_at_run": {"type": "string"}}},
					"timeout_ms":       {"type": "integer", "minimum": 1, "description": "Optional transport timeout (RFC P): max milliseconds this spawn_run call may block before loomcycle cancels the run and returns status:\"timeout\" instead of hanging. Narrows the operator default (LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS) — it can shorten but not exceed it. Omit to block until the run finishes on its own run_timeout_seconds budget. This is a transport bound, NOT the run's wall-clock budget."},
					"compaction":       {"type": "object", "description": "Optional per-run context-compaction override, merged per-field over the agent's own block. Trigger compaction mid-run with the compact_run tool.", "properties": {"enabled": {"type": "boolean", "description": "Turn AUTO-compaction on for this run."}, "target_percentage": {"type": "integer", "minimum": 10, "maximum": 50, "description": "Summary aims for ~N% of the compacted span (default 10)."}, "keep_last_n": {"type": "integer", "minimum": 0, "description": "Keep the last N messages verbatim (default 4; 0 = summarize all)."}, "keep_first": {"type": "boolean", "description": "Pin the first user message (the task) verbatim (default true)."}, "autocompact_at_pct": {"type": "integer", "minimum": 50, "maximum": 95, "description": "Auto-compact when used/window ≥ N% (default 80; only when enabled + the provider reports a window)."}, "model": {"type": "string", "description": "Optional cheaper/faster summary model served by the same provider."}}}
				},
				"anyOf": [
					{"required": ["agent"]},
					{"required": ["session_id"]}
				]
			}`),
		},
		{
			Name:        "spawn_runs",
			Description: "RFC Y external fan-out: spawn up to 32 agent runs concurrently in ONE call (server-side, bounded by the per-user admission gate) and block until all settle, returning a combined index-aligned envelope. A per-child failure is captured in that child's result and never fails the batch. Prefer this over firing N parallel spawn_run calls, which serialize over a single MCP connection. Each child is a FRESH run (no session continuation). mode \"detach\" (async run handles) is reserved for a future release and rejected today.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["spawns"],
				"properties": {
					"spawns": {
						"type": "array",
						"minItems": 1,
						"maxItems": 32,
						"description": "The child runs to fan out (each a fresh run; session_id is ignored).",
						"items": {
							"type": "object",
							"required": ["agent"],
							"properties": {
								"agent":            {"type": "string", "description": "Registered agent name."},
								"segments":         {"type": "array", "description": "Prompt segments — each {role, content:[blocks]}.", "items": {"type": "object", "required": ["role", "content"], "properties": {"role": {"type": "string", "enum": ["system", "user"]}, "content": {"type": "array", "items": {"type": "object", "required": ["type"], "properties": {"type": {"type": "string", "enum": ["trusted-text", "untrusted-block", "image"]}, "text": {"type": "string"}, "cacheable": {"type": "boolean"}, "kind": {"type": "string", "description": "untrusted-block source label"}, "media_type": {"type": "string", "enum": ["image/png", "image/jpeg", "image/gif", "image/webp"], "description": "image blocks only (RFC AT)"}, "data": {"type": "string", "description": "image blocks only: base64 image bytes, NO data: prefix"}}}}}}},
								"tenant_id":        {"type": "string"},
								"user_id":          {"type": "string"},
								"agent_id":         {"type": "string", "description": "Optional caller-supplied tracking handle."},
								"user_tier":        {"type": "string"},
								"user_bearer":      {"type": "string"},
								"user_credentials": {"type": "object", "additionalProperties": {"type": "string"}},
								"tools":    {"type": "array", "items": {"type": "string"}},
								"allowed_hosts":    {"type": "array", "items": {"type": "string"}, "description": "OMIT for no narrowing; [] denies all outbound HTTP; non-empty intersects the operator list."},
								"web_search_filter": {"type": "string", "enum": ["drop", "keep"]},
								"parent_context":   {"type": "object", "properties": {"root_agent_run_id": {"type": "string"}, "function_key": {"type": "string"}, "tier_at_run": {"type": "string"}}, "description": "Set a shared root_agent_run_id across the spawns to group the batch for cost attribution."}
							}
						}
					},
					"mode":       {"type": "string", "enum": ["join"], "description": "Only \"join\" (default) is supported today: block until all children settle. \"detach\" awaits a future async-handle release."},
					"timeout_ms": {"type": "integer", "minimum": 1, "description": "Optional join deadline: a child still running when it elapses is cancelled and reported with a cancelled status in-envelope."}
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
			Name:        "compact_run",
			Description: "Compact a run's conversation: summarize the history to free context and continue from the summary. Targets the run by agent_id (resolved to its run_id). A live run must be PARKED (awaiting input) — a mid-turn run is refused. Returns {compacted, before_tokens, after_tokens, applied}, where applied is \"live\" (pushed to the running loop), \"marker\" (persisted for a terminal run's next continuation), or \"noop\" (too short to compact). Honors the agent's compaction settings (keep_last_n / keep_first / target_percentage / summary model).",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["agent_id"],
				"properties": {
					"agent_id": {"type": "string"},
					"reason":   {"type": "string", "description": "Optional free-text note (audit only)."}
				}
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
			Description: "Register a dynamic agent at runtime. Survives until TTL expires or unregister_agent is called. Bash/Write/Edit are stripped from tools unless the operator set LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["name", "system_prompt", "tools"],
				"properties": {
					"name":          {"type": "string", "pattern": "^[A-Za-z0-9_-]{1,64}$"},
					"system_prompt": {"type": "string", "maxLength": 65536},
					"tools": {"type": "array", "items": {"type": "string"}, "minItems": 1},
					"tier":          {"type": "string"},
					"provider":      {"type": "string"},
					"model":         {"type": "string"},
					"effort":        {"type": "string", "enum": ["minimal", "low", "medium", "high"]},
					"max_tokens":    {"type": "integer", "minimum": 1},
					"memory_scopes": {"type": "array", "items": {"type": "string"}, "description": "Memory tool scope gate: [agent] and/or [user]. Required for the Memory tool to work."},
					"evaluation_scopes": {"type": "array", "items": {"type": "string"}, "description": "Evaluation tool scope gate, e.g. [submit_self, read_any]."},
					"max_iterations": {"type": "integer", "minimum": 1, "description": "Cap on provider calls per run. 0 = loop default (16)."},
					"channels": {"type": "object", "description": "Channel tool ACL.", "properties": {"publish": {"type": "array", "items": {"type": "string"}}, "subscribe": {"type": "array", "items": {"type": "string"}}}},
					"interruption": {"type": "object", "description": "Interruption tool gate. enabled MUST be true for the tool to work.", "properties": {"enabled": {"type": "boolean"}, "kinds": {"type": "array", "items": {"type": "string"}}, "max_pending": {"type": "integer"}}},
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
		// Each delegates 1:1 to the underlying builtin tool, and advertises
		// that tool's own discriminated-op input schema via builtinSchema()
		// — clients see the real `op` enum + properties, and the schema
		// can't drift from the validation the agent loop applies.
		{
			Name:        "memory",
			Description: "Memory tool ops. Families: key/value (get/set/delete/list/incr/merge/append_dedupe/bounded_list/search); memory-layer (add/recall — add enqueues for background consolidation, recall needs an embedder + vector store); SQL (sql_query/sql_exec/sql_begin/sql_commit/sql_rollback — a per-scope SQL database, gated separately by sql_scopes). Pass-through to the underlying Memory builtin; see the inputSchema's op enum.",
			InputSchema: builtinSchema("memory"),
		},
		{
			Name:        "channel",
			Description: "Channel tool ops (publish/subscribe/ack/peek/list_channels/await/broadcast). await = multi-channel fan-in barrier (any/all/at_least N or timeout; non-committing); broadcast = symmetric fan-out (one payload → N channels, atomic ACL pre-flight). Pass-through.",
			InputSchema: builtinSchema("channel"),
		},
		{
			Name:        "channeldef",
			Description: "Channel admin CRUD (create/update/delete/purge) for the channel substrate — the MCP twin of the REST /v1/_channels surface (F20). yaml-declared channels are immutable for create/update/delete (returns channel_yaml_immutable), but purge — which clears buffered messages without touching the definition — is allowed on ANY channel, yaml included.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["op", "name"],
				"properties": {
					"op":           {"type": "string", "enum": ["create", "update", "delete", "purge"], "description": "Which admin operation to perform. purge clears buffered messages (allowed on yaml channels); create/update/delete mutate the definition (runtime channels only)."},
					"name":         {"type": "string", "description": "Channel name (required for all ops)."},
					"description":  {"type": "string"},
					"scope":        {"type": "string", "enum": ["global", "agent", "user"], "description": "create only. Default global."},
					"semantic":     {"type": "string", "enum": ["queue", "topic"], "description": "Default queue."},
					"default_ttl":  {"type": "integer", "description": "Per-message TTL seconds. 0 = no TTL."},
					"max_messages": {"type": "integer", "description": "Bounded-queue cap. 0 = unbounded."},
					"publisher":    {"type": "string", "description": "create only. Free-form attribution."}
				}
			}`),
		},
		{
			Name:        "agentdef",
			Description: "AgentDef tool ops (create/fork/get/list/promote/retire). Pass-through.",
			InputSchema: builtinSchema("agentdef"),
		},
		{
			Name:        "skilldef",
			Description: "SkillDef tool ops (create/fork/get/list/promote/retire). Runtime-mutable skill substrate; mirror of AgentDef for skills. Pass-through.",
			InputSchema: builtinSchema("skilldef"),
		},
		{
			Name:        "teamdef",
			Description: "TeamDef tool ops (create/fork/get/list/promote/retire/verify). RFC AP team-workflow substrate — a state-machine graph (states + transitions) validated before any write; invalid graphs are refused. Colours are excluded from the content hash. Tenant-confined. Pass-through.",
			InputSchema: builtinSchema("teamdef"),
		},
		{
			Name:        "mcpserverdef",
			Description: "MCPServerDef tool ops (create/fork/get/list/promote/retire/rediscover/verify). v0.9.x dynamic MCP server registration. Operator-admin-only — register HTTP / Streamable-HTTP MCP servers at runtime; stdio stays yaml. URL hostname must be in LOOMCYCLE_HTTP_HOST_ALLOWLIST. Pass-through.",
			InputSchema: builtinSchema("mcpserverdef"),
		},
		{
			Name:        "scheduledef",
			Description: "ScheduleDef tool ops (create/fork/get/list/retire). v1.x RFC E scheduled-runs substrate. Operator-admin-only — author + fork per-user schedules at runtime. Forks auto-promote by default (schedule versioning model differs from agent/skill where promote is a separate step). Pass-through.",
			InputSchema: builtinSchema("scheduledef"),
		},
		{
			Name:        "a2aservercarddef",
			Description: "A2AServerCardDef tool ops (create/fork/get/list/retire). v1.x RFC G A2A-server-card substrate. Operator-admin-only — author + fork A2A server cards at runtime. Pass-through.",
			InputSchema: builtinSchema("a2aservercarddef"),
		},
		{
			Name:        "a2aagentdef",
			Description: "A2AAgentDef tool ops (create/fork/get/list/retire). v1.x RFC G A2A-agent substrate. Operator-admin-only — author + fork A2A agents at runtime. Pass-through.",
			InputSchema: builtinSchema("a2aagentdef"),
		},
		{
			Name:        "webhookdef",
			Description: "WebhookDef tool ops (create/fork/get/list/retire). v1.x RFC H inbound-webhook substrate. Operator-admin-only — author + fork inbound webhook definitions at runtime. Static webhooks.<name>: yaml entries stay immutable ground truth; this produces the derived layer. Pass-through.",
			InputSchema: builtinSchema("webhookdef"),
		},
		{
			Name:        "memorybackenddef",
			Description: "MemoryBackendDef tool ops (create/fork/get/list/retire). RFC I MR-3a memory-backend substrate. Operator-admin-only — author + fork named memory backend definitions at runtime. Static memory_backends.<name>: yaml entries stay immutable ground truth; this produces the derived layer. Pass-through.",
			InputSchema: builtinSchema("memorybackenddef"),
		},
		{
			Name:        "operatortokendef",
			Description: "OperatorTokenDef tool ops (create/rotate/retire/get/list). RFC L OSS multi-tenant authorization. Operator-admin-only — mint, rotate, and retire bearer tokens each bound to an authoritative principal {tenant_id, subject, allowed_scopes}. The token plaintext is shown ONCE on create/rotate. Pass-through.",
			InputSchema: builtinSchema("operatortokendef"),
		},
		{
			Name:        "volumedef",
			Description: "VolumeDef tool ops (create/get/list/delete/purge). RFC AH dynamic filesystem-volume substrate. Tenant-confined — provision + manage CONFINED per-tenant volumes at runtime. Volumes are created by NAME + MODE only; the runtime derives the path inside an operator-blessed parent (dynamic_root/<tenant>/<name>) — you never supply a host path. delete unmaps (keeps files); purge removes the row AND the directory tree. Pass-through.",
			InputSchema: builtinSchema("volumedef"),
		},
		{
			Name:        "credentialdef",
			Description: "CredentialDef tool ops (create/get/list/delete). RFC AR secure per-tenant credential store — named API secrets encrypted at rest, scoped tenant|user|agent, referenced elsewhere as $cred:<name> and bound server-side (the model never sees the value). user scope keys on YOUR subject (per-user tokens, e.g. a personal Telegram/Slack bot token); tenant scope is shared. get/list return metadata only, never the secret. Requires LOOMCYCLE_SECRET_KEY. Tenant-confined. Pass-through.",
			InputSchema: builtinSchema("credentialdef"),
		},
		{
			Name:        "path",
			Description: "Path tool ops (resolve/ls/stat/mkdir/mv/rm). RFC AL Unix-like VFS over the dirents table — address Memory entries, Volume mounts, and Documents by human-readable paths (e.g. /docs/launch). Scope-aware (agent/user/tenant, default agent) and tenant-isolated; segments are [a-zA-Z0-9._-], no \"..\". mkdir is a no-op (dirs are implicit). Pass-through.",
			InputSchema: builtinSchema("path"),
		},
		{
			Name:        "document",
			Description: "Document tool ops (create_document/get_document/delete_document, create_chunk/get_chunk/update_chunk/delete_chunk/move_chunk, link_chunks/unlink_chunks, query_chunks, define_type/list_types). RFC AK chunked-graph documents — each chunk is a first-class unit (UUID, hierarchy, type, fields, edges, Markdown body). Requires SQL Memory. Scope agent/user (tenant deferred); tenant-isolated. Pass-through.",
			InputSchema: builtinSchema("document"),
		},
		{
			Name:        "history",
			Description: "History tool ops (list/get/search/rename/annotate/pin/archive). RFC BE — browse, search, and annotate past chats (a chat = a conversation session). Owner-scope-aware (self/user/tenant/global; global = admin only) with the owner resolved server-side from the run identity; cross-scope reads fold to an opaque not-found. Per-chat token/cost/run-count stats included. Pass-through.",
			InputSchema: builtinSchema("history"),
		},
		{
			Name:        "evaluation",
			Description: "Evaluation tool ops (submit/get/list_for_run/list_for_def/aggregate). Pass-through.",
			InputSchema: builtinSchema("evaluation"),
		},
		{
			Name:        "context",
			Description: "Context tool ops (self/tools/doc/permissions/agents/lineage/evaluations/channels/help/time). Pass-through.",
			InputSchema: builtinSchema("context"),
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
		{
			Name:        "resolve_probe",
			Description: "Trigger an immediate re-probe of every configured provider and return the refreshed availability matrix {generated_at, providers: {<id>: {excluded, reachable, models, last_check, last_error}}}. The operator escape hatch when a transient outage stalls every provider. Mirrors POST /v1/_resolve/probe.",
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
		// v0.9.x n8n RFC Phase 0 — channel listing + run-state streaming.
		{
			Name:        "list_channels",
			Description: "List every operator-declared channel with aggregate runtime stats (message_count, oldest_visible_at, newest_visible_at). Mirrors GET /v1/_channels. No arguments.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "stream_user_run_states",
			Description: "Subscribe to run state transitions for one user_id. Returns {events: [RunStateEvent...], count}. When the session opted into capabilities.loomcycle.runEvents=true, each matching event also arrives as a notifications/loomcycle/run_state notification and the response carries an empty events array (count only). Filters: statuses (e.g. ['completed','failed']) and agent (exact name). max_events caps the response at N events; timeout_ms bounds the blocking wait.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["user_id"],
				"properties": {
					"user_id":    {"type": "string"},
					"statuses":   {"type": "array", "items": {"type": "string"}},
					"agent":      {"type": "string"},
					"max_events": {"type": "integer", "minimum": 1, "default": 16},
					"timeout_ms": {"type": "integer", "minimum": 100, "default": 30000}
				}
			}`),
		},
		// v0.9.x Channel CRUD — admin + per-user publish / subscribe /
		// peek / ack. Mirrors the HTTP routes; scope + scope_id select
		// the cursor namespace.
		{
			Name:        "publish_channel",
			Description: "Publish a message to an operator-declared channel. scope is 'global' (admin surface) or 'user' (per-user). When scope is 'user', scope_id is REQUIRED and must be the user_id; when scope is 'global', scope_id is ignored. payload is the JSON object/array/value to deliver. deliver_at (RFC3339Nano) defers the publish; subscribers wake at visible_at. Returns {msg_id, channel, created_at, visible_at?}.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["channel", "scope", "payload"],
				"properties": {
					"channel":    {"type": "string"},
					"scope":      {"type": "string", "enum": ["global", "user"]},
					"scope_id":   {"type": "string", "description": "REQUIRED when scope=user (must be the user_id); ignored when scope=global."},
					"payload":    {},
					"deliver_at": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "subscribe_channel",
			Description: "Read the next batch of messages from a channel. Single-round-trip long-poll: returns immediately if messages are present, otherwise waits up to wait_ms (capped at operator's ChannelsLongPollCapMS). Auto-commits the cursor on non-empty batch (at-most-once shape). scope is 'global' or 'user'; scope_id is REQUIRED when scope=user. Returns {channel, messages: [{id, value, published_at}...], next_cursor}.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["channel", "scope"],
				"properties": {
					"channel":      {"type": "string"},
					"scope":        {"type": "string", "enum": ["global", "user"]},
					"scope_id":     {"type": "string", "description": "REQUIRED when scope=user (must be the user_id); ignored when scope=global."},
					"from_cursor":  {"type": "string"},
					"max_messages": {"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
					"wait_ms":      {"type": "integer", "minimum": 0, "default": 0}
				}
			}`),
		},
		{
			Name:        "peek_channel",
			Description: "Non-destructive read — never advances the committed cursor. Useful for at-least-once processing patterns (peek + explicit ack after durable processing). scope is 'global' or 'user'; scope_id is REQUIRED when scope=user. Returns {channel, messages: [...]}.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["channel", "scope"],
				"properties": {
					"channel":      {"type": "string"},
					"scope":        {"type": "string", "enum": ["global", "user"]},
					"scope_id":     {"type": "string", "description": "REQUIRED when scope=user (must be the user_id); ignored when scope=global."},
					"from_cursor":  {"type": "string"},
					"max_messages": {"type": "integer", "minimum": 1, "maximum": 100, "default": 10}
				}
			}`),
		},
		{
			Name:        "ack_channel",
			Description: "Advance the committed cursor for a (channel, scope, scope_id) tuple. Cursor must be monotonically forward — older cursors return a channel_cursor_regression error. scope is 'global' or 'user'; scope_id is REQUIRED when scope=user. Returns {ok: true}.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["channel", "scope", "cursor"],
				"properties": {
					"channel":  {"type": "string"},
					"scope":    {"type": "string", "enum": ["global", "user"]},
					"scope_id": {"type": "string", "description": "REQUIRED when scope=user (must be the user_id); ignored when scope=global."},
					"cursor":   {"type": "string"}
				}
			}`),
		},
	}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

// MetaToolCount is the number of meta-tools the LoomCycle MCP server exposes.
// The CLI help (internal/cli) prints this so the advertised count is sourced
// from the registry and can never drift — the static "33" in the help text
// went stale once the registry had grown to 40.
func MetaToolCount() int { return len(toolDescriptors()) }

// builtinSchema returns the canonical input schema for an op-dispatched
// builtin wrapper (memory, channel, agentdef, …), sourced from the
// builtin tool itself so the advertised MCP schema can't drift from the
// tool's real validation. Falls back to a bare object if the wrapper has
// no registered schema — a programmer error caught by
// TestBuiltinWrapperSchemas_CoverAllWrappers.
func builtinSchema(name string) json.RawMessage {
	if s, ok := builtin.MCPWrapperInputSchema(name); ok {
		return s
	}
	return rawJSON(`{"type": "object"}`)
}
