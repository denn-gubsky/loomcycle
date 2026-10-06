package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// A poll-mode team walk is a background child of the CALLING run: it is read
// with op=poll and ended with op=cancel from that run's own table. An MCP
// session has no run, so on this transport those ops and mode "poll" can never
// succeed — the builtin refuses them, but with in-run wording ("this run has no
// team walks to poll") that sends an MCP client looking for walks it never had.
// The MCP surface therefore neither advertises them nor accepts them; the
// in-run TeamDef tool is unchanged.
var (
	teamDefOffRunOps = []string{"poll", "cancel"}
	// The properties only poll mode reads: notify / on_parent_end on op=run,
	// run_ids / wait / wait_ms on op=poll and op=cancel.
	teamDefPollOnlyProps = []string{"notify", "on_parent_end", "run_ids", "wait", "wait_ms"}
)

const (
	teamDefMCPModeDescription = "run (optional): omit to wait for the walk and get its trace. \"detach\" returns {run_id, status:\"running\"} immediately and the walk continues in the background — use it when you need a handle WHILE the walk runs, to arm a breakpoint, answer a pause, or watch progress. Follow a detached walk with get_run (its run_id) or list_runs (walk_id). Either way the response carries run_id."
	teamDefPollRefusal        = "teamdef: poll mode needs a calling agent run, and an MCP session has none — op=poll, op=cancel and mode \"poll\" are not available here"
	teamDefPollRefusalFix     = "Run the team with mode \"detach\" and follow the walk by its run_id with get_run, or list its runs with list_runs walk_id."
)

// teamDefMCPSchema is the TeamDef builtin's input schema with the poll-mode
// surface removed. It is derived from the builtin's schema rather than restated
// so every other field keeps tracking the tool's real validation; a shape it
// cannot edit falls back to the builtin's schema, which
// TestTeamDefMCP_SchemaAdvertisesOnlyOffRunOps catches.
func teamDefMCPSchema() json.RawMessage {
	base := builtinSchema("teamdef")
	var s map[string]any
	if err := json.Unmarshal(base, &s); err != nil {
		return base
	}
	props, _ := s["properties"].(map[string]any)
	op, _ := props["op"].(map[string]any)
	mode, _ := props["mode"].(map[string]any)
	if op == nil || mode == nil {
		return base
	}
	op["enum"] = withoutValues(op["enum"], teamDefOffRunOps)
	mode["enum"] = withoutValues(mode["enum"], []string{"poll"})
	mode["description"] = teamDefMCPModeDescription
	for _, p := range teamDefPollOnlyProps {
		delete(props, p)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return base
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes()))
}

func withoutValues(enum any, drop []string) []any {
	vals, _ := enum.([]any)
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		if s, ok := v.(string); ok && slices.Contains(drop, s) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// handleTeamDef forwards to the TeamDef builtin, refusing the poll-mode surface
// up front (see teamDefOffRunOps) so a client that ignores the schema gets an
// answer that names the transport's limit and the way that does work.
func handleTeamDef(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var in struct {
		Op   string `json:"op"`
		Mode string `json:"mode"`
	}
	// A malformed body is the builtin's to report; only a readable op/mode
	// is judged here.
	if json.Unmarshal(args, &in) == nil &&
		(slices.Contains(teamDefOffRunOps, in.Op) || (in.Op == "run" && in.Mode == "poll")) {
		return toolErrValidation(teamDefPollRefusal, teamDefPollRefusalFix), nil
	}
	return wrapBuiltin("teamdef", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.TeamDef(ctx, in)
	})(ctx, env, args)
}
