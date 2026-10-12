package mcp

import (
	"context"
	"encoding/json"
	"errors"
	nethttp "net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// handleDeleteRunTree deletes a run tree, or every run of a session with their
// descendants. The operation — who may delete what, the walk, the tables — is
// Connector.DeleteRunTree, shared with the HTTP routes and the RPC; the tool
// gate (toolRequiredScope) has already required runs:delete.
func handleDeleteRunTree(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid delete_run_tree arguments: " + err.Error()), nil
	}
	if (p.RunID == "") == (p.SessionID == "") {
		return toolErrValidation("delete_run_tree: pass exactly one of run_id and session_id",
			"Pass run_id to delete that run and its descendants, or session_id to delete every run of that session."), nil
	}
	res, err := env.connector.DeleteRunTree(ctx, connector.DeleteRunTreeRequest{RunID: p.RunID, SessionID: p.SessionID})
	if err != nil {
		return deleteRunTreeToolErr(err), nil
	}
	return toolResultJSON(res), nil
}

// deleteRunTreeToolErr classifies a refusal by the status it carries, so the
// caller knows whether to fix its call, cancel a run first, or stop.
func deleteRunTreeToolErr(err error) *loommcp.CallToolResult {
	const prefix = "delete_run_tree"
	var hse interface{ HTTPStatus() int }
	if !errors.As(err, &hse) {
		return toolErrFrom(prefix, err)
	}
	msg := prefix + ": " + err.Error()
	info := tools.ErrorInfo{Category: tools.CategoryBusiness, Retryable: false}
	switch hse.HTTPStatus() {
	case nethttp.StatusBadRequest:
		return toolErrValidation(msg, "Correct the id and call again.")
	case nethttp.StatusForbidden:
		info.Category = tools.CategoryPermission
		info.Description = "This token may not delete runs. It needs the runs:delete scope, which is granted when a token is minted."
	case nethttp.StatusConflict:
		info.Description = "Nothing was deleted. A run in the tree is still live or paused: cancel it with cancel_run, wait for it to end, and call again."
	case nethttp.StatusUnprocessableEntity:
		info.Description = "Nothing was deleted. The tree is too large for one call: delete its sub-trees first, starting from the runs list_runs returns for parent_run_id."
	default:
		return toolErrFrom(prefix, err)
	}
	res := toolErr(msg)
	res.StructuredContent = loommcp.StructuredErrorJSON(info)
	return res
}
