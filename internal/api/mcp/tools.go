package mcp

import (
	"encoding/json"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// spawnPerRunProps is the JSON-schema fragment for every per-run knob the
// shared spawn shape (connector.SpawnRunRequest) accepts. It is spliced into
// BOTH spawn_run and spawn_runs.
//
// It is one declaration because it used to be two. The batch tool advertised
// none of these while accepting all of them — both handlers unmarshal the same
// struct, so a caller who already knew the names got them honoured and an agent
// reading the schema had no way to learn they existed. For a model-facing tool
// that is indistinguishable from not having the feature.
//
// Flat indentation is deliberate: it is spliced in at two different nesting
// depths and cannot align with both. Carries no leading or trailing comma.
const spawnPerRunProps = `
	"metadata": {"type": "object", "description": "Non-secret structured metadata handed to the run (repo name, review policy, and the like). A code-js agent reads it as input.metadata; an LLM agent receives it as a trusted prompt block. NOT a place for credentials — those are user_credentials."},
	"sampling": {"type": "object", "description": "Optional per-run LLM sampling override, merged per-field over the agent's own block (unset fields inherit; omit to inherit entirely). temperature 0 is deterministic, which is not the same as unset. Each provider maps only what it supports.", "properties": {"temperature": {"type": "number", "minimum": 0, "maximum": 2}, "top_p": {"type": "number", "minimum": 0, "maximum": 1}, "top_k": {"type": "integer", "minimum": 1}, "frequency_penalty": {"type": "number", "minimum": -2, "maximum": 2}, "presence_penalty": {"type": "number", "minimum": -2, "maximum": 2}, "seed": {"type": "integer"}, "stop": {"type": "array", "items": {"type": "string"}}}},
	"tool_choice": {"type": "object", "description": "Optional per-run tool choice: whether and which tool the model must call, and for how many calls. Replaces the agent's own choice whole. until defaults to first_call; always is refused with required or tool, because the model could then never give a final answer. A model that cannot enforce it still runs, and the run reports what was not enforced.", "properties": {"mode": {"type": "string", "enum": ["auto", "none", "required", "tool"]}, "name": {"type": "string", "description": "The tool to call; only with mode tool."}, "until": {"type": "string", "enum": ["first_call", "until_called", "always"]}}, "required": ["mode"]},
	"output_format": {"type": "object", "description": "Optional per-run answer schema: the run's final answer is held to this JSON Schema (root type object) where the model supports it, and returned parsed as result.structured. Replaces the agent's own whole. A model that cannot enforce it still runs, and the run reports it.", "properties": {"type": {"type": "string", "enum": ["json_schema"], "description": "The only kind, and the default."}, "name": {"type": "string", "description": "Schema label, letters/digits/_/- up to 64 (default output)."}, "schema": {"type": "object", "description": "The JSON Schema; root type object."}}, "required": ["schema"]},
	"compaction":       {"type": "object", "description": "Optional per-run context-compaction override, merged per-field over the agent's own block. Trigger compaction mid-run with the compact_run tool.", "properties": {"enabled": {"type": "boolean", "description": "Turn AUTO-compaction on for this run."}, "target_percentage": {"type": "integer", "minimum": 10, "maximum": 50, "description": "Summary aims for ~N% of the compacted span (default 10)."}, "keep_last_n": {"type": "integer", "minimum": 0, "description": "Keep the last N messages verbatim (default 4; 0 = summarize all)."}, "keep_first": {"type": "boolean", "description": "Pin the first user message (the task) verbatim (default true)."}, "autocompact_at_pct": {"type": "integer", "minimum": 50, "maximum": 95, "description": "Auto-compact when used/window ≥ N% (default 80; only when enabled + the provider reports a window)."}, "model": {"type": "string", "description": "Optional cheaper/faster summary model served by the same provider."}}},
	"context":          {"type": "object", "description": "Optional per-run layered-context / retention override, merged per-field over the agent's own context block.", "properties": {"mode": {"type": "string", "enum": ["append", "recap", "stateful", "auto"], "description": "Retention strategy: append (default) | recap (L1 reasoning-recap) | stateful (L2 structured state) | auto (tier-routed: local to recap, frontier to stateful)."}, "keep_last_n": {"type": "integer", "minimum": 0, "description": "Recent tool_use/tool_result pairs kept verbatim in recap mode (default 6)."}, "reasoning": {"type": "string", "enum": ["recap", "drop", "keep"], "description": "R-layer policy in recap mode (default recap)."}, "recap_max_chars": {"type": "integer", "minimum": 0, "description": "Bound on the running recap note (default 512)."}, "model": {"type": "string", "description": "Run the recap call on a different model served by the SAME provider (default: the run's own model). For a cheap non-thinking summarizer beside a thinking chat model."}, "autorecap_at_pct": {"type": "integer", "minimum": 50, "maximum": 95, "description": "Recap when used/window over N pct (default 80)."}, "state_schema": {"type": "object", "description": "JSON-Schema (object with typed properties) each state patch is validated against, in stateful mode."}, "on_invalid_patch": {"type": "string", "enum": ["retry", "fail"], "description": "Stateful policy on an invalid patch (default retry)."}, "max_patch_retries": {"type": "integer", "minimum": 0, "description": "Bounded rollback-retry on an invalid patch (default 2)."}, "recall": {"type": "boolean", "description": "Embed each evicted span so a later Recall can fetch back what distillation dropped."}, "harvest_to_memory": {"type": "boolean", "description": "Bank each evicted span to persistent memory for the consolidator."}}},
	"max_context_tokens": {"type": "integer", "minimum": 1, "description": "Optional per-run context-WINDOW override in tokens (wins over the agent's own max_context_tokens; omit to inherit it, which itself defers to the provider/driver default). Distinct from a model's output cap; primarily for local inference (Ollama num_ctx)."},
	"model": {"type": "string", "description": "Run this child on a specific model. Must be one the agent's definition already allows; naming a model pins it, so the tier stops choosing."},
	"provider": {"type": "string", "description": "Run this child on a specific provider. Must be one its definition already allows; this narrows the tier's cascade to that vendor rather than replacing it."},
	"tier": {"type": "string", "description": "Route this child through a different configured tier."},
	"effort": {"type": "string", "enum": ["low", "medium", "high"], "description": "Reasoning-effort hint for this child. Any other value is refused rather than ignored."},
	"max_tokens": {"type": "integer", "minimum": 1, "description": "Per-reply output cap for this child. May be raised above its definition's."},
	"max_iterations": {"type": "integer", "minimum": 1, "description": "Loop bound for this child. May be raised above its definition's."},
	"unbounded_iterations": {"type": "boolean", "description": "Lift or restore this child's loop bound. false bounds an otherwise-unbounded agent for this run only."},
	"max_concurrent_children": {"type": "integer", "minimum": 1, "description": "How wide this child may itself fan out. May only be LOWERED below what its definition allows; raising it is refused, because this is the only bound on fan-out there is."},
	"retry_attempts": {"type": "integer", "minimum": 0, "description": "How many times to retry the same provider before falling back. 0 disables retrying for this child."},
	"memory_inject_max_tokens": {"type": "integer", "minimum": 0, "description": "Token budget for memory injected into this child's prompt. 0 injects none."},
	"memory_index_max_bytes": {"type": "integer", "minimum": 0, "description": "Byte budget for this child's memory index. 0 omits it."},
	"inject_tool_guide": {"type": "boolean", "description": "Whether to inject the generated tool guide into this child's prompt."},
	"interactive": {"type": "boolean", "description": "Park this child at its turn boundaries instead of finishing, so an operator can steer it. Also settable on a run that is ALREADY GOING \u2014 that is the point, since nobody knows at start that they will need to correct it. false releases a run that was started interactive."},
	"review": {"type": "boolean", "description": "Hold this child's finished answer for an operator's verdict instead of completing. The call returns once the answer is approved, or when the run is rejected. Also settable on a run that is already going; false releases a held run as approved."},
	"interruption": {"type": "object", "description": "Let this child ASK a human a question, overriding what its definition allows. It blocks and waits for a person, so the cost is the run stopping until someone answers \u2014 bounded by the run timeout and the interruption's own.", "properties": {"enabled": {"type": "boolean"}, "kinds": {"type": "array", "items": {"type": "string"}}, "max_pending": {"type": "integer", "minimum": 0}}}`

// retuneProps is the JSON-schema fragment for a RETUNE, which accepts a
// strictly smaller set than a spawn: the twelve per-run overrides plus
// interactive, interruption and review, and NOT sampling / compaction / context /
// max_context_tokens / metadata.
//
// Spliced from spawnPerRunProps' own entries rather than rewritten, so the two
// tools cannot end up describing the same field differently — but filtered,
// because advertising a field the endpoint ignores is the same defect as hiding
// one that works, pointed the other way.
var retuneProps = filterProps(spawnPerRunProps, []string{
	"model", "provider", "tier", "effort",
	"max_tokens", "max_iterations", "unbounded_iterations", "max_concurrent_children",
	"retry_attempts", "memory_inject_max_tokens", "memory_index_max_bytes",
	"inject_tool_guide", "interactive", "interruption", "review",
})

// filterProps keeps the named entries of a schema-property fragment, in the
// fragment's own order. Each entry is one line, which is what makes this safe;
// TestRetuneSchema_MatchesWhatTheEndpointAccepts fails if that stops being true.
func filterProps(fragment string, keep []string) string {
	want := make(map[string]bool, len(keep))
	for _, k := range keep {
		want[k] = true
	}
	var out []string
	for _, line := range strings.Split(fragment, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, `"`) {
			continue
		}
		name := strings.SplitN(strings.TrimPrefix(t, `"`), `"`, 2)[0]
		if want[name] {
			out = append(out, "\t"+strings.TrimSuffix(t, ","))
		}
	}
	return strings.Join(out, ",\n")
}

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
			Description: "Run ONE agent to completion and return its final text plus token usage. BLOCKS for the whole run. Supply exactly one of `agent` (a fresh run against a registered agent) or `session_id` (continue an existing session); supplying both, or neither, is refused. Optional per-run overrides \u2014 `sampling`, `compaction`, `context` and the routing/budget knobs (`model`, `provider`, `tier`, `effort`, the max_* caps) \u2014 apply to this run only and select WITHIN what the agent's definition already allows; they cannot widen it. When the session opted in via initialize.capabilities.loomcycle.runEvents=true, intermediate events arrive as notifications/loomcycle/run_event while the call is open. Do NOT call it N times in parallel to fan out \u2014 one MCP connection serializes them, so they run one after another; spawn_runs does N concurrently server-side. Do NOT use it to resume a PARKED run awaiting input \u2014 that is a session continuation via `session_id`, not a fresh `agent`. A long run holds the connection for its duration; there is no detached form of this tool.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"agent":            {"type": "string", "description": "Registered agent name. Required for fresh runs; ignored for continuations (session's stored agent is authoritative)."},
					"segments":         {"type": "array",  "description": "Prompt segments — each {role, content:[blocks]}. Typically required for fresh runs; continuations may omit when the caller has nothing new to add.", "items": {"type": "object", "required": ["role", "content"], "properties": {"role": {"type": "string", "enum": ["system", "user"]}, "content": {"type": "array", "items": {"type": "object", "required": ["type"], "properties": {"type": {"type": "string", "enum": ["trusted-text", "untrusted-block", "image"]}, "text": {"type": "string"}, "cacheable": {"type": "boolean"}, "kind": {"type": "string", "description": "untrusted-block source label (e.g. web_content)"}, "media_type": {"type": "string", "enum": ["image/png", "image/jpeg", "image/gif", "image/webp"], "description": "image blocks only; valid only in a user segment"}, "data": {"type": "string", "description": "image blocks only: base64-encoded image bytes, NO data: prefix"}}}}}}},
					"session_id":       {"type": "string", "description": "Set to continue an existing session. When set, agent is ignored."},
					"tenant_id":        {"type": "string"},
					"user_id":          {"type": "string"},
					"agent_id":         {"type": "string", "description": "Optional caller-supplied tracking handle."},
					"user_tier":        {"type": "string"},
					"user_bearer":      {"type": "string", "description": "Per-run MCP bearer (substituted into ${run.user_bearer} in mcp_servers.*.headers)."},
					"user_credentials": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Per-tool named credentials map. Keys [a-zA-Z0-9_-]{1,64}; values arbitrary strings. Substituted into ${run.credentials.<name>} in mcp_servers.*.headers. Coexists with user_bearer (legacy promotes to user_credentials.default for back-compat)."},
					"tools":    {"type": "array", "items": {"type": "string"}},
					"allowed_hosts":    {"type": "array", "items": {"type": "string"}, "description": "OMIT for no narrowing (operator's static allowlist applies). Pass empty array [] to DENY ALL outbound HTTP. Pass non-empty array to intersect with operator's list."},
					"web_search_filter": {"type": "string", "enum": ["drop", "keep"]},
					"parent_context":   {"type": "object", "description": "v0.12.x opaque caller-tracking lineage carried verbatim, inherited by every sub-agent, and echoed on the per-agent report surfaces so a consumer can attribute a child sub-agent's usage to the user-initiated request.", "properties": {"root_agent_run_id": {"type": "string"}, "function_key": {"type": "string"}, "tier_at_run": {"type": "string"}}},
					"timeout_ms":       {"type": "integer", "minimum": 1, "description": "Optional transport timeout: max milliseconds this spawn_run call may block before loomcycle cancels the run and returns status:\"timeout\" instead of hanging. Narrows the operator default (LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS) — it can shorten but not exceed it. Omit to block until the run finishes on its own run_timeout_seconds budget. This is a transport bound, NOT the run's wall-clock budget."},
					` + spawnPerRunProps + `
				},
				"anyOf": [
					{"required": ["agent"]},
					{"required": ["session_id"]}
				]
			}`),
		},
		{
			Name:        "spawn_runs",
			Description: "Run up to 32 agents CONCURRENTLY in one call and block until all of them settle, returning one index-aligned envelope \u2014 result[i] belongs to spawns[i]. Each child is a FRESH run; there is no session continuation here. Each child spec takes the same per-run overrides spawn_run does \u2014 sampling, compaction, context and the routing/budget knobs \u2014 so one batch can fan the same agent out across different models or budgets. Concurrency is server-side and still bounded by the per-user admission gate. A child that fails is reported in its own slot and never fails the batch, so always read per-child status rather than assuming success. USE THIS for any fan-out: N parallel spawn_run calls serialize over the single MCP connection and will be slower for no benefit. Do NOT use it to continue sessions, and do NOT pass mode 'detach' \u2014 async handles are reserved for a future release and rejected today. Over 32 spawns is refused rather than truncated.",
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
								"segments":         {"type": "array", "description": "Prompt segments — each {role, content:[blocks]}.", "items": {"type": "object", "required": ["role", "content"], "properties": {"role": {"type": "string", "enum": ["system", "user"]}, "content": {"type": "array", "items": {"type": "object", "required": ["type"], "properties": {"type": {"type": "string", "enum": ["trusted-text", "untrusted-block", "image"]}, "text": {"type": "string"}, "cacheable": {"type": "boolean"}, "kind": {"type": "string", "description": "untrusted-block source label"}, "media_type": {"type": "string", "enum": ["image/png", "image/jpeg", "image/gif", "image/webp"], "description": "image blocks only"}, "data": {"type": "string", "description": "image blocks only: base64 image bytes, NO data: prefix"}}}}}}},
								"tenant_id":        {"type": "string"},
								"user_id":          {"type": "string"},
								"agent_id":         {"type": "string", "description": "Optional caller-supplied tracking handle."},
								"user_tier":        {"type": "string"},
								"user_bearer":      {"type": "string"},
								"user_credentials": {"type": "object", "additionalProperties": {"type": "string"}},
								"tools":    {"type": "array", "items": {"type": "string"}},
								"allowed_hosts":    {"type": "array", "items": {"type": "string"}, "description": "OMIT for no narrowing; [] denies all outbound HTTP; non-empty intersects the operator list."},
								"web_search_filter": {"type": "string", "enum": ["drop", "keep"]},
								"parent_context":   {"type": "object", "properties": {"root_agent_run_id": {"type": "string"}, "function_key": {"type": "string"}, "tier_at_run": {"type": "string"}}, "description": "Set a shared root_agent_run_id across the spawns to group the batch for cost attribution."},` + spawnPerRunProps + `
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
			Description: "Stop ONE in-flight run by agent_id, cascading to every sub-agent it spawned. Idempotent \u2014 cancelling a finished or unknown run is not an error. Takes `agent_id`, the handle spawn_run returned. Use it to stop a run that is looping, expensive or no longer wanted. Do NOT use it to stop the whole deployment \u2014 that is pause_runtime, which quiesces new admissions without killing work in flight. Do NOT expect a partial result: cancellation ends the run, it does not return what it had so far; read the transcript separately if you need that. It does not undo side effects the run already committed.",
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
			Description: "Return the current status of ONE run, by agent_id \u2014 the handle spawn_run returned. Takes `agent_id` (required). Reports status and the run's latest tracked state. Use it to poll a run you hold a handle for. Do NOT use it to browse or search runs you have no handle for \u2014 that is list_runs, which is filtered by user. It returns a status snapshot, not the conversation: for the transcript or the final text, read the run's own surfaces.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["agent_id"],
				"properties": {"agent_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "compact_run",
			Description: "Summarize a run's conversation so it can continue with freed context. Targets a run by `agent_id` (resolved to its run_id). Returns {compacted, before_tokens, after_tokens, applied}, where applied is 'live' (pushed into the running loop), 'marker' (persisted for a terminal run's next continuation) or 'noop' (too short to be worth compacting). Honours the agent's own compaction settings \u2014 keep_last_n, keep_first, target_percentage and the summary model. A LIVE run must be PARKED, awaiting input: a mid-turn run is refused rather than compacted underneath itself, so park it first or wait. Do NOT reach for it to shorten output \u2014 it rewrites the run's own history, not what the run returns to you. 'noop' is a normal answer, not a failure.",
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
			Name:        "configured_run",
			Description: "Create a run WITHOUT starting it, edit it, then start or discard it. op=create validates a run exactly as spawn_run would \u2014 same agent, segments and per-run overrides \u2014 but stores it instead of running it: it takes no concurrency slot and no token budget, and returns {run_id, agent_id, session_id, status:\"configured\", draft}. op=update replaces fields of the draft: pass `patch`, an object in the same field names, where null removes a field; the agent, the user, the agent_id and credentials cannot be changed. op=start runs the draft to completion and BLOCKS like spawn_run, returning the same result; admission happens here, so a busy or over-budget refusal leaves the run configured to start again later. op=delete discards the draft. Use it when a run should be reviewed or adjusted before it spends anything. Do NOT pass user_bearer or user_credentials to create \u2014 a configured run never stores them; give them to start. Do NOT use op=delete on a run that has started \u2014 cancel_run stops a live run.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["op"],
				"properties": {
					"op":               {"type": "string", "enum": ["create", "update", "start", "delete"]},
					"run_id":           {"type": "string", "description": "The configured run, from create. Required for update, start and delete."},
					"agent":            {"type": "string", "description": "create: the registered agent to run."},
					"segments":         {"type": "array", "description": "create: prompt segments, as for spawn_run.", "items": {"type": "object", "required": ["role", "content"], "properties": {"role": {"type": "string", "enum": ["system", "user"]}, "content": {"type": "array", "items": {"type": "object", "required": ["type"], "properties": {"type": {"type": "string", "enum": ["trusted-text", "untrusted-block", "image"]}, "text": {"type": "string"}, "cacheable": {"type": "boolean"}, "kind": {"type": "string", "description": "untrusted-block source label (e.g. web_content)"}, "media_type": {"type": "string", "enum": ["image/png", "image/jpeg", "image/gif", "image/webp"], "description": "image blocks only; valid only in a user segment"}, "data": {"type": "string", "description": "image blocks only: base64-encoded image bytes, NO data: prefix"}}}}}}},
					"user_id":          {"type": "string"},
					"agent_id":         {"type": "string", "description": "create: optional caller-chosen handle; refused if a live or configured run already holds it."},
					"user_tier":        {"type": "string"},
					"tools":            {"type": "array", "items": {"type": "string"}},
					"allowed_hosts":    {"type": "array", "items": {"type": "string"}, "description": "OMIT for no narrowing. Empty array [] denies all outbound HTTP; a non-empty array intersects with the operator's list."},
					"web_search_filter": {"type": "string", "enum": ["drop", "keep"]},
					"run_timeout_seconds": {"type": "integer", "minimum": 1, "description": "create: the run's own wall-clock budget once started."},
					"patch":            {"type": "object", "description": "update: the fields to replace, in the same names create takes; null removes one."},
					"user_bearer":      {"type": "string", "description": "start: per-run MCP bearer, as for spawn_run."},
					"user_credentials": {"type": "object", "additionalProperties": {"type": "string"}, "description": "start: per-tool named credentials, as for spawn_run."},
					` + spawnPerRunProps + `
				}
			}`),
		},
		{
			Name:        "retune_run",
			Description: "Change a RUNNING agent's settings without sending it a turn. Targets a run by `agent_id`. Use it to take hold of a run that is going the wrong way: move it to a different model, raise its iteration bound, or park it at its next turn boundary so a person can correct it (`interactive`). Returns the run's merged configuration, which is what it now holds — not an echo of what you sent, because the merge is not a field-wise union: naming a model clears the provider, and naming a tier clears the model. Overrides select WITHIN what the agent's definition already allows and cannot widen it; one it forbids is REFUSED here rather than applied and discovered later. Do NOT use it to send the agent a message — that is spawn_run with the run's session_id, and a retune deliberately writes nothing to the transcript that the operator did not say. At least one field is required: an empty call is refused rather than reported as a no-op change.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["agent_id"],
				"properties": {
					"agent_id": {"type": "string", "description": "The handle spawn_run returned."},
					` + retuneProps + `
				}
			}`),
		},
		{
			Name: "directory",
			Description: "Who is in this deployment, and what is held for them. READ-ONLY. op=users (default) lists the subjects with activity in your tenant; op=inspect with a subject aggregates that one subject's activity, chats, memory rows, documents, token budget and usage in a single call. op=tenants enumerates tenants with counts and requires an operator-admin token. There is no create/update/delete: a user is DERIVED from run activity, not a stored record, so there is nothing to write \u2014 to remove a subject's footprint use the `erasure` tool, which is the only thing that does it across every plane. Tenant listings are derived from runs, so a tenant with no runs does not appear (an empty list means no ACTIVITY, not no tenants). The tenant is taken from your credentials and cannot be passed. Do NOT use it to change anything \u2014 there is no create, update or delete, because a subject is DERIVED from activity rather than stored. Do NOT use it to remove what it reports: erasure deletes a subject's data; this only shows you what exists." +
				"op=users (default) lists the subjects with activity in your tenant; " +
				"op=inspect with a subject aggregates that one subject's activity, chats, memory rows, " +
				"documents, token budget and usage in a single call. " +
				"op=tenants enumerates tenants with counts and requires an operator-admin token. " +
				"There is no create/update/delete: a user is DERIVED from run activity, not a stored " +
				"record, so there is nothing to write — to remove a subject's footprint use the " +
				"`erasure` tool, which is the only thing that does it across every plane. " +
				"Tenant listings are derived from runs, so a tenant with no runs does not appear " +
				"(an empty list means no ACTIVITY, not no tenants). " +
				"The tenant is taken from your credentials and cannot be passed.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {
					"op":      {"type": "string", "enum": ["users", "inspect", "tenants"], "description": "users (default) | inspect | tenants (admin only)."},
					"subject": {"type": "string", "description": "inspect only: the user id to aggregate."}
				}
			}`),
		},
		{
			Name: "erasure",
			Description: "Report or erase everything this deployment holds about one SUBJECT (a user id), " +
				"scoped to your own tenant. op=report (default) is read-only and returns three tiers: " +
				"tier1_covered (deletable with existing primitives), tier2_uncovered (subject-keyed but " +
				"nothing deletes it — credentials are the ones that matter), and tier3_residue (facts ABOUT " +
				"the subject living in scopes they do not own, found only by tracing provenance from their " +
				"chats). op=execute removes tiers 1 and 2. " +
				"IT DEFAULTS TO A DRY RUN: pass dry_run:false AND confirm equal to the subject to actually " +
				"delete, which is irreversible. " +
				"IMPORTANT: tier-3 residue is traceable only through the subject's chats, which execute " +
				"deletes — so a report run AFTERWARDS shows residue 0 while those facts remain. The execute " +
				"response is the only durable record of what was not reached; keep it. " +
				"The usage/cost ledger is retained by design (accounting records). " +
				"The tenant is taken from your credentials and cannot be passed.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["subject"],
				"properties": {
					"subject": {"type": "string", "description": "The user id whose footprint to report or erase."},
					"op":      {"type": "string", "enum": ["report", "execute"], "description": "report (default, read-only) or execute."},
					"dry_run": {"type": "boolean", "description": "execute only. Defaults TRUE — omitting it deletes nothing."},
					"confirm": {"type": "string", "description": "execute only. Must equal subject when dry_run is false."}
				}
			}`),
		},
		{
			Name:        "list_runs",
			Description: "Enumerate a USER's runs, newest first. `user_id` is REQUIRED \u2014 there is no unfiltered listing of everything in the deployment. Optional `status` filters to running / completed / failed / cancelled, and `limit` (1\u2013200) caps the page. Use it to find runs when you do not already hold an agent_id. Do NOT use it to poll one run you already have a handle for \u2014 get_run answers that directly and more cheaply. It returns run metadata, never transcripts or final text.",
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
			Description: "Define an agent that lives in memory for a TTL \u2014 the quick, disposable way to get an agent running now. Requires `name`, `system_prompt` and `tools`; it survives until the TTL expires or unregister_agent removes it, and it does not outlive the runtime. Bash, Write and Edit are STRIPPED from the tool list unless the operator set LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1, so ask for them and you may silently get an agent without them \u2014 check what came back. Do NOT use it for an agent you want to keep, version, or hand to someone else: that is agentdef, the durable definition plane with create / fork / promote / retire and content hashes. Rule of thumb: register_agent for a scratch agent inside this session, agentdef for anything that should still exist tomorrow.",
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
			Description: "Remove a dynamic agent registered with register_agent. Idempotent \u2014 removing an unknown name is not an error. Static agents defined in the operator's yaml CANNOT be removed this way and are refused. Use it to clean up a scratch agent before its TTL expires. Do NOT use it to retire a durable definition \u2014 that is agentdef op=retire, which versions the change instead of dropping it. Runs already in flight are not cancelled by this; use cancel_run for those.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["name"],
				"properties": {"name": {"type": "string"}}
			}`),
		},
		{
			Name:        "list_agents",
			Description: "List every agent this deployment can run \u2014 both static ones from the operator's yaml and dynamic ones currently alive under a TTL. No arguments. Use it to discover what `agent` values spawn_run will accept. Do NOT use it to inspect ONE agent's definition \u2014 that is agentdef op=get, which returns the versioned body and its content hash. A dynamic agent disappears from this list when its TTL expires, so a name that worked earlier in a long session may be gone.",
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
			Description: "Memory tool ops. Families: key/value (get/set/delete/list/incr/merge/append_dedupe/bounded_list/search); memory-layer (add/recall \u2014 add enqueues for background consolidation, recall needs an embedder + vector store); SQL (sql_query/sql_exec/sql_begin/sql_commit/sql_rollback \u2014 a per-scope SQL database, gated separately by sql_scopes); placement (which scope a batch of {type, subject} facts belongs in, from the operator's per-type declaration on the tenant ontology \u2014 it DECIDES and never writes, so a caller that stores a fact in more than one place can ask once and put both halves together); consolidation (cursor_get/cursor_scan/cursor_lease/cursor_advance/cursor_release/supersede/pending_drain/pending_ack \u2014 background memory consolidation, gated separately by a dedicated grant; supersede retires a fact in BOTH the k/v plane and the fact graph in one call, and takes an optional superseded_by naming the fact that replaces it). Do NOT use it for anything you want back as structured prose with sections, links or citations \u2014 that is the document tool. Do NOT use it to browse by name: memory is keyed, and path is what resolves a human-readable path. Three tools share this ground and the split is by SHAPE, not by topic: memory stores values and facts you look up by key or by meaning; document stores structured, chunked prose you navigate and cite; path is only the NAMING layer over both \u2014 it resolves a human-readable path to a thing and never reads its content. The op enum in the input schema is authoritative; the families above group it.",
			InputSchema: builtinSchema("memory"),
		},
		{
			Name:        "channel",
			Description: "Send and receive messages on an operator-declared channel \u2014 loomcycle's durable queue between agents and runs. One tool, all ops: publish, subscribe, peek, ack, release, await, broadcast, list_channels. Every op takes `channel` plus `scope` ('global' or 'user'); `scope_id` is REQUIRED and is the user_id when scope='user'. await is a multi-channel fan-in barrier (any / all / at_least N, or a timeout) and does NOT commit a cursor; broadcast sends one payload to N channels with an atomic permission pre-flight; release hands over the oldest `count` (default 1) messages held on a hold: channel. USE THIS unless you specifically want a single-purpose tool: await, broadcast and release exist ONLY here \u2014 publish_channel / subscribe_channel / peek_channel / ack_channel are narrower twins of four of these ops and nothing more. Do NOT use it to create, edit or purge a channel itself \u2014 that is channeldef. Publishing to a channel the operator never declared is refused; it does not create one.",
			InputSchema: builtinSchema("channel"),
		},
		{
			Name:        "channeldef",
			Description: "Create, update, delete or purge the channel DEFINITION \u2014 the channel substrate's admin CRUD, twin of the REST /v1/_channels surface. Ops: create, update, delete, purge. This is the plane that decides which channels exist and how they behave; it does not move messages. Channels declared in the operator's yaml are IMMUTABLE here: create, update and delete on one return channel_yaml_immutable. `purge` is the exception \u2014 it clears buffered messages without touching the definition, and is allowed on ANY channel including yaml-declared ones. Do NOT use it to publish or read messages: that is `channel`, or the publish/subscribe/peek/ack tools. Note the asymmetry before reaching for it: `purge` DISCARDS buffered messages irreversibly, while `delete` removes a runtime-declared channel entirely.",
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
					"hold":         {"type": "boolean", "description": "Breakpoint: publishes are stored but never delivered until released (POST /v1/_channels/{name}/release, or Channel op=release)."},
					"publisher":    {"type": "string", "description": "create only. Free-form attribution."}
				}
			}`),
		},
		{
			Name:        "agentdef",
			Description: "Author and version a DURABLE agent definition \u2014 the system prompt, tool grant, model tier and policy the runtime resolves when someone runs that agent by name. Ops: create, fork, get, list, promote, retire. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. fork starts from an existing version, so it is the way to change a def; retire stops a version being served without deleting its history. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. A def you create is NOT live until it is promoted. You cannot widen your own reach with it: the `tools` you give an agent must be a subset of what YOUR token already holds, and a create that tries is refused rather than trimmed. Do NOT use it for a throwaway agent you only need in this session \u2014 register_agent is the TTL-scoped scratch version and needs no promote. Do NOT use it to RUN anything: spawn_run does that, by name.",
			InputSchema: builtinSchema("agentdef"),
		},
		{
			Name:        "skilldef",
			Description: "Author and version a durable SKILL \u2014 a named, reusable block of instructions an agent pulls in with the Skill tool, rather than a whole agent. Ops: create, fork, get, list, promote, retire. Same lifecycle as agentdef: Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. A skill cannot widen the agent that loads it \u2014 the tools a skill declares must be within the agent's own grant, and a skill asking for more is refused at call time, not silently trimmed. Do NOT use it for something that needs its own prompt, model or tool grant: that is an agent, so use agentdef. Rule of thumb: a skill is a procedure many agents can share; an agent is who is following it.",
			InputSchema: builtinSchema("skilldef"),
		},
		{
			Name:        "teamdef",
			Description: "Author and version a TEAM \u2014 a workflow expressed as a state machine of agent nodes and the transitions between them. Ops: create, fork, get, list, promote, retire, verify. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. The graph is validated BEFORE any write, so an unreachable state or a dangling transition is refused rather than stored and discovered at run time; verify re-checks a stored def against the agents and channels it names. Layout colours are excluded from the content hash, so moving a node on a canvas does not fork the definition. Do NOT use it to run one agent \u2014 that is spawn_run; a team is for work that hands off between several. Do NOT use it to fan the SAME agent out over many inputs: spawn_runs does that in one call without a graph.",
			InputSchema: builtinSchema("teamdef"),
		},
		{
			Name:        "mcpserverdef",
			Description: "Register an external MCP server at runtime so its tools become available to agents here. Ops: create, fork, get, list, promote, retire, rediscover, verify. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. rediscover re-reads the server's tool list after it changed; verify checks the definition still resolves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. HTTP and Streamable-HTTP servers only \u2014 stdio servers stay in the operator's yaml and cannot be added here. The URL's hostname must already be on the operator's HTTP host allowlist, so a server on an unlisted host is refused at create rather than failing later at call time. Do NOT use it to grant an agent those tools: registering the server publishes them, and the agent's own `tools` list still has to name them.",
			InputSchema: builtinSchema("mcpserverdef"),
		},
		{
			Name:        "scheduledef",
			Description: "Author and version a SCHEDULE \u2014 a recurring run of an agent, owned by a user. Ops: create, fork, get, list, retire. Versioned like the other defs, but forks AUTO-PROMOTE by default: there is no separate promote step here, so a fork goes live immediately. That differs deliberately from agentdef and skilldef, where promote is its own decision. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. Do NOT use it to run something once, now \u2014 that is spawn_run. Retiring a schedule stops future fires; it does not cancel a run already in flight, which is cancel_run.",
			InputSchema: builtinSchema("scheduledef"),
		},
		{
			Name:        "a2aservercarddef",
			Description: "Author and version an A2A SERVER CARD \u2014 how this deployment advertises itself to other agent platforms that speak agent-to-agent. Ops: create, fork, get, list, retire. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. This is the OUTWARD-facing half: what peers discover about us. Do NOT use it to describe a peer you want to call \u2014 that is a2aagentdef, the inward-facing half. Getting these the wrong way round is the usual mistake.",
			InputSchema: builtinSchema("a2aservercarddef"),
		},
		{
			Name:        "a2aagentdef",
			Description: "Author and version an A2A PEER \u2014 a remote agent on another platform that agents here can call. Ops: create, fork, get, list, retire. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. This is the INWARD-facing half: who we can reach. Do NOT use it to publish how others reach US \u2014 that is a2aservercarddef. Registering a peer makes it addressable; it does not grant any particular agent permission to call it.",
			InputSchema: builtinSchema("a2aagentdef"),
		},
		{
			Name:        "webhookdef",
			Description: "Author and version an INBOUND WEBHOOK \u2014 an external HTTP callback that starts a run here. Ops: create, fork, get, list, retire. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. Webhooks declared in the operator's yaml are immutable ground truth and cannot be edited through this tool; what you author here is the derived runtime layer beside them. Do NOT use it to CALL an outbound endpoint \u2014 that is the HTTP tool; this is the door inward. Retiring one stops it accepting deliveries; it does not remove runs it already started.",
			InputSchema: builtinSchema("webhookdef"),
		},
		{
			Name:        "memorybackenddef",
			Description: "Author and version a named MEMORY BACKEND \u2014 where an agent's memory is stored and how it is retrieved. Ops: create, fork, get, list, retire. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. Backends declared in the operator's yaml are immutable ground truth; what you author here is the derived runtime layer beside them. Do NOT use it to read or write memory CONTENT \u2014 that is the memory tool. This decides where the content lives, not what is in it. Repointing an agent at a different backend does not migrate anything that was already stored.",
			InputSchema: builtinSchema("memorybackenddef"),
		},
		{
			Name:        "documentsourcedef",
			Description: "Author and version a named DOCUMENT SOURCE \u2014 a peer loomcycle whose documents can be pulled into this one. Ops: create, fork, get, list, retire. Definitions are VERSIONED and immutable: create and fork each mint a new version rather than editing one, and promote decides which version the runtime actually serves. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. Sources declared in the operator's yaml are immutable ground truth; what you author here is the derived runtime layer. Defining a source only makes it NAMEABLE \u2014 the document tool's set_remote binds a document to it and sync moves the content; nothing syncs because a source exists. Do NOT use it to read documents: that is the document tool.",
			InputSchema: builtinSchema("documentsourcedef"),
		},
		{
			Name:        "operatortokendef",
			Description: "Mint, rotate and retire BEARER TOKENS, each bound to an authoritative principal \u2014 a tenant, a subject, and the scopes it may use. Ops: create, rotate, retire, get, list. ADMIN ONLY: tokens have no tenant dimension to confine them to, so a tenant or user token will not see this tool at all. THE PLAINTEXT IS SHOWN ONCE, on create and on rotate, and cannot be retrieved afterwards \u2014 capture it immediately or rotate again. get and list return metadata only. rotate issues a new secret for the same principal; retire revokes one. Do NOT use it to change what an existing token may do: scopes are bound at mint time, so authorising differently means minting a new token and retiring the old one.",
			InputSchema: builtinSchema("operatortokendef"),
		},
		{
			Name:        "volumedef",
			Description: "Provision a filesystem VOLUME an agent can read and write \u2014 the storage behind the Read, Write, Edit, Glob and Grep tools. Ops: create, get, list, delete, purge. Unlike the other def tools these are HANDLES, not versions: there is no fork and no promote, and create/delete simply add and remove. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. You supply a NAME and a MODE only \u2014 never a host path. The runtime derives the location inside an operator-blessed parent, so a volume cannot be pointed at arbitrary storage. delete and purge are NOT the same and the difference is unrecoverable: delete unmaps the volume and LEAVES the files; purge removes the row AND deletes the directory tree. Do NOT use it to read or write file contents \u2014 that is the Read and Write tools, once an agent is bound to the volume.",
			InputSchema: builtinSchema("volumedef"),
		},
		{
			Name:        "credentialdef",
			Description: "Store a named API secret that agents can USE without ever seeing it. Ops: create, get, list, delete. Secrets are encrypted at rest and referenced elsewhere as $cred:<name>, resolved server-side at call time \u2014 the model receives the reference, never the value. Scope is tenant (shared by everyone in it), user (keyed to YOUR subject \u2014 a personal bot token, say) or agent. Confined to your own tenant: rows are stamped from your token, and another tenant's def reads back as not-found rather than forbidden. get and list return METADATA ONLY: name, scope, timestamps. There is no op that reads a secret back, by design, so record it elsewhere if you need it again. Requires the deployment to have a secret key configured; without one, create is refused rather than storing anything in the clear. Do NOT use it for non-secret configuration \u2014 anything you would be happy to read back belongs in the definition that uses it.",
			InputSchema: builtinSchema("credentialdef"),
		},
		{
			Name:        "path",
			Description: "Resolve, list and reorganise human-readable PATHS over your memory entries, volume mounts and documents \u2014 a naming layer, not a store. Ops: resolve, ls, stat, mkdir, mv, rm. Takes `op` and `path`; scope is agent, user or tenant (default agent) and tenant-isolated. Segments are limited to [a-zA-Z0-9._-] and \"..\" is refused, so a path cannot climb out of its scope. mkdir is a no-op because directories are implicit \u2014 a path exists when something is named at it. Use it to BROWSE (what is under /docs?) or to rename and move without touching content. Do NOT use it to read or write what a name points at: resolve the path, then use document for a document, memory for a memory entry, or the file tools for a volume. `rm` removes the NAME only \u2014 the thing it pointed at still exists and is still reachable by id, so this is not a delete. Three tools share this ground and the split is by SHAPE, not by topic: memory stores values and facts you look up by key or by meaning; document stores structured, chunked prose you navigate and cite; path is only the NAMING layer over both \u2014 it resolves a human-readable path to a thing and never reads its content.",
			InputSchema: builtinSchema("path"),
		},
		{
			Name: "document",
			// Keep this op list in step with the `op` enum in documentInputSchema — the
			// schema is generated from the tool, this prose is not, and it had drifted
			// ten ops behind (the whole entity tier, set_path, reorder_chunk, the image
			// assets and the Markdown round-trip were all callable but unadvertised).
			// A model reads this description; an under-advertised op is one it will not
			// reach for. Same failure as a hand-written tool inventory beside the list
			// it describes.
			Description: "Document tool ops \u2014 documents (create_document/get_document/query_documents/documents_summary/delete_document/set_path), chunks (create_chunk/get_chunk/update_chunk/delete_chunk/move_chunk/reorder_chunk), facts (upsert_chunk/supersede_chunk/graph_recall/list_facts/judge_fact/verbatim_answer/verification_stats/remember), the ontology (propose_entity/propose_subject), links (link_chunks/unlink_chunks/get_edges/backlinks/related/unlinked_mentions), tags (add_tags/remove_tags/list_tags), history (history/get_version/diff), types (define_type/list_types), assets (set_asset/get_asset), search (query_chunks/search), import/export (export_md/import_md/export_canvas/import_canvas), and federation (set_remote/sync/diff_remote). Chunked-graph documents \u2014 each chunk is a first-class unit (UUID, hierarchy, type, fields, edges, Markdown body). upsert_chunk/supersede_chunk/graph_recall add a bi-temporal fact tier: write by natural_key, correct without deleting, and recall across relations as of a past instant. remember stores a statement a PERSON supplied as a fact that cites ITSELF: the text becomes both the claim and its source span, filed as evidential. Additive only \u2014 it is never a way to delete. verbatim_answer answers a LOOKUP question with a stored fact quoted verbatim plus its source span and no generated text, returning one only when a single verified fact clearly matches (every ambiguity resolves to no answer, with the reason). judge_fact records whether a fact is SUPPORTED by the source span recorded on it; an unsupported fact is withheld from the fact surfaces rather than deleted, and stays readable with include_refuted. propose_entity SUGGESTS an entity type for the tenant ontology and propose_subject suggests a SUBJECT the tenant does not know yet: what either files is inert until an operator accepts it, and they are the only way an agent may touch the ontology document. A proposed subject changes nothing until adoption \u2014 facts about it stay in the scope that learned them, which is why proposing is safe from a transcript and minting is not. set_remote binds this document to a peer loomcycle declared in the operator's document_sources; sync then reconciles keyed chunks with that peer by natural_key \u2014 body, tags, hierarchy, and manual links \u2014 direction pull (default) copies the peer's chunks in, push writes this document's chunks up to the peer; diff_remote is the read-only dry-run. Requires SQL Memory. Scope agent/user/tenant (tenant = shared across the whole tenant, and requires BOTH memory_scopes and sql_scopes to grant it); tenant-isolated. Do NOT use it as a key/value store, or for a fact you only ever fetch by key \u2014 that is memory, and a document carries structure you would not be using. Do NOT use it to look a document up by human-readable path \u2014 resolve the path with the path tool, then come here with the id. Three tools share this ground and the split is by SHAPE, not by topic: memory stores values and facts you look up by key or by meaning; document stores structured, chunked prose you navigate and cite; path is only the NAMING layer over both \u2014 it resolves a human-readable path to a thing and never reads its content.",
			InputSchema: builtinSchema("document"),
		},
		{
			Name:        "history",
			Description: "Browse, search and annotate past CHATS \u2014 a chat being one conversation session, with its own transcript and token/cost/run totals. Ops: list, get, search, rename, annotate, pin, archive. `search` matches the chat's TITLE by default, which is usually auto-generated \u2014 pass match:\"content\" to search what was actually SAID in your own turns instead, which is what you want when you remember the conversation but not what it was called. Owner scope is self, user, tenant or global (global is admin only), resolved server-side from your identity, so a cross-scope read folds to an opaque not-found rather than a refusal that confirms the row exists. Use it to find what was discussed before, across sessions. Do NOT use it to fetch what a RUN did \u2014 a chat groups runs; list_runs and get_run are the run-level view. Do NOT use it as a memory store: renaming, pinning and annotating are metadata about a conversation, not durable facts \u2014 those belong in memory.",
			InputSchema: builtinSchema("history"),
		},
		{
			Name:        "evaluation",
			Description: "Record and read back scores for agent runs \u2014 the feedback loop for judging whether a run or a definition is doing well. Ops: submit (attach a score to a run), get (one evaluation), list_for_run, list_for_def (every score for a definition), aggregate (roll them up). Use it to close the loop: spawn a run, judge the output, submit the verdict, aggregate over a definition to compare versions. Do NOT use it to read what a run PRODUCED \u2014 that is get_run and the run's own surfaces; this holds judgements about runs, not their output. Scores are additive: submitting again records another evaluation rather than replacing the previous one.",
			InputSchema: builtinSchema("evaluation"),
		},
		{
			Name: "context",
			// Hand-written mirror of the Context tool's op list — the InputSchema
			// below is the tool's own, so the enum can't drift, but this string
			// can and did (it omitted `compact` for a whole release line). Keep
			// it in sync with contextInputSchema when adding an op.
			Description: "Ask the runtime about ITSELF and about your own session \u2014 the introspection tool. Ops: self, tools, doc, permissions, agents, lineage, evaluations, channels, help, time, compact, capabilities. Start with capabilities to learn what this deployment supports before calling something that would refuse; tools lists what YOU may call and doc returns one tool's full schema. Every listing is confined to your own grant \u2014 it reports what you hold, not the deployment's whole catalogue. Do NOT use it to change anything except op=compact, which acts on your own run; the rest are reads. Do NOT use op=agents to discover what you can spawn \u2014 list_agents is the runnable inventory; op=agents is definition metadata.",
			InputSchema: builtinSchema("context"),
		},

		// --- Pause/Resume (v0.8.17 primitives, exposed via Connector in v0.8.18) ---
		{
			Name:        "pause_runtime",
			Description: "Quiesce the whole runtime: stop admitting new work and bring in-flight runs to a stop at a safe point. ADMIN ONLY: this is runtime-global with no tenant dimension, so a tenant or user token will not see it. Idempotent tools are cancelled immediately; non-idempotent and external ones get a grace window (default 30s) and are then force-cancelled. While paused, new runs are refused with 503 on every entry surface. Returns {status, duration_ms, force_cancelled_count, paused_runs_count, warnings?}; already pausing or paused is a 409, not a no-op. Use it before a snapshot or a restart, so state is captured at rest. Do NOT use it to stop ONE run \u2014 that is cancel_run; this stops the deployment. It does not lift by itself: the runtime stays paused until resume_runtime.",
			InputSchema: rawJSON(`{
				"type": "object",
				"properties": {"timeout_ms": {"type": "integer", "default": 30000}}
			}`),
		},
		{
			Name:        "resume_runtime",
			Description: "Release a quiesce: every run paused by pause_runtime flips back to running and its loop re-enters. ADMIN ONLY: this is runtime-global with no tenant dimension, so a tenant or user token will not see it. Returns {status, resumed_run_count, warnings?}; calling it when the runtime is not paused is a 409. Use it after the maintenance the pause was covering. Do NOT expect it to revive runs that were FORCE-CANCELLED during the pause \u2014 those ended, and only the parked ones come back. It does not restart the process; it lifts the gate.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "get_runtime_state",
			Description: "Report whether the runtime is running, pausing or paused, with the number of paused runs and stored snapshots. ADMIN ONLY: this is runtime-global with no tenant dimension, so a tenant or user token will not see it. Returns {status: 'running'|'pausing'|'paused', paused_run_count, snapshots_count}. No arguments, and it changes nothing. Use it to check before pausing, or to poll while a pause drains \u2014 'pausing' means in-flight work has not settled yet. Do NOT use it to list the paused runs themselves \u2014 it returns a count; list_runs enumerates them. Do NOT use it as a health check: it reports the quiesce gate, not whether the deployment is well.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "resolve_probe",
			Description: "Re-probe every configured model provider NOW and return the refreshed availability matrix: {generated_at, providers: {<id>: {excluded, reachable, models, last_check, last_error}}}. ADMIN ONLY: this is runtime-global with no tenant dimension, so a tenant or user token will not see it. The escape hatch for when a transient outage has left every provider marked unavailable and runs are failing to route. Use it to force a re-check instead of waiting for the next scheduled probe. Do NOT use it as a routine health poll \u2014 it makes a live call to every provider, so it costs something each time. It reports what the resolver can reach; it does not change routing policy or re-enable a provider the operator disabled.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},

		// --- Snapshot (v0.8.17 primitives, exposed via Connector in v0.8.18) ---
		{
			Name:        "create_snapshot",
			Description: "Capture the runtime's state into one versioned JSON envelope: agent definitions and which version is active, memory, channels, evaluations, paused runs, and optionally interaction history. Takes the optional section toggles; returns a descriptor (id, created_at, section versions), NOT the envelope itself \u2014 fetch that with get_snapshot or export_snapshot. ADMIN ONLY: snapshots span every tenant, so there is no confined form of this tool and a tenant or user token will not see it. Use it before a risky migration or to move state to another instance. Do NOT treat it as a backup of everything: per-run secrets and call-time overrides are deliberately excluded and are re-derived from the agent definition on restore. It captures state, not traffic \u2014 a run in flight is captured only if it is PARKED.",
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
			Description: "List captured snapshots, most recent first, capped at 200. Returns metadata only \u2014 id, timestamps, section versions \u2014 never envelope content. ADMIN ONLY: snapshots span every tenant, so there is no confined form of this tool and a tenant or user token will not see it. Use it to find the id you want. Do NOT use it to inspect what a snapshot CONTAINS: fetch the envelope with get_snapshot, or the raw bytes with export_snapshot. There is no paging past 200; older snapshots are only reachable by id.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "get_snapshot",
			Description: "Fetch ONE snapshot envelope, including its full JSON content, by id. ADMIN ONLY: snapshots span every tenant, so there is no confined form of this tool and a tenant or user token will not see it. Use this when you or an agent needs to READ what a snapshot holds \u2014 inspect it, diff it, decide whether to restore it. Do NOT confuse it with export_snapshot, which returns the same envelope as canonical BYTES for writing to a file or piping to another instance; get_snapshot is the one to parse, export_snapshot is the one to move. An unknown id is an error, not an empty result.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "export_snapshot",
			Description: "Return a snapshot's canonical envelope BYTES for one id \u2014 the exact serialization to hand to another instance. ADMIN ONLY: snapshots span every tenant, so there is no confined form of this tool and a tenant or user token will not see it. Use this to move state between deployments: export here, then restore_snapshot with the bytes as raw_json there. Do NOT use it when you want to read or reason about the contents \u2014 get_snapshot returns the same envelope as parsed JSON. Over HTTP the equivalent route streams the body directly, so very large exports need not pass through a tool result. An unknown id is an error, not an empty result.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		{
			Name:        "restore_snapshot",
			Description: "Write a snapshot's state back into this runtime, from a local `snapshot_id` OR from cross-instance `raw_json` bytes. ADMIN ONLY: snapshots span every tenant, so there is no confined form of this tool and a tenant or user token will not see it. Idempotent per row (existing rows are left alone), and the returned counters report what was ACTUALLY written, not what was in the envelope \u2014 a second restore of the same snapshot legitimately reports zeros. Paused runs reference sessions, so a missing session row is synthesized and counted separately as synthesized_sessions. Use snapshot_id for same-instance rollback and raw_json for a migration from another deployment. Do NOT expect it to remove anything: it only adds and fills gaps, so restoring an older snapshot does NOT roll back rows created since. An envelope whose section version is newer than this runtime understands is refused rather than partially applied.",
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
			Description: "Delete one snapshot by id. Idempotent \u2014 it succeeds whether or not the row existed, so a repeat call is not an error. ADMIN ONLY: snapshots span every tenant, so there is no confined form of this tool and a tenant or user token will not see it. Use it to prune old captures. Do NOT expect it to touch runtime state: it removes the stored envelope only, and anything already restored from it stays exactly as it is. There is no undo, and no separate archive \u2014 export_snapshot first if the bytes still matter.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["snapshot_id"],
				"properties": {"snapshot_id": {"type": "string"}}
			}`),
		},
		// --- Interruption (v0.8.16) — the 21st meta-tool ---
		{
			Name:        "interruption_resolve",
			Description: "Resolve a pending Interruption.ask from outside the agent loop. Lets an external orchestrator (Claude Code, custom dashboard) act as the human answerer when the operator yaml configures `interruption.backend: mcp_server:...` or when the orchestrator wants to take over the webui default. Writes the answer, wakes the blocked agent loop, publishes _system/interrupts/resolved for downstream consumers. Returns 409-equivalent error on already-resolved / timed-out / cancelled rows. Use it when an agent is BLOCKED waiting on a human answer and you are acting as that human. Do NOT use it to message a running agent that is not asking anything \u2014 there is nothing to resolve, and a run that merely takes input is a session continuation via spawn_run. Do NOT use it to stop a run: answering releases the agent to carry on, and cancel_run is what ends it.",
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
			Description: "Register a pre- or post-tool webhook. The callback_url must be an http:// or https:// endpoint the consumer runs \u2014 loomcycle POSTs PreHookCall/PostHookCall payloads to it. Returns {id}. Re-registering the same (owner, name) replaces the prior entry with a fresh id (idempotent app-restart contract). Use the id with delete_hook. Use it to observe or gate tool calls from outside \u2014 an audit trail, a policy check, a dashboard. Do NOT use it to add capability to an agent: a hook watches tool calls, it does not provide a tool. Registering an MCP server is mcpserverdef. Hooks are IN-MEMORY and gone after a restart, so a consumer is expected to re-register on startup \u2014 which is why re-registering the same (owner, name) replaces rather than duplicates.",
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
			Description: "List every hook registered right now, in registration order. Returns {hooks: [...]}. No arguments. Hooks are held IN MEMORY, so this is empty after a loomcycle restart even though the consumers that registered them may still be running \u2014 an empty list means 'nobody has registered since boot', not 'nobody wants hooks'. Use it to find the id you need for delete_hook. Do NOT use it to see hook DELIVERIES or failures: it lists registrations, never the calls made to them.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "delete_hook",
			Description: "Remove one hook by id. Returns {deleted: id}. Takes the `id` register_hook returned \u2014 list_hooks has it if you did not keep it. An id that no longer exists is an error rather than a silent success, so a stale id tells you. Use it to stop a callback you registered. Do NOT use it to pause hooks temporarily \u2014 there is no disable, so removing and re-registering is the only route, and re-registering mints a NEW id.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["id"],
				"properties": {"id": {"type": "string"}}
			}`),
		},
		// v0.9.x n8n RFC Phase 0 — channel listing + run-state streaming.
		{
			Name:        "list_channels",
			Description: "List every operator-declared channel with aggregate traffic stats across ALL scopes \u2014 message_count, oldest_visible_at, newest_visible_at. No arguments. ADMIN ONLY: it aggregates over every user's scope, so it has no tenant-confined form and a tenant or user token will not see this tool at all. Use it to answer \"what channels exist and is anything backing up\". Do NOT use it to read messages \u2014 it returns counts and timestamps, never payloads; use subscribe_channel or peek_channel for those. If you hold a tenant token, use `channel` op=list_channels instead: same listing, confined to what you may see.",
			InputSchema: rawJSON(`{"type": "object"}`),
		},
		{
			Name:        "stream_user_run_states",
			Description: "Subscribe to run state transitions for one user_id. Returns {events: [RunStateEvent...], count}. When the session opted into capabilities.loomcycle.runEvents=true, each matching event also arrives as a notifications/loomcycle/run_state notification and the response carries an empty events array (count only). Filters: statuses (e.g. ['completed','failed']), agent (exact name), and walk_id \u2014 the runs one team walk spawned, matched on parent_context.walk_id. A team walk's own run_id IS its walk_id, so filtering by the id a detached run returned gives a live view of that workflow's agents. max_events caps the response at N events; timeout_ms bounds the blocking wait. Use it to WATCH runs as they change \u2014 a dashboard, or a live view of one team walk's agents. Do NOT use it to fetch the state of a run you already hold a handle for: get_run answers that in one call without a subscription. Do NOT expect run OUTPUT here \u2014 these are state transitions, never final text or transcripts.",
			InputSchema: rawJSON(`{
				"type": "object",
				"required": ["user_id"],
				"properties": {
					"user_id":    {"type": "string"},
					"statuses":   {"type": "array", "items": {"type": "string"}},
					"agent":      {"type": "string"},
					"walk_id":    {"type": "string", "description": "Only runs spawned by this team walk (its run_id)."},
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
			Description: "Put ONE message on an operator-declared channel. Takes `channel`, `scope` ('global' or 'user'), `scope_id` (REQUIRED when scope='user', ignored when 'global'), `payload` (any JSON value), and optional `deliver_at` (RFC3339Nano) to defer delivery \u2014 subscribers wake at visible_at, not before. Returns {msg_id, channel, created_at, visible_at?}. This is the single-op twin of `channel` op=publish; use whichever fits, but reach for `channel` if you also need await, broadcast or release, which exist only there. Do NOT use it to send one payload to SEVERAL channels \u2014 that is `channel` op=broadcast, which pre-flights permissions across all of them atomically instead of leaving you half-published. The channel must already be declared by the operator; publishing to an unknown name is refused rather than creating it.",
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
			Description: "Read the next batch of messages and COMMIT the cursor in one step \u2014 at-most-once delivery. Takes `channel`, `scope`, `scope_id` (REQUIRED when scope='user') and optional `wait_ms`: it returns immediately when messages are waiting, otherwise long-polls up to wait_ms (capped by the operator's limit). Returns {channel, messages:[{id, value, published_at}...], next_cursor}. The cursor advances as soon as the batch is returned, so a message you fail to process is GONE \u2014 this is the right choice when losing one is cheaper than handling it twice. Do NOT use it when processing must survive a crash: use peek_channel to read, do the durable work, then ack_channel to commit \u2014 that is at-least-once, and the pair exists for exactly this. An empty batch after wait_ms is a normal result, not an error.",
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
			Description: "Read messages WITHOUT committing the cursor \u2014 the read half of at-least-once processing. Takes `channel`, `scope`, `scope_id` (REQUIRED when scope='user'). Returns {channel, messages:[...]}. Nothing is consumed: the same messages come back on the next peek until you advance the cursor with ack_channel. USE THIS with ack_channel when a message must not be lost \u2014 peek, do the durable work, then ack. Do NOT use it alone as a polling loop: without an ack you will re-read the same messages forever. Do NOT use it when at-most-once is fine \u2014 subscribe_channel does the read and the commit in one round trip.",
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
			Description: "Commit the cursor for a (channel, scope, scope_id) tuple \u2014 the second half of at-least-once processing. Takes `channel`, `scope`, `scope_id` (REQUIRED when scope='user') and the `cursor` you are acknowledging. Returns {ok: true}. Cursors move FORWARD only: an older cursor is refused with channel_cursor_regression rather than silently rewinding, so a late ack cannot replay messages someone else has moved past. USE THIS after peek_channel, once the work is durable. Do NOT use it after subscribe_channel \u2014 that already committed. There is no un-ack: to re-process a message, republish it.",
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
