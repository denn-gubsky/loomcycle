package pause

import (
	"encoding/json"
	"strings"
)

// ToolCategory discriminates how a pending tool call should be treated
// when pause is declared. Three categories cover every tool the runtime
// can dispatch:
//
//   - CategoryIdempotent      — cancel immediately on pause. Re-running
//     the tool on resume is safe and produces
//     the same result. Reads only.
//   - CategoryNonIdempotent   — wait for completion up to the operator-
//     supplied timeout; force-cancel after.
//     Writes, mutations, side-effecting calls.
//   - CategoryExternal        — MCP-served tools. Treated as
//     non-idempotent by default (we can't
//     introspect the MCP server's semantics)
//     BUT the manager applies the same
//     timeout-then-force-cancel policy.
type ToolCategory int

const (
	CategoryIdempotent ToolCategory = iota
	CategoryNonIdempotent
	CategoryExternal
)

// String returns a stable wire label, used in audit-event payloads
// and the force-cancelled-count details so operators can attribute
// cancellations to a category.
func (c ToolCategory) String() string {
	switch c {
	case CategoryIdempotent:
		return "idempotent"
	case CategoryNonIdempotent:
		return "non_idempotent"
	case CategoryExternal:
		return "external"
	default:
		return "unknown"
	}
}

// idempotentBuiltins is the static allowlist of built-in tool names
// whose Execute() is read-only OR otherwise safe to cancel-and-retry.
// Source of truth: doc-internal/rfcs/pause-resume-snapshot.md's locked
// per-tool cancel policy.
//
// Tools that take an `op` field (Memory, Channel, AgentDef, Evaluation,
// Context) are categorised at the OP level — see CategoryForInput
// below, which parses the input's `op` field and consults the
// op-specific table.
//
// MCP tools (prefix `mcp__`) are NOT in this map; they get
// CategoryExternal via the prefix check in CategoryForInput.
var idempotentBuiltins = map[string]bool{
	// Read-only file I/O
	"Read":      true,
	"WebFetch":  true,
	"WebSearch": true,
	// HTTP method-discriminated — see CategoryForInput; map entry
	// would be misleading.
}

// Op-discriminated builtin: idempotent ops vs non-idempotent ops on
// the Memory tool. Reads are safe to cancel; writes need wait-with-
// timeout.
var memoryIdempotentOps = map[string]bool{
	"get":  true,
	"list": true,
	// set / delete / incr are non-idempotent.
}

// Op-discriminated builtin: Channel tool.
var channelIdempotentOps = map[string]bool{
	"peek":          true,
	"list_channels": true,
	// publish / subscribe / ack are non-idempotent.
}

// Op-discriminated builtin: AgentDef tool.
var agentDefIdempotentOps = map[string]bool{
	"get":           true,
	"list":          true,
	"list_children": true,
	"list_names":    true,
	// create / fork / promote / retire are non-idempotent.
}

// Op-discriminated builtin: Evaluation tool.
var evaluationIdempotentOps = map[string]bool{
	"get":          true,
	"list_for_run": true,
	"list_for_def": true,
	"aggregate":    true,
	// submit is non-idempotent.
}

// Op-discriminated builtin: Interruption tool — all three ops are
// non-idempotent (ask blocks on human input; notify side-effects to
// the delivery surface; cancel mutates the interrupts row).
//
// Context tool's 10 ops are ALL idempotent — read-only introspection.
// Built-in HTTP tool is method-discriminated: GET/HEAD are
// idempotent; POST/PUT/PATCH/DELETE are not.

// CategoryForInput categorises one pending tool call. Inspects the
// tool name and (for op-discriminated tools) the op field of the
// input. Errors during input parsing default to NonIdempotent — the
// safe stance: don't cancel-immediately something we can't classify.
//
// The manager calls this once per pending tool at pause time. Not on
// the hot path of normal dispatch; the static maps + a single JSON
// peek are cheap enough to keep the policy logic centralised here
// rather than scattered through the tool implementations.
func CategoryForInput(toolName string, input json.RawMessage) ToolCategory {
	if strings.HasPrefix(toolName, "mcp__") {
		return CategoryExternal
	}
	switch toolName {
	case "Read", "Glob", "Grep", "WebFetch", "WebSearch":
		// Read-only file/search tools — safe to cancel immediately on pause
		// and re-run on resume (same result). Glob/Grep were previously
		// absent and fell through to the non-idempotent default, so a pending
		// directory walk needlessly blocked pause until its timeout.
		return CategoryIdempotent
	case "Write", "Edit", "Bash":
		// File writes + shell commands are always non-idempotent.
		return CategoryNonIdempotent
	case "HTTP":
		// Method-discriminated. Default to GET when method is missing
		// (matches the tool's own default).
		method := extractStringField(input, "method")
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" || method == "GET" || method == "HEAD" {
			return CategoryIdempotent
		}
		return CategoryNonIdempotent
	case "Memory":
		if isIdempotentOp(input, memoryIdempotentOps) {
			return CategoryIdempotent
		}
		return CategoryNonIdempotent
	case "Channel":
		if isIdempotentOp(input, channelIdempotentOps) {
			return CategoryIdempotent
		}
		return CategoryNonIdempotent
	case "AgentDef":
		if isIdempotentOp(input, agentDefIdempotentOps) {
			return CategoryIdempotent
		}
		return CategoryNonIdempotent
	case "Evaluation":
		if isIdempotentOp(input, evaluationIdempotentOps) {
			return CategoryIdempotent
		}
		return CategoryNonIdempotent
	case "Context":
		// All 10 Context ops (self / tools / doc / permissions / agents
		// / lineage / evaluations / channels / history / help) are
		// read-only introspection. Idempotent.
		return CategoryIdempotent
	case "Interruption":
		// ask blocks on human input; notify side-effects; cancel
		// mutates. None are safe to silently cancel-and-retry.
		return CategoryNonIdempotent
	case "Agent":
		// Spawning a sub-agent is a control-flow boundary; treat as
		// non-idempotent so the parent waits for the child to either
		// pause or finish at its own iteration boundary rather than
		// being abruptly torn down mid-spawn.
		return CategoryNonIdempotent
	case "Skill":
		// Loading a skill is a fast in-memory op; idempotent.
		return CategoryIdempotent
	}
	// Unknown built-in name. Conservative default.
	return CategoryNonIdempotent
}

// isIdempotentOp parses the input's `op` field and looks it up in the
// per-tool map. Missing op or parse error → false (the caller treats
// the missing-op case as non-idempotent, the safer default).
func isIdempotentOp(input json.RawMessage, opMap map[string]bool) bool {
	op := extractStringField(input, "op")
	if op == "" {
		return false
	}
	return opMap[op]
}

// extractStringField peels a single top-level string field out of the
// tool's input. Doesn't fully unmarshal — bounded peek that returns
// "" on any error or non-string value.
func extractStringField(input json.RawMessage, field string) string {
	if len(input) == 0 {
		return ""
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(input, &probe); err != nil {
		return ""
	}
	raw, ok := probe[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
