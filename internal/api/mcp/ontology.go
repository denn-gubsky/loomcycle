package mcp

import (
	"bytes"
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
	"A change may instead be {op: proposal, id}: an open proposal by its chunk id. A change or retire proposal plans as the direct changes accepting it makes (each carrying proposal: the id), checked again as accepting would; an add proposal plans as op add, risk none. Proposals and direct changes in one call are planned together, under one fingerprint. " +
	"{op: unretire, entry} plans clearing a retired entry's status (risk none). {op: release, seq}, the call's only change, plans rolling the ontology back to release seq: the direct changes that make it declare what the release declared (rename, edit, move, retire, unretire of entries in both; delete of an entry added since, retire when something rests on it; {op: add, seq, entry} of an entry only in the release), each carrying release: seq. An operator applies it with POST /v1/_ontology/rollback and the plan's fingerprint. " +
	"Per change it returns the entry, a risk (destroys: stored rows would be lost; stales: rows would need rework; none), counts of each kind of dependent across the tenant scope and every user and agent scope of the tenant ({n, effect, tenant_scope, other_scopes, by_status, sample}: sample ids from the tenant scope only), the subtree that goes with it and notes. " +
	"An edit stales what rests on the entry when it changes any of the entry's directives; one of the meaning alone is risk none. " +
	"A rename of a class something rests on carries job: the rows the rename job would rewrite per step, blocked (why it cannot carry the rename: a plan that could not count every scope) and collisions (stored keys that would collide under the new name: the tenant scope's listed, collisions_other_scopes counted). When blocked, the change carries a note starting rename_job_pending: the rename is refused, with or without a fingerprint; with collisions, a note starting rename_identity_collision. Otherwise the rename applies with the fingerprint and the job carries the new name through the ontology's own mentions of the class and the stored rows and classification records of the tenant scope and every user and agent scope (relations and classes nothing rests on rename with the plan, with no job; renaming or moving a concept starts the re-tag job, which carries the tags of what is filed under it to the new path). " +
	"Returns {tenant, fingerprint, content_version, changes, issues, new_issues, scopes_counted, scopes_skipped, complete}: issues are the ontology's advisories as it would read after every change, new_issues those it does not report now, and complete is false when some scope was not counted (the other-scope counts are then a floor). " +
	"The fingerprint names the ontology the plan was computed against. " +
	"content_version names what the ontology in force declares now, before the changes: it moves only when an in-force entry is added, changed, retired, moved or removed, never on a proposal, a review rule or a reorder. " +
	"op=releases lists the ontology's releases, newest first: the copies taken when it was confirmed (reason confirm), when an accepted change was applied (apply) or rolled back (rollback), and before either when it had moved without one (catch_up); the newest 50 are kept. " +
	"Returns {tenant, releases: [{seq, content_version, fingerprint, reason, actor, created_at, entries}]}. " +
	"op=release_diff compares release from with release to, or with the ontology as it stands when to is omitted, entry by entry. A rename keeps the entry's chunk, so it is one changed entry, never an add and a remove; proposals, the Review section, section headers and sibling order are not compared. " +
	"Returns {tenant, from, to (each {seq, current, content_version}), changes: [{chunk_id, kind, change (added, removed or changed), changed (renamed, moved, kind, status, meaning, directives), before, after (each {name, parent, parent_name, status, meaning, directives})}], issues}. " +
	"Both write nothing. A release keeps text an edit or an erasure removed from the ontology, which is why they are an operator's. " +
	"op=job reads the tenant's ontology job: the class rename or concept re-tag carrying a change through the stored rows of the tenant scope and every user and agent scope — the unfinished one, else the one updated last, else job null. It writes nothing. " +
	"Returns {tenant, job: {id, kind (class_rename or concept_retag), entry, from, to, to_tags, state (pending, running, blocked, done, abandoned), step, done (rows rewritten per step), error (why a blocked job cannot go on), remaining (rows still under an old name in the scope it is at), scope, scopes_done, scopes_total, created_at, updated_at}}. " +
	"op=resume_job gives the unfinished job one slice of work (about 20 seconds) under the ontology rename lease, from the scope and step it is at, as the next class rename or concept move would; a job whose change never reached the ontology is marked abandoned. Pass id (from op=job) to refuse the resume unless that job is still the unfinished one. Returns the same shape, as the slice left the job; call again while it is unfinished. " +
	"For an admin or a tenant operator only, holding write on the subject's document for merge_subjects (both documents) and bind_principal; plan reads the whole tenant. The tenant is your own unless you are an admin and pass another. " +
	"A refusal is {code, error}. merge_subjects: same_subject, not_a_subject, subject_not_in_scope, already_merged, principal_conflict, forbidden, merge_conflict. " +
	"bind_principal: subject_not_found, not_a_subject, binding_conflict (names the subject that holds the binding), principal_not_found, forbidden, invalid_request, unavailable. " +
	"plan: invalid_request (also a proposal accepting would refuse as it stands), invalid_body (over 64 KiB), entry_not_found (no in-force entry by that id, no open proposal by that id, or no ontology), forbidden, unavailable. " +
	"releases and release_diff: invalid_request (a from or to that is not a release's number), release_not_found (no such release, or no ontology to compare with), forbidden, unavailable. " +
	"job and resume_job: job_not_found (resume: no unfinished job, or not the one id names), job_busy (resume: a class rename or concept move is being checked or carried out right now), invalid_request (an id that is not a string), forbidden, unavailable. " +
	"Do NOT use it to rename a subject or change its type — edit the subject's document with the document tool."

// ontologyToolSchema is the `ontology` tool's input schema.
const ontologyToolSchema = `{
	"type": "object",
	"required": ["op"],
	"properties": {
		"op":        {"type": "string", "enum": ["merge_subjects", "bind_principal", "plan", "releases", "release_diff", "job", "resume_job"], "description": "merge_subjects (needs duplicate and survivor), bind_principal (needs subject and principal), plan (needs changes), releases, release_diff (needs from), job, or resume_job."},
		"duplicate": {"type": "string", "description": "merge_subjects: the subject that goes: its document id or natural key."},
		"survivor":  {"type": "string", "description": "merge_subjects: the subject that stays and receives the duplicate's facts and statements: its document id or natural key."},
		"subject":   {"type": "string", "description": "bind_principal: the subject to bind or clear: its document id, root chunk id or natural key."},
		"principal": {"type": "string", "description": "bind_principal: the user's subject id in the tenant, or \"\" to clear the binding. Required: leaving it out does not clear."},
		"changes":   {"type": "array", "minItems": 1, "maxItems": 50, "description": "plan: the changes to plan, each against the ontology as it stands.", "items": {
			"type": "object",
			"required": ["op"],
			"properties": {
				"op":     {"type": "string", "enum": ["retire", "unretire", "rename", "move", "edit", "delete", "add", "release", "proposal"]},
				"entry":  {"type": "string", "description": "Every op but proposal and release (required): the entry's chunk id; for add, a chunk id of release seq."},
				"seq":    {"type": "integer", "minimum": 1, "description": "release (required, nothing else with it, the call's only change) and add (required): a release's sequence number."},
				"id":     {"type": "string", "description": "proposal (required, nothing else with it): an open proposal's chunk id."},
				"name":   {"type": "string", "description": "rename: the new name, one line."},
				"parent": {"type": "string", "description": "move: the new parent's chunk id (an entry of the same section, or the section)."},
				"body":   {"type": "string", "description": "edit: the entry's new body (its meaning and directives), at most 32 KiB."}
			}
		}},
		"from":      {"type": "integer", "minimum": 1, "description": "release_diff (required): the earlier release's sequence number (seq from releases)."},
		"to":        {"type": "integer", "minimum": 1, "description": "release_diff: the later release's sequence number. Omit it to compare with the ontology as it stands now."},
		"id":        {"type": "string", "description": "resume_job: the job's id from op=job; the resume is refused unless that job is still the unfinished one. Omit it to resume whichever job is unfinished."},
		"tenant":    {"type": "string", "description": "Admin only: whose ontology. Ignored for a tenant operator, which always acts on its own tenant."}
	}
}`

// handleOntology serves the `ontology` tool. Each op delegates to the shared
// connector, which holds who may act, the tenant and the operation itself, so this
// transport cannot differ from HTTP or gRPC.
func handleOntology(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		Op        string  `json:"op"`
		Duplicate string  `json:"duplicate"`
		Survivor  string  `json:"survivor"`
		Subject   string  `json:"subject"`
		Principal *string `json:"principal"`
		Tenant    string  `json:"tenant"`

		// Decoded in op plan only, after the size bound and with unknown keys
		// refused, as HTTP does.
		Changes json.RawMessage `json:"changes"`

		// release_diff: decoded by releaseSeqArg, so a value that is not a
		// release's number is invalid_request, as HTTP's query parse answers.
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`

		// resume_job: decoded in its op, so a value that is not a string is
		// invalid_request, as HTTP's body decode refuses it.
		ID json.RawMessage `json:"id"`
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
		// HTTP's body bound, so this transport accepts no larger plan than HTTP does.
		if len(args) > connector.OntologyPlanMaxRequestBytes {
			return ontologyRefusal("plan", "invalid_body",
				fmt.Errorf("the plan arguments are %d bytes; at most %d", len(args), connector.OntologyPlanMaxRequestBytes)), nil
		}
		var changes []connector.OntologyChange
		if len(p.Changes) > 0 {
			dec := json.NewDecoder(bytes.NewReader(p.Changes))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&changes); err != nil {
				return ontologyRefusal("plan", "invalid_body",
					fmt.Errorf(`expected changes: [{"op": "retire|rename|move|edit|delete", "entry": "<chunk id>", …} or {"op": "proposal", "id": "<proposal chunk id>"}]: %w`, err)), nil
			}
		}
		res, err := env.connector.PlanOntology(ctx, connector.PlanOntologyRequest{Tenant: p.Tenant, Changes: changes})
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
	case "releases":
		res, err := env.connector.ListOntologyReleases(ctx, connector.ListOntologyReleasesRequest{Tenant: p.Tenant})
		if err != nil {
			return ontologyReleaseRefusal("releases", err), nil
		}
		return toolResultJSON(res), nil
	case "release_diff":
		from, err := releaseSeqArg(p.From, "from", false)
		if err != nil {
			return ontologyRefusal("release_diff", "invalid_request", err), nil
		}
		to, err := releaseSeqArg(p.To, "to", true)
		if err != nil {
			return ontologyRefusal("release_diff", "invalid_request", err), nil
		}
		res, err := env.connector.DiffOntologyReleases(ctx, connector.DiffOntologyReleasesRequest{Tenant: p.Tenant, From: from, To: to})
		if err != nil {
			return ontologyReleaseRefusal("release_diff", err), nil
		}
		return toolResultJSON(res), nil
	case "job":
		res, err := env.connector.GetOntologyJob(ctx, connector.GetOntologyJobRequest{Tenant: p.Tenant})
		if err != nil {
			return ontologyJobRefusal("job", err), nil
		}
		return toolResultJSON(res), nil
	case "resume_job":
		var id string
		if len(p.ID) > 0 && string(p.ID) != "null" {
			if err := json.Unmarshal(p.ID, &id); err != nil {
				return ontologyRefusal("resume_job", "invalid_request", errors.New("id must be a job's id, a string")), nil
			}
		}
		res, err := env.connector.ResumeOntologyJob(ctx, connector.ResumeOntologyJobRequest{Tenant: p.Tenant, ID: id})
		if err != nil {
			return ontologyJobRefusal("resume_job", err), nil
		}
		return toolResultJSON(res), nil
	}
	return toolErrValidation(`ontology: unknown op "`+p.Op+`" (must be merge_subjects, bind_principal, plan, releases, release_diff, job or resume_job)`,
		"pass op merge_subjects, bind_principal, plan, releases, release_diff, job or resume_job"), nil
}

// ontologyJobRefusal renders a job refusal as HTTP's {code, error}; an internal
// failure carries no text, as on HTTP.
func ontologyJobRefusal(op string, err error) *loommcp.CallToolResult {
	code := connector.OntologyJobCode(err)
	if code == "" {
		code, err = "job_failed", errors.New("the ontology job could not be read or resumed; see the server log")
	}
	return ontologyRefusal(op, code, err)
}

// releaseSeqArg reads a release's sequence number: a positive integer, or — when
// current is allowed — "current", null or an absent value for the ontology as it
// stands (0). Anything else is refused here with the sentence HTTP's query parse
// answers, so a string where a number belongs is invalid_request on both rather
// than an argument-decoding failure on this transport alone.
func releaseSeqArg(raw json.RawMessage, name string, current bool) (int64, error) {
	if current {
		var s string
		if len(raw) == 0 || string(raw) == "null" || (json.Unmarshal(raw, &s) == nil && s == "current") {
			return 0, nil
		}
	}
	var seq int64
	if err := json.Unmarshal(raw, &seq); err != nil || seq <= 0 {
		return 0, fmt.Errorf("%s must be a release's sequence number, a positive integer", name)
	}
	return seq, nil
}

// ontologyReleaseRefusal renders a release-read refusal as HTTP's {code, error}. An
// internal failure carries no text, as on HTTP: a store fault's text can carry a
// path or a DSN, and the connector has logged it.
func ontologyReleaseRefusal(op string, err error) *loommcp.CallToolResult {
	code := connector.OntologyReleaseCode(err)
	if code == "" {
		code, err = "release_read_failed", errors.New("the ontology releases could not be read; see the server log")
	}
	return ontologyRefusal(op, code, err)
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
