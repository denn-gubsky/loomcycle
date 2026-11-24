package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// handleCalc serves POST /v1/_calc (RFC GY P4a): the Calc tool outside a run.
// The body is the tool's input and a 200 its result. Who may call it is the
// route row's (any token but one held to the run plane); what each call costs
// and what it may not send are Server.Calc's, shared with MCP and gRPC.
func (s *Server) handleCalc(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxRequestBytes()))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("request body exceeds the %d-byte limit", maxErr.Limit))
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "empty body: expected the Calc tool's input, e.g. {\"expr\": \"2 + 2\"}")
		return
	}
	res, err := s.Calc(r.Context(), body)
	if err != nil {
		if errors.Is(err, runner.ErrRateLimited) {
			writeRateLimited(w, err)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if res.IsError {
		writeJSONErrorBody(w, calcRefusalStatus(res), calcRefusalBody(res))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(res.Text))
}

// calcRefusalStatus is the HTTP status of a Calc refusal, by its category: the
// caller fixes the call (400), the mathematics or a limit refused a
// well-formed one (422), or sending it again may work (503).
func calcRefusalStatus(res connector.ToolResult) int {
	if res.ErrorInfo != nil {
		switch res.ErrorInfo.Category {
		case tools.CategoryValidation:
			return http.StatusBadRequest
		case tools.CategoryTransient:
			return http.StatusServiceUnavailable
		}
	}
	return http.StatusUnprocessableEntity
}

// calcRefusalBody is the error envelope of a Calc refusal: the tool's error
// code as `code` and its detail as `error`, beside the refusal's own fields
// (detail, hint, pos) and its category. A refusal whose text is not the tool's
// JSON (none today) keeps the text as `error` under code tool_refused.
func calcRefusalBody(res connector.ToolResult) map[string]any {
	var refusal struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Hint   string `json:"hint"`
		Pos    *int   `json:"pos"`
	}
	env := map[string]any{"code": "tool_refused", "error": res.Text}
	if json.Unmarshal([]byte(res.Text), &refusal) == nil && refusal.Error != "" {
		env["code"], env["error"], env["detail"] = refusal.Error, refusal.Detail, refusal.Detail
		if refusal.Hint != "" {
			env["hint"] = refusal.Hint
		}
		if refusal.Pos != nil {
			env["pos"] = *refusal.Pos
		}
	}
	addErrorInfo(env, res.Text, calcRefusalInfoForHTTP(res.ErrorInfo))
	return env
}

// calcRefusalHelpInRun is the next step a Calc refusal names when the error
// carries no hint of its own: a call to the in-run Context tool, which a
// caller of this route does not have and which no route serves. The MCP
// transport rewrites the same sentence (calcRefusalForMCP);
// TestCalcAPI_ARefusalPointsAtHelpACallerCanRead fails if the builtin's
// wording moves and this no longer matches.
const calcRefusalHelpInRun = "Read Context op=help topic=Calc/errors"

// calcRefusalHelpOffRun names the same article by its topic, and the guide
// that carries the same table, for a caller with no run.
const calcRefusalHelpOffRun = "See the help topic Calc/errors (also docs/CALC.md#when-a-call-is-refused)"

// calcRefusalInfoForHTTP rewrites a refusal's help pointer for this route. A
// copy: the tool's own value, and so the run's text, is never written through.
// The gRPC twin carries no description, so it has no pointer to rewrite.
func calcRefusalInfoForHTTP(info *tools.ErrorInfo) *tools.ErrorInfo {
	if info == nil || !strings.Contains(info.Description, calcRefusalHelpInRun) {
		return info
	}
	out := *info
	out.Description = strings.Replace(out.Description, calcRefusalHelpInRun, calcRefusalHelpOffRun, 1)
	return &out
}
