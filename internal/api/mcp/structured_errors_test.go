package mcp

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A builtin's failure reaches an MCP client as structuredContent: the category,
// retryability, next step and correct call format. The call format names the
// tools the way an MCP client knows them — the lowercased meta-tools — or it
// would send the client to a tool it does not have.
func TestToolResultFromConnector_CarriesTheFailureStructure(t *testing.T) {
	res := toolResultFromConnector(connector.ToolResult{
		Text:    `Document: unknown argument "text" — nothing was done.`,
		IsError: true,
		ErrorInfo: &tools.ErrorInfo{
			Category:    tools.CategoryValidation,
			Description: "Pass only this tool's own arguments, at the top level.",
			CallFormat: &tools.CallFormat{
				Tool: "Document", Op: "create_chunk",
				Example:   json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"T"}`),
				Reference: &tools.CallRef{Tool: "Context", Input: json.RawMessage(`{"op":"help","topic":"Document/create_chunk"}`)},
			},
		},
	})
	if !res.IsError {
		t.Fatal("the failure is not marked isError")
	}
	var sc struct {
		ErrorCategory     string `json:"errorCategory"`
		IsRetryable       bool   `json:"isRetryable"`
		Description       string `json:"description"`
		CorrectCallFormat struct {
			Tool      string          `json:"tool"`
			Op        string          `json:"op"`
			Example   json.RawMessage `json:"example"`
			Reference struct {
				Tool string `json:"tool"`
			} `json:"reference"`
		} `json:"correctCallFormat"`
	}
	if err := json.Unmarshal(res.StructuredContent, &sc); err != nil {
		t.Fatalf("structuredContent: %v (%s)", err, res.StructuredContent)
	}
	if sc.ErrorCategory != "validation" || sc.IsRetryable || sc.Description == "" {
		t.Errorf("structuredContent = %s", res.StructuredContent)
	}
	cf := sc.CorrectCallFormat
	if cf.Tool != "document" || cf.Op != "create_chunk" || cf.Reference.Tool != "context" || len(cf.Example) == 0 {
		t.Errorf("correctCallFormat = %+v; want MCP tool names and the example", cf)
	}
}

// An unclassified failure that still carries the call format sends only that:
// inventing a category would claim something nobody decided.
func TestToolResultFromConnector_UnclassifiedSendsOnlyTheCallFormat(t *testing.T) {
	res := toolResultFromConnector(connector.ToolResult{
		Text:      "move: missing required field: to",
		IsError:   true,
		ErrorInfo: &tools.ErrorInfo{CallFormat: &tools.CallFormat{Tool: "Path", Op: "mv"}},
	})
	var sc map[string]any
	if err := json.Unmarshal(res.StructuredContent, &sc); err != nil {
		t.Fatalf("structuredContent: %v (%s)", err, res.StructuredContent)
	}
	if _, ok := sc["errorCategory"]; ok || len(sc) != 1 || sc["correctCallFormat"] == nil {
		t.Errorf("structuredContent = %s; want only correctCallFormat", res.StructuredContent)
	}
}
