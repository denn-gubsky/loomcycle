package mcp

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// reviewToolDescription is the `review` tool's description, kept beside its
// handler.
const reviewToolDescription = "Review proposed document relations (statements) in bulk, for a whole scope at once. " +
	"op=queue lists the statements awaiting review (status proposed unless filter.status names another), newest first, each with both ends' titles and documents and up to three evidence quotes; " +
	"filter narrows it (kind, kinds, method — an exact prefix such as rule: or rule:citation —, actor, document_id, under_path, from_document_id, to_document_id, from_under_path, to_under_path, min_confidence, created_before, created_after, has_judge_verdict, judge_verdict, the exact method_exact, kind_exact, from_path_prefix, from_unfiled, and review_route / judge_audit for the judge's queue and its audit sample). " +
	"With summarize: true it returns counts grouped by kind, method, the from-document's path prefix and judge verdict instead, plus the total: start there to see what the queue holds. Each group carries filter, which selects exactly that group: pass it to op=rule as it is. " +
	"Returns {tenant, scope, statements, truncated, next_cursor} or {summary, total}. " +
	"op=rule accepts or rejects many statements at once (action accept|reject), named by ids or by the queue's filter, at most 1000 per call (max). " +
	"It is a DRY RUN unless dry_run is false: the dry run returns matched (the count), a sample of 20 and the kind/method breakdown. " +
	"To rule, call again with dry_run false and expect_count equal to the dry run's matched; if the queue changed in between it is refused (queue_changed) and nothing is ruled. " +
	"reason is required to reject. Each statement is ruled as accept_statement / reject_statement would rule it, with a result per statement; one failure does not stop the rest. " +
	"Returns {ruling_id, matched, selected, skipped, to_rule, ruled, failed, items, breakdown}; statements you may not rule on are skipped and only counted. " +
	"op=revert {ruling_id} returns each statement of that ruling to its status before, if nothing changed it since (no new evidence, no maintenance, no later ruling); a rejected one is no longer refused when proposed again. Changed ones are listed and left; returns revert_id. It also undoes a review policy's ruling (the ruling_id an apply_policy call or a statement's get_edge names). " +
	"op=rulings lists the scope's bulk rulings and reverts, newest first (limit default 20, at most 100; after = next_cursor): id, action, ruled_by, actor, reason, selector (filter, or ids_count), counts, created_at, and reverted / reverted_by. An operator sees them all, a member only its own. Use it to find a ruling_id to revert. " +
	"For an admin or a tenant operator (holding write on each statement's from-document), or a member holding the access-control action review on the from-document's path. The tenant is your own unless you are an admin and pass another. " +
	"With scope user, scope_id names whose user documents: an admin or a tenant operator may name another subject of its tenant; a member only itself. " +
	"op=apply_policy runs the tenant's review policy (the ontology's Review section; describe_ontology shows it) over the proposals already in the queue, narrowed by filter (status may only be proposed). " +
	"It is a DRY RUN unless dry_run is false: the dry run returns matched, how many the policy would accept, reject, route to the judge or leave for a person, the count per rule and a sample of 20. " +
	"To apply, call again with dry_run false and expect_count equal to the dry run's matched: at most max (default and ceiling 1000) accepts and rejects are ruled, as one ruling op=revert undoes, the rest deferred to the next call. " +
	"Returns {source, version, matched, skipped, accept, reject, judge, human, to_rule, deferred, ruled, failed, marked, rules, ruling_id, items}. An admin or a tenant operator only. " +
	"A refusal is {code, error}: invalid_request, forbidden, queue_changed, ruling_not_found, unavailable. " +
	"Do NOT use it to rule on one statement you are looking at — use the document tool's accept_statement / reject_statement."

