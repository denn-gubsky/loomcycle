package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The `calc` meta-tool is the Calc tool outside a run: the same input, output
// and refusals as the in-run tool. connector.Calc owns the one rule (each call
// takes a unit of the token's run rate); this file advertises the tool and
// renders a spent rate.

// calcHelpInRun is the help pointer in the Calc builtin's description. Over
// MCP the help is served by the `context` meta-tool, so it is rewritten;
// TestCalcMCP_DescriptionPointsAtTheContextTool fails if the builtin's
// wording moves and this no longer matches.
const calcHelpInRun = "Context op=help topic=Calc"

// calcToolDescriptor is the `calc` entry of tools/list. The schema is the
// builtin's own, through the wrapper table.
func calcToolDescriptor() loommcp.ToolDescriptor {
	return loommcp.ToolDescriptor{Name: "calc", Description: calcDescription(), InputSchema: builtinSchema("calc")}
}

// calcDescription is the `calc` tool's description: the Calc builtin's own,
// so the two cannot drift, with the help pointer rewritten for this transport
// and what a caller with no run needs to know: what it pays, and what the tool
// is not.
func calcDescription() string {
	desc := (&builtin.Calc{}).Description()
	desc = strings.Replace(desc, calcHelpInRun, "call the `context` tool with {\"op\": \"help\", \"topic\": \"Calc\"}", 1)
	return desc + " A refusal names its error code, where in expr it went wrong and what to write instead; its category is in the structured error." +
		" Each call takes one unit of your token's run rate; past it the call is refused with when to retry." +
		" Do NOT use it to reach stored data or start a run: it is a pure function of its arguments and reads nothing from this deployment." +
		" Do NOT use spawn_run to have an agent do arithmetic this tool can do."
}

// handleCalc forwards to connector.Calc under the session's principal, which
// the connector charges the call to. The evaluator itself never sees who is
// calling.
func handleCalc(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("calc: no connector wired")
	}
	res, err := env.connector.Calc(mcpPrincipalCtx(ctx), args)
	if err != nil {
		if errors.Is(err, runner.ErrRateLimited) {
			return runRateSpent("calc", err), nil
		}
		return toolErrFrom("calc", err), nil
	}
	return toolResultFromConnector(res), nil
}
