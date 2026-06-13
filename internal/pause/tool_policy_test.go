package pause

import (
	"encoding/json"
	"testing"
)

// TestCategoryForInput covers the categorisation matrix for every
// tool the runtime can dispatch. Pins the locked per-tool cancel
// policy from rfcs/pause-resume-snapshot.md.
func TestCategoryForInput(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		input    string
		want     ToolCategory
	}{
		// Read-only file I/O — idempotent.
		{"Read", "Read", `{"path":"/etc/hosts"}`, CategoryIdempotent},
		// exp7: Glob/Grep are read-only searches; previously fell through to
		// the non-idempotent default.
		{"Glob", "Glob", `{"pattern":"**/*.go"}`, CategoryIdempotent},
		{"Grep", "Grep", `{"pattern":"func"}`, CategoryIdempotent},
		{"WebFetch", "WebFetch", `{"url":"https://example.com"}`, CategoryIdempotent},
		{"WebSearch", "WebSearch", `{"query":"go modules"}`, CategoryIdempotent},

		// File writes + shell — non-idempotent.
		{"Write", "Write", `{"path":"/tmp/x","content":"hi"}`, CategoryNonIdempotent},
		{"Edit", "Edit", `{"path":"/tmp/x","old":"a","new":"b"}`, CategoryNonIdempotent},
		{"Bash", "Bash", `{"command":"date"}`, CategoryNonIdempotent},

		// HTTP method-discriminated.
		{"HTTP_GET", "HTTP", `{"method":"GET","url":"https://x"}`, CategoryIdempotent},
		{"HTTP_HEAD", "HTTP", `{"method":"HEAD","url":"https://x"}`, CategoryIdempotent},
		{"HTTP_default_is_GET", "HTTP", `{"url":"https://x"}`, CategoryIdempotent},
		{"HTTP_POST", "HTTP", `{"method":"POST","url":"https://x"}`, CategoryNonIdempotent},
		{"HTTP_PUT", "HTTP", `{"method":"PUT","url":"https://x"}`, CategoryNonIdempotent},
		{"HTTP_PATCH", "HTTP", `{"method":"PATCH","url":"https://x"}`, CategoryNonIdempotent},
		{"HTTP_DELETE", "HTTP", `{"method":"DELETE","url":"https://x"}`, CategoryNonIdempotent},
		{"HTTP_method_case_insensitive", "HTTP", `{"method":"post","url":"https://x"}`, CategoryNonIdempotent},

		// Op-discriminated builtins. Memory reads vs writes.
		{"Memory_get", "Memory", `{"op":"get","scope":"agent","key":"k"}`, CategoryIdempotent},
		{"Memory_list", "Memory", `{"op":"list","scope":"agent"}`, CategoryIdempotent},
		{"Memory_set", "Memory", `{"op":"set","scope":"agent","key":"k","value":1}`, CategoryNonIdempotent},
		{"Memory_delete", "Memory", `{"op":"delete","scope":"agent","key":"k"}`, CategoryNonIdempotent},
		{"Memory_incr", "Memory", `{"op":"incr","scope":"agent","key":"k"}`, CategoryNonIdempotent},
		{"Memory_missing_op", "Memory", `{"scope":"agent"}`, CategoryNonIdempotent},

		// Channel reads vs writes.
		{"Channel_peek", "Channel", `{"op":"peek","channel":"c"}`, CategoryIdempotent},
		{"Channel_list_channels", "Channel", `{"op":"list_channels"}`, CategoryIdempotent},
		{"Channel_publish", "Channel", `{"op":"publish","channel":"c","payload":{}}`, CategoryNonIdempotent},
		{"Channel_subscribe", "Channel", `{"op":"subscribe","channel":"c"}`, CategoryNonIdempotent},
		{"Channel_ack", "Channel", `{"op":"ack","channel":"c"}`, CategoryNonIdempotent},

		// AgentDef + Evaluation reads vs writes.
		{"AgentDef_get", "AgentDef", `{"op":"get","def_id":"d"}`, CategoryIdempotent},
		{"AgentDef_create", "AgentDef", `{"op":"create","name":"a"}`, CategoryNonIdempotent},
		{"Evaluation_get", "Evaluation", `{"op":"get","eval_id":"e"}`, CategoryIdempotent},
		{"Evaluation_submit", "Evaluation", `{"op":"submit","run_id":"r"}`, CategoryNonIdempotent},

		// Context (all 10 ops are read-only).
		{"Context_self", "Context", `{"op":"self"}`, CategoryIdempotent},
		{"Context_history", "Context", `{"op":"history"}`, CategoryIdempotent},

		// Interruption — all ops are non-idempotent.
		{"Interruption_ask", "Interruption", `{"op":"ask","prompt":"?"}`, CategoryNonIdempotent},
		{"Interruption_notify", "Interruption", `{"op":"notify","message":"!"}`, CategoryNonIdempotent},
		{"Interruption_cancel", "Interruption", `{"op":"cancel","id":"i"}`, CategoryNonIdempotent},

		// Agent + Skill.
		{"Agent_spawn", "Agent", `{"name":"sub-agent","prompt":"go"}`, CategoryNonIdempotent},
		{"Skill_load", "Skill", `{"name":"karpathy-guidelines"}`, CategoryIdempotent},

		// MCP — all external; treated like non-idempotent for the
		// wait-with-timeout policy (CategoryExternal is its own
		// label but behaviour matches non-idempotent).
		{"MCP_any", "mcp__jobs__getAgentContext", `{}`, CategoryExternal},
		{"MCP_prefix_only", "mcp__", `{}`, CategoryExternal},

		// Unknown builtin → conservative non-idempotent.
		{"unknown_builtin", "FutureTool", `{}`, CategoryNonIdempotent},

		// Bad JSON → conservative (don't cancel-immediately something
		// we can't categorise).
		{"bad_JSON_Memory", "Memory", `{"op":"get`, CategoryNonIdempotent},
		{"bad_JSON_HTTP", "HTTP", `{"method`, CategoryIdempotent},
		// ^ HTTP defaults to GET when method missing/unparseable, so
		// even on bad JSON the default is idempotent.

		// Empty input → bring back to defaults.
		{"empty_Memory", "Memory", `{}`, CategoryNonIdempotent},
		{"empty_HTTP", "HTTP", `{}`, CategoryIdempotent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CategoryForInput(tc.toolName, json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("CategoryForInput(%q, %s) = %s, want %s",
					tc.toolName, tc.input, got, tc.want)
			}
		})
	}
}

// TestToolCategory_String pins the wire-stable category labels used in
// audit-event payloads. Operator dashboards parse these strings.
func TestToolCategory_String(t *testing.T) {
	cases := []struct {
		cat  ToolCategory
		want string
	}{
		{CategoryIdempotent, "idempotent"},
		{CategoryNonIdempotent, "non_idempotent"},
		{CategoryExternal, "external"},
		{ToolCategory(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.cat.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.cat, got, tc.want)
		}
	}
}