// reviewToolSchema is the `review` tool's input schema.
const reviewToolSchema = `{
	"type": "object",
	"required": ["op"],
	"properties": {
		"op":     {"type": "string", "enum": ["queue", "rule", "revert", "rulings", "apply_policy"], "description": "queue (list or summarize), rule (accept or reject in bulk), revert (undo a ruling), rulings (list past rulings), apply_policy (run the review policy over the queue)."},
		"scope":  {"type": "string", "enum": ["tenant", "user"], "description": "The document scope: tenant (default) or a user scope (your own, or scope_id's)."},
		"scope_id": {"type": "string", "description": "With scope user: whose user documents. An operator may name another subject of its tenant; a member only itself."},
		"tenant": {"type": "string", "description": "Admin only: whose documents. Ignored for anyone else, who always reviews their own tenant."},
		"filter": {"type": "object", "description": "queue / rule / apply_policy: which statements. Every field optional; they combine with AND. An empty filter on rule is the whole queue.", "properties": {
			"status":            {"type": "string", "description": "Statement status (default proposed)."},
			"kind":              {"type": "string"},
			"kinds":             {"type": "array", "items": {"type": "string"}},
			"method":            {"type": "string", "description": "Exact, case-sensitive prefix of the method, e.g. rule: or rule:citation."},
			"actor":             {"type": "string", "description": "The writer: the agent that wrote it in a run, else the user."},
			"document_id":       {"type": "string", "description": "Either end in this document."},
			"under_path":        {"type": "string", "description": "Either end in a document named at or under this Path."},
			"from_document_id":  {"type": "string"},
			"to_document_id":    {"type": "string"},
			"from_under_path":   {"type": "string"},
			"to_under_path":     {"type": "string"},
			"min_confidence":    {"type": "number", "description": "The writer's own confidence is at least this (0..1)."},
			"created_before":    {"type": "string", "description": "Unix nanoseconds or an RFC 3339 timestamp."},
			"created_after":     {"type": "string", "description": "Unix nanoseconds or an RFC 3339 timestamp."},
			"has_judge_verdict": {"type": "boolean", "description": "A judge's verdict is recorded (attributes.judge), or not."},
			"judge_verdict":     {"type": "string", "description": "The recorded verdict is exactly this: accept, reject or unsure."},
			"method_exact":      {"type": "string", "description": "The method is exactly this (rule:citation, not rule:citation_title)."},
			"kind_exact":        {"type": "string", "description": "The kind is exactly this, never widened to its declared sub-kinds."},
			"from_path_prefix":  {"type": "string", "description": "The from-document's path prefix is exactly this, as summarize groups it."},
			"from_unfiled":      {"type": "boolean", "description": "The from-document is filed under no Path name (summarize's empty prefix)."},
			"review_route":      {"type": "string", "enum": ["judge"], "description": "The proposals the review policy left for the judge agent."},
			"judge_audit":       {"type": "string", "enum": ["pending", "confirmed", "overturned"], "description": "A judge's acceptances in this audit state; pair it with status (asserted for pending or confirmed, rejected for overturned)."}
		}},
		"summarize":    {"type": "boolean", "description": "queue: counts by kind, method, path prefix and judge verdict instead of rows."},
		"limit":        {"type": "integer", "description": "queue: page size (default 500, at most 5000). rulings: default 20, at most 100."},
		"after":        {"type": "string", "description": "queue / rulings: the next_cursor of the previous page."},
		"order":        {"type": "string", "enum": ["created", "verified"], "description": "queue: created (default, newest first) or verified (least recently verified first)."},
		"action":       {"type": "string", "enum": ["accept", "reject"], "description": "rule: accept or reject."},
		"ids":          {"type": "array", "items": {"type": "string"}, "description": "rule: the statement ids, instead of a filter."},
		"reason":       {"type": "string", "description": "rule: why; required to reject."},
		"dry_run":      {"type": "boolean", "description": "rule / apply_policy: default true. Pass false, with expect_count, to rule."},
		"expect_count": {"type": "integer", "description": "rule / apply_policy: the dry run's matched count; required when dry_run is false."},
		"max":          {"type": "integer", "description": "rule / apply_policy: at most this many statements ruled (default and ceiling 1000)."},
		"ruling_id":    {"type": "string", "description": "revert: the ruling_id a rule call returned."}
	}
}`

// reviewToolFilter is the tool's filter: the connector's, with the two time
// bounds as text so a caller may pass a timestamp.
type reviewToolFilter struct {
	connector.ReviewQueueFilter
	CreatedBefore string `json:"created_before"`
	CreatedAfter  string `json:"created_after"`
}

func (f *reviewToolFilter) resolve() (connector.ReviewQueueFilter, error) {
	out := f.ReviewQueueFilter
	for _, t := range []struct {
		name, v string
		dst     **int64
	}{{"created_before", f.CreatedBefore, &out.CreatedBefore}, {"created_after", f.CreatedAfter, &out.CreatedAfter}} {
		if t.v == "" {
			continue
		}
		n, err := connector.ParseReviewInstant(t.v)
		if err != nil {
			return out, connector.WithMessage(connector.ErrStatementReviewInvalid, "filter."+t.name+": "+err.Error())
		}
		*t.dst = &n
	}
	return out, nil
}

