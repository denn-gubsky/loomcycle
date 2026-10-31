package mcp

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// ontologyToolDescription is the `ontology` tool's description, kept beside its
// handler.
const ontologyToolDescription = "Operator actions on the tenant ontology. " +
	"op=merge_subjects folds one SUBJECT (duplicate) into another (survivor) when both name the same entity — say person:ada-king and person:ada-lovelace. " +
	"Name each by its document id or its natural key; both must be subjects of the tenant scope. " +
	"In one transaction the duplicate's facts move under the survivor (their keys unchanged), every statement about the duplicate is copied onto the survivor and the original expired as merged (kept for as-of reads, never deleted), " +
	"and the duplicate stays as a retired document that keeps its key and path and records where it went (fields.merged_into, status merged). " +
	"A user the duplicate is bound to is bound to the survivor instead; if the survivor is bound to a different user the merge is refused (principal_conflict). " +
	"A `merged` event then goes to the tenant's memory/subjects channel for the links agent, unless your access-control rules hide part of the tenant's documents from you; not_announced then says why. " +
	"Returns {tenant, duplicate, survivor, facts_moved, statements_repointed, statements_retired, statements_left, not_announced}. " +
	"It cannot be undone by another call: choose the survivor with care. " +
	"For an admin or a tenant operator only, holding write on both documents. The tenant is your own unless you are an admin and pass another. " +
	"A refusal is {code, error}: same_subject, not_a_subject, subject_not_in_scope, already_merged, principal_conflict, forbidden, merge_conflict. " +
	"Do NOT use it to rename a subject or change its type — edit the subject's document with the document tool."

// ontologyToolSchema is the `ontology` tool's input schema.
const ontologyToolSchema = `{
	"type": "object",
	"required": ["op", "duplicate", "survivor"],
	"properties": {
		"op":        {"type": "string", "enum": ["merge_subjects"], "description": "merge_subjects."},
		"duplicate": {"type": "string", "description": "The subject that goes: its document id or natural key."},
		"survivor":  {"type": "string", "description": "The subject that stays and receives the duplicate's facts and statements: its document id or natural key."},
		"tenant":    {"type": "string", "description": "Admin only: whose ontology. Ignored for a tenant operator, which always acts on its own tenant."}
	}
}`

// handleOntology serves the `ontology` tool. Its one op, merge_subjects,
// delegates to the shared connector, which holds who may merge, the tenant and
// the merge itself, so this transport cannot differ from HTTP or gRPC.
func handleOntology(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		Op        string `json:"op"`
		Duplicate string `json:"duplicate"`
		Survivor  string `json:"survivor"`
		Tenant    string `json:"tenant"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return toolErr("invalid ontology arguments: " + err.Error()), nil
		}
	}
	if p.Op != "merge_subjects" {
		return toolErrValidation(`ontology: unknown op "`+p.Op+`" (must be merge_subjects)`, "pass op merge_subjects"), nil
	}
	res, err := env.connector.MergeSubjects(ctx, connector.MergeSubjectsRequest{
		Tenant: p.Tenant, Duplicate: p.Duplicate, Survivor: p.Survivor,
	})
	if err != nil {
		code := connector.SubjectMergeCode(err)
		if code == "" {
			return toolErrFrom("ontology merge_subjects", err), nil
		}
		// The HTTP error body's shape, so a caller branches on the same code
		// whichever transport it used.
		body, _ := json.Marshal(struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}{code, err.Error()})
		return toolErr(string(body)), nil
	}
	return toolResultJSON(res), nil
}
