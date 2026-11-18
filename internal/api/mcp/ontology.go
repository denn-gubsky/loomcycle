package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	"op=bind_principal records which user of the tenant a subject IS (person:ada-lovelace is the account u-ada): name the subject by document id or natural key, and the user by subject id; principal \"\" clears the binding. " +
	"Erasing that user then finds the subject, reports it, clears the binding and announces `erased` — the subject itself is never deleted. " +
	"One user per subject (clear a binding before binding another user) and one subject per user. The user must be registered in the tenant (any status). " +
	"Returns {tenant, document_id, root_chunk_id, natural_key, path, principal, previous, changed}. " +
	"op=plan says what changes to the ontology's entries WOULD do before any is made, and writes nothing: pass changes, 1 to 50 of {op, entry, name?, parent?, body?}, " +
	"where op is retire, rename (needs name), move (needs parent: an entry of the same section, or the section), edit (needs body: the entry's new body) or delete, and entry is the entry's chunk id (from describe_ontology or the ontology document). " +
	"Per change it returns the entry, a risk (destroys: stored rows would be lost; stales: rows would need rework; none), counts of each kind of dependent across the tenant scope and every user and agent scope of the tenant ({n, effect, tenant_scope, other_scopes, by_status, sample}: sample ids from the tenant scope only), the subtree that goes with it and notes. " +
	"Returns {tenant, fingerprint, changes, issues, new_issues, scopes_counted, scopes_skipped, complete}: issues are the ontology's advisories as it would read after every change, new_issues those it does not report now, and complete is false when some scope was not counted (the other-scope counts are then a floor). " +
	"The fingerprint names the ontology the plan was computed against. " +
	"For an admin or a tenant operator only, holding write on the subject's document for merge_subjects (both documents) and bind_principal; plan reads the whole tenant. The tenant is your own unless you are an admin and pass another. " +
	"A refusal is {code, error}. merge_subjects: same_subject, not_a_subject, subject_not_in_scope, already_merged, principal_conflict, forbidden, merge_conflict. " +
	"bind_principal: subject_not_found, not_a_subject, binding_conflict (names the subject that holds the binding), principal_not_found, forbidden, invalid_request, unavailable. " +
	"plan: invalid_request, invalid_body (over 64 KiB), entry_not_found (no in-force entry by that id, or no ontology), forbidden, unavailable. " +
	"Do NOT use it to rename a subject or change its type — edit the subject's document with the document tool."

// ontologyToolSchema is the `ontology` tool's input schema.
const ontologyToolSchema = `{
	"type": "object",
	"required": ["op"],
	"properties": {
		"op":        {"type": "string", "enum": ["merge_subjects", "bind_principal", "plan"], "description": "merge_subjects (needs duplicate and survivor), bind_principal (needs subject and principal) or plan (needs changes)."},
		"duplicate": {"type": "string", "description": "merge_subjects: the subject that goes: its document id or natural key."},
		"survivor":  {"type": "string", "description": "merge_subjects: the subject that stays and receives the duplicate's facts and statements: its document id or natural key."},
		"subject":   {"type": "string", "description": "bind_principal: the subject to bind or clear: its document id, root chunk id or natural key."},
		"principal": {"type": "string", "description": "bind_principal: the user's subject id in the tenant, or \"\" to clear the binding. Required: leaving it out does not clear."},
		"changes":   {"type": "array", "minItems": 1, "maxItems": 50, "description": "plan: the changes to plan, each against the ontology as it stands.", "items": {
			"type": "object",
			"required": ["op", "entry"],
			"properties": {
				"op":     {"type": "string", "enum": ["retire", "rename", "move", "edit", "delete"]},
				"entry":  {"type": "string", "description": "The entry's chunk id."},
				"name":   {"type": "string", "description": "rename: the new name, one line."},
				"parent": {"type": "string", "description": "move: the new parent's chunk id (an entry of the same section, or the section)."},
				"body":   {"type": "string", "description": "edit: the entry's new body (its meaning and directives), at most 32 KiB."}
			}
		}},
		"tenant":    {"type": "string", "description": "Admin only: whose ontology. Ignored for a tenant operator, which always acts on its own tenant."}
	}
}`

// handleOntology serves the `ontology` tool. Each op delegates to the shared
// connector, which holds who may act, the tenant and the operation itself, so this
// transport cannot differ from HTTP or gRPC.
func handleOntology(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if len(args) > connector.OntologyPlanMaxRequestBytes {
		// The plan's bound, held for every op: HTTP refuses a plan body over it, and
		// no other op's arguments come near it.
		return ontologyRefusal("arguments", "invalid_body",
			fmt.Errorf("ontology arguments are %d bytes; at most %d", len(args), connector.OntologyPlanMaxRequestBytes)), nil
	}
	var p struct {
		Op        string  `json:"op"`
		Duplicate string  `json:"duplicate"`
		Survivor  string  `json:"survivor"`
		Subject   string  `json:"subject"`
		Principal *string `json:"principal"`
		Tenant    string  `json:"tenant"`

		Changes []connector.OntologyChange `json:"changes"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return toolErr("invalid ontology arguments: " + err.Error()), nil
		}
	}
	switch p.Op {
	case "merge_subjects":
		res, err := env.connector.MergeSubjects(ctx, connector.MergeSubjectsRequest{
			Tenant: p.Tenant, Duplicate: p.Duplicate, Survivor: p.Survivor,
		})
		if err != nil {
			return ontologyRefusal("merge_subjects", connector.SubjectMergeCode(err), err), nil
		}
		return toolResultJSON(res), nil
	case "bind_principal":
		// An empty principal clears a binding, so a call that left the field out
		// must not be read as one: a model dropping an argument would otherwise
		// unbind the person an erasure is meant to find.
		if p.Principal == nil {
			return toolErrValidation("ontology bind_principal: principal is required",
				`pass principal: the user's subject id, or "" to clear the binding`), nil
		}
		res, err := env.connector.BindSubjectPrincipal(ctx, connector.BindSubjectPrincipalRequest{
			Tenant: p.Tenant, Subject: p.Subject, Principal: *p.Principal,
		})
		if err != nil {
			return ontologyRefusal("bind_principal", connector.SubjectBindCode(err), err), nil
		}
		return toolResultJSON(res), nil
	case "plan":
		res, err := env.connector.PlanOntology(ctx, connector.PlanOntologyRequest{Tenant: p.Tenant, Changes: p.Changes})
		if err != nil {
			code := connector.OntologyPlanCode(err)
			if code == "" {
				// As HTTP: a store fault's text can carry a path or a DSN, and the
				// connector has logged it.
				code, err = "plan_failed", errors.New("the plan could not be computed; see the server log")
			}
			return ontologyRefusal("plan", code, err), nil
		}
		return toolResultJSON(res), nil
	}
	return toolErrValidation(`ontology: unknown op "`+p.Op+`" (must be merge_subjects, bind_principal or plan)`,
		"pass op merge_subjects, bind_principal or plan"), nil
}

// ontologyRefusal renders a refusal as the HTTP error body's {code, error}, so a
// caller branches on the same code whichever transport it used. An error with no
// code is an internal failure and is classified by its type.
func ontologyRefusal(op, code string, err error) *loommcp.CallToolResult {
	if code == "" {
		return toolErrFrom("ontology "+op, err)
	}
	body, _ := json.Marshal(struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}{code, err.Error()})
	return toolErr(string(body))
}