// handleReview serves the `review` tool. Each op delegates to the shared
// connector, which holds who may review, as whom, the tenant and the rulings
// themselves, so this transport cannot differ from HTTP or gRPC.
func handleReview(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		Op          string            `json:"op"`
		Scope       string            `json:"scope"`
		ScopeID     string            `json:"scope_id"`
		Tenant      string            `json:"tenant"`
		Filter      *reviewToolFilter `json:"filter"`
		Summarize   bool              `json:"summarize"`
		Limit       int               `json:"limit"`
		After       string            `json:"after"`
		Order       string            `json:"order"`
		Action      string            `json:"action"`
		IDs         []string          `json:"ids"`
		Reason      string            `json:"reason"`
		DryRun      *bool             `json:"dry_run"`
		ExpectCount *int              `json:"expect_count"`
		Max         int               `json:"max"`
		RulingID    string            `json:"ruling_id"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return toolErr("invalid review arguments: " + err.Error()), nil
		}
	}
	var filter *connector.ReviewQueueFilter
	if p.Filter != nil {
		f, err := p.Filter.resolve()
		if err != nil {
			return reviewRefusal(p.Op, err), nil
		}
		filter = &f
	}
	switch p.Op {
	case "queue":
		req := connector.ListReviewQueueRequest{Tenant: p.Tenant, Scope: p.Scope, ScopeID: p.ScopeID, Summarize: p.Summarize, Limit: p.Limit, After: p.After, Order: p.Order}
		if filter != nil {
			req.Filter = *filter
		}
		res, err := env.connector.ListReviewQueue(ctx, req)
		if err != nil {
			return reviewRefusal(p.Op, err), nil
		}
		return toolResultJSON(res), nil
	case "rule":
		res, err := env.connector.RuleStatements(ctx, connector.RuleStatementsRequest{
			Tenant: p.Tenant, Scope: p.Scope, ScopeID: p.ScopeID, Action: p.Action, IDs: p.IDs, Filter: filter, Reason: p.Reason,
			DryRun: p.DryRun, ExpectCount: p.ExpectCount, Max: p.Max,
		})
		if err != nil {
			return reviewRefusal(p.Op, err), nil
		}
		return toolResultJSON(res), nil
	case "revert":
		res, err := env.connector.RevertRuling(ctx, connector.RevertRulingRequest{Tenant: p.Tenant, Scope: p.Scope, ScopeID: p.ScopeID, RulingID: p.RulingID})
		if err != nil {
			return reviewRefusal(p.Op, err), nil
		}
		return toolResultJSON(res), nil
	case "rulings":
		res, err := env.connector.ListRulings(ctx, connector.ListRulingsRequest{Tenant: p.Tenant, Scope: p.Scope, ScopeID: p.ScopeID, Limit: p.Limit, After: p.After})
		if err != nil {
			return reviewRefusal(p.Op, err), nil
		}
		return toolResultJSON(res), nil
	case "apply_policy":
		res, err := env.connector.ApplyReviewPolicy(ctx, connector.ApplyReviewPolicyRequest{
			Tenant: p.Tenant, Scope: p.Scope, ScopeID: p.ScopeID, Filter: filter, DryRun: p.DryRun, ExpectCount: p.ExpectCount, Max: p.Max,
		})
		if err != nil {
			return reviewRefusal(p.Op, err), nil
		}
		return toolResultJSON(res), nil
	}
	return toolErrValidation(`review: unknown op "`+p.Op+`" (must be queue, rule, revert, rulings or apply_policy)`, "pass op queue, rule, revert, rulings or apply_policy"), nil
}

// reviewRefusal renders a refusal as the HTTP error body's {code, error}, so a
// caller branches on the same code whichever transport it used.
// An error with no code is an internal failure and is classified by its type.
func reviewRefusal(op string, err error) *loommcp.CallToolResult {
	code := connector.StatementReviewCode(err)
	if code == "" {
		return toolErrFrom("review "+op, err)
	}
	body, _ := json.Marshal(struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}{code, err.Error()})
	return toolErr(string(body))
}
