package builtin

// document_review.go — reviewing proposed statements at scale (RFC FS P0).
//
// One corpus pass can propose a thousand relations, and the only ruling surface
// was accept_statement / reject_statement, one statement per call. This file
// adds the three pieces a reviewer needs to rule on them as a set:
//
//   - the queue's filters and its grouped summary (list_statements, through
//     statementQueueWhere), so a reviewer sees "1,118 cites by rule:citation
//     under /board/regulatory-external" before opening anything;
//   - RuleStatements: a bulk ruling by ids or by the queue's filter, dry run by
//     default, refused when the queue changed since the dry run (expect_count),
//     each statement ruled through applyRuling — the very checks and write a
//     single ruling makes — and recorded, so the set can be undone together;
//   - RevertRuling: the undo, which restores only what nothing touched since.
//
// Who may rule is decided twice: the connector says as whom the caller acts
// (an operator, or a member holding `review` somewhere), and each statement is
// asked here of its own from-document. What the caller may not rule on is
// skipped and counted, never named.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// statementRulingsDDL holds one row per bulk ruling, and one per revert of one
// (action revert, selector {"reverts": <ruling id>}): what it was, who made it,
// what it selected and what came of it. reverted_at and reverted_by mark a
// ruling's last revert. (reverted_by is in the CREATE itself, with no ALTER:
// the table has not shipped in a release yet.)
const statementRulingsDDL = `CREATE TABLE IF NOT EXISTS statement_rulings (
		id TEXT PRIMARY KEY, action TEXT NOT NULL, ruled_by TEXT NOT NULL, actor TEXT, reason TEXT,
		selector TEXT NOT NULL, matched INTEGER NOT NULL, ruled INTEGER NOT NULL, failed INTEGER NOT NULL,
		skipped INTEGER NOT NULL, created_at BIGINT NOT NULL, reverted_at BIGINT, reverted_by TEXT)`

// statementRulingItemsDDL holds, per statement a bulk ruling ruled, the
// ruling fields as they were before (prior) and a fingerprint of the statement
// just after: a revert restores prior only while the fingerprint still holds.
const statementRulingItemsDDL = `CREATE TABLE IF NOT EXISTS statement_ruling_items (
		ruling_id TEXT NOT NULL, edge_id TEXT NOT NULL, prior TEXT NOT NULL, fingerprint TEXT NOT NULL,
		PRIMARY KEY (ruling_id, edge_id))`

const (
	// reviewRulingMax is the most statements one bulk ruling rules.
	reviewRulingMax = 1000
	// reviewSampleSize is how many statements a dry run shows.
	reviewSampleSize = 20
	// reviewFieldMax bounds the free-text filters (actor, judge_verdict) and
	// a ruling id: each is a short label.
	reviewFieldMax = 256
	// reviewBindMax keeps a filter's bound values under SQLite's 32766: the
	// document lists can each run to listStatementsMaxDocs.
	reviewBindMax = 30000
)

// RulingAuthority is as whom a bulk ruling or a revert is made; the connector
// decides it from the principal.
type RulingAuthority int

const (
	// RuleAsOperator: an admin or a tenant operator. Each statement needs
	// write on its from-document, and link on both ends as accept_statement
	// asks.
	RuleAsOperator RulingAuthority = iota
	// RuleAsReviewer: a member. Each statement needs an explicit `review`
	// grant on its from-document's path.
	RuleAsReviewer
)

// reviewSelection adds the queue's own filters to list_statements' selection.
func (d *Document) reviewSelection(ctx context.Context, key sqlmem.ScopeKey, in docInput, where []string, args []any, readable func(string) (bool, error)) ([]string, []any, bool, *tools.Result, error) {
	refuse := func(msg, fix string) ([]string, []any, bool, *tools.Result, error) {
		r := errValidation("list_statements: "+msg, fix)
		return nil, nil, false, &r, nil
	}
	for _, end := range []struct{ id, alias string }{{in.FromDocumentID, "cf"}, {in.ToDocumentID, "ct"}} {
		if end.id == "" {
			continue
		}
		ok, err := readable(end.id)
		if err != nil || !ok {
			return nil, nil, !ok, nil, err
		}
		where = append(where, end.alias+".document_id = ?")
		args = append(args, end.id)
	}
	for _, end := range []struct{ field, path, alias string }{{"from_under_path", in.FromUnderPath, "cf"}, {"to_under_path", in.ToUnderPath, "ct"}} {
		if end.path == "" {
			continue
		}
		docs, none, refusal, err := d.readableDocsUnder(ctx, key, end.field, end.path, readable)
		if err != nil || refusal != nil || none {
			return nil, nil, none, refusal, err
		}
		if len(args)+len(docs) > reviewBindMax {
			return refuse("the path filters together name too many documents", "Narrow under_path, from_under_path or to_under_path.")
		}
		where = append(where, end.alias+".document_id IN ("+placeholders(len(docs))+")")
		args = append(args, docs...)
	}
	if in.Actor != "" {
		if len(in.Actor) > reviewFieldMax {
			return refuse(fmt.Sprintf("actor is longer than %d bytes", reviewFieldMax), "")
		}
		where = append(where, "e.actor = ?")
		args = append(args, in.Actor)
	}
	if c := in.MinConfidence; c != nil {
		if math.IsNaN(*c) || *c < 0 || *c > 1 {
			return refuse("min_confidence must be between 0 and 1", "")
		}
		where = append(where, "e.confidence >= ?")
		args = append(args, *c)
	}
	if in.CreatedBefore != nil {
		where = append(where, "e.created_at < ?")
		args = append(args, *in.CreatedBefore)
	}
	if in.CreatedAfter != nil {
		where = append(where, "e.created_at > ?")
		args = append(args, *in.CreatedAfter)
	}
	if in.HasJudgeVerdict != nil {
		cond := " IS NULL"
		if *in.HasJudgeVerdict {
			cond = " IS NOT NULL"
		}
		where = append(where, d.judgeVerdictExpr()+cond)
	}
	if in.JudgeVerdict != "" {
		if len(in.JudgeVerdict) > reviewFieldMax {
			return refuse(fmt.Sprintf("judge_verdict is longer than %d bytes", reviewFieldMax), "")
		}
		where = append(where, d.judgeVerdictExpr()+" = ?")
		args = append(args, in.JudgeVerdict)
	}
	for _, ex := range []struct{ field, v, col string }{{"method_exact", in.MethodExact, "e.method"}, {"kind_exact", in.KindExact, "e.kind"}} {
		if ex.v == "" {
			continue
		}
		if len(ex.v) > reviewFieldMax {
			return refuse(fmt.Sprintf("%s is longer than %d bytes", ex.field, reviewFieldMax), "")
		}
		where = append(where, ex.col+" = ?")
		args = append(args, ex.v)
	}
	if in.FromPathPrefix == "" && !in.FromUnfiled {
		return where, args, false, nil, nil
	}
	return d.fromPathPrefixSelection(ctx, key, in, where, args, readable)
}

// fromPathPrefixSelection selects by the from-document's path prefix exactly
// as summarizeStatements groups it (pathPrefix over the same documentNames),
// so a summary group's filter selects that group and nothing else: a document
// filed under two names belongs to the prefix of its first name in sort
// order only, and from_unfiled is the group with no prefix — a from-document
// filed nowhere, or a from-chunk that is gone. Unfiled is spelled as "not one
// of the filed documents the caller may read", so a document with no name
// needs no listing.
func (d *Document) fromPathPrefixSelection(ctx context.Context, key sqlmem.ScopeKey, in docInput, where []string, args []any, readable func(string) (bool, error)) ([]string, []any, bool, *tools.Result, error) {
	refuse := func(msg, fix string) ([]string, []any, bool, *tools.Result, error) {
		r := errValidation("list_statements: "+msg, fix)
		return nil, nil, false, &r, nil
	}
	switch {
	case in.FromPathPrefix != "" && in.FromUnfiled:
		return refuse("from_path_prefix and from_unfiled select different documents", "Pass one of them.")
	case len(in.FromPathPrefix) > reviewFieldMax:
		return refuse(fmt.Sprintf("from_path_prefix is longer than %d bytes", reviewFieldMax), "")
	}
	names, err := d.documentNames(ctx, key)
	if err != nil {
		return nil, nil, false, nil, err
	}
	// Only documents the caller may read are listed, for unfiled too: a
	// statement from one it may not read is withheld by the selection's own
	// access filter either way, and a list (or a refusal) that counted them
	// would tell the caller how many it cannot see.
	var docs []string
	for doc, n := range names {
		p := pathPrefix(n)
		if p == "" || (!in.FromUnfiled && p != in.FromPathPrefix) {
			continue
		}
		ok, err := readable(doc)
		if err != nil {
			return nil, nil, false, nil, err
		}
		if ok {
			docs = append(docs, doc)
		}
	}
	sort.Strings(docs)
	if len(docs) > listStatementsMaxDocs || len(args)+len(docs) > reviewBindMax {
		return refuse("the from-document path filter names more documents than one call binds",
			"Narrow the selection by from_document_id or from_under_path.")
	}
	bound := make([]any, len(docs))
	for i, doc := range docs {
		bound[i] = doc
	}
	if in.FromUnfiled {
		if len(docs) > 0 {
			where = append(where, "(cf.document_id IS NULL OR cf.document_id NOT IN ("+placeholders(len(docs))+"))")
			args = append(args, bound...)
		}
		return where, args, false, nil, nil
	}
	if len(docs) == 0 {
		return nil, nil, true, nil, nil
	}
	return append(where, "cf.document_id IN ("+placeholders(len(docs))+")"), append(args, bound...), false, nil, nil
}

// summaryGroupFilter is the filter that selects exactly group g of a summary
// asked with base: base, narrowed by each of the group's keys exactly.
// Kind and Method in base stay (the group lies inside them) and only narrow.
func summaryGroupFilter(base connector.ReviewQueueFilter, g connector.ReviewQueueGroup) *connector.ReviewQueueFilter {
	f := base
	f.Kinds = append([]string(nil), base.Kinds...)
	f.KindExact, f.MethodExact = g.Kind, g.Method
	f.FromPathPrefix, f.FromUnfiled = g.FromPathPrefix, g.FromPathPrefix == ""
	if g.JudgeVerdict == "" {
		f.HasJudgeVerdict, f.JudgeVerdict = boolRef(false), ""
	} else {
		f.HasJudgeVerdict, f.JudgeVerdict = nil, g.JudgeVerdict
	}
	return &f
}

func boolRef(b bool) *bool { return &b }

// judgeVerdictExpr reads attributes.judge.verdict on alias e: NULL when the
// statement carries none. attributes is JSON text on both tiers; SQLite's
// json_extract fails on text that is not JSON, so it is asked only of text that
// is, and a stored "" is no verdict on postgres. A verdict recorded as "" is
// none too (NULLIF): the summary groups it with the statements carrying none,
// so has_judge_verdict=false must select it, on either tier.
func (d *Document) judgeVerdictExpr() string {
	if d.docSQL().Tier() == "postgres" {
		return `NULLIF((CASE WHEN e.attributes IS NULL OR e.attributes = '' THEN NULL ELSE (e.attributes::jsonb -> 'judge' ->> 'verdict') END), '')`
	}
	return `NULLIF((CASE WHEN json_valid(e.attributes) THEN json_extract(e.attributes, '$.judge.verdict') END), '')`
}

// summarizeStatements is list_statements with summarize: the queue's counts
// grouped by kind × method × the from-document's path prefix × judge verdict.
// The grouping by document runs in SQL and the fold onto path prefixes here,
// because a document's Path names live outside the scope's database.
func (d *Document) summarizeStatements(ctx context.Context, key sqlmem.ScopeKey, where []string, args []any, none bool) (tools.Result, error) {
	type groupKey struct{ kind, method, prefix, verdict string }
	counts := map[groupKey]int{}
	total := 0
	if !none {
		names, err := d.documentNames(ctx, key)
		if err != nil {
			return errFrom("list_statements: "+err.Error(), err), nil
		}
		stmt := `SELECT e.kind, e.method, cf.document_id, ` + d.judgeVerdictExpr() + `, COUNT(*)
			FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id
			WHERE ` + strings.Join(where, " AND ") + `
			GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4 LIMIT ? OFFSET ?`
		for offset := 0; ; {
			res, err := d.query(ctx, key, stmt, append(append([]any{}, args...), searchFilterPage, offset)...)
			if err != nil {
				return errFrom("list_statements: "+err.Error(), err), nil
			}
			for _, r := range res.Rows {
				n := asInt(r[4])
				counts[groupKey{asStr(r[0]), asStr(r[1]), pathPrefix(names[asStr(r[2])]), asStr(r[3])}] += n
				total += n
			}
			offset += len(res.Rows)
			// Truncated: the store's row cap cut the page short of searchFilterPage.
			if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
				break
			}
		}
	}
	groups := make([]connector.ReviewQueueGroup, 0, len(counts))
	for k, n := range counts {
		groups = append(groups, connector.ReviewQueueGroup{Kind: k.kind, Method: k.method, FromPathPrefix: k.prefix, JudgeVerdict: k.verdict, Count: n})
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return fmt.Sprint(a.Kind, "\x00", a.Method, "\x00", a.FromPathPrefix, "\x00", a.JudgeVerdict) <
			fmt.Sprint(b.Kind, "\x00", b.Method, "\x00", b.FromPathPrefix, "\x00", b.JudgeVerdict)
	})
	return jsonResult(map[string]any{"summary": groups, "total": total})
}

// pathPrefix is the first two segments of a document's first Path name (in
// sort order, so a document filed twice always lands in one group), or "" for
// a document filed nowhere.
func pathPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	first := names[0]
	for _, n := range names[1:] {
		if n < first {
			first = n
		}
	}
	segs := strings.Split(strings.Trim(first, "/"), "/")
	if len(segs) > 2 {
		segs = segs[:2]
	}
	return "/" + strings.Join(segs, "/")
}

// reviewFilterInput is a queue filter as list_statements' input, built field by
// field rather than through JSON: a JSON round trip would carry each request
// value into every input field, the raw-SQL escape hatch among them.
func reviewFilterInput(f connector.ReviewQueueFilter) docInput {
	return docInput{
		Op:              "list_statements",
		Status:          f.Status,
		Kind:            f.Kind,
		Kinds:           f.Kinds,
		MethodPrefix:    f.Method,
		Actor:           f.Actor,
		DocumentID:      f.DocumentID,
		UnderPath:       f.UnderPath,
		FromDocumentID:  f.FromDocumentID,
		ToDocumentID:    f.ToDocumentID,
		FromUnderPath:   f.FromUnderPath,
		ToUnderPath:     f.ToUnderPath,
		JudgeVerdict:    f.JudgeVerdict,
		MethodExact:     f.MethodExact,
		KindExact:       f.KindExact,
		FromPathPrefix:  f.FromPathPrefix,
		FromUnfiled:     f.FromUnfiled,
		MinConfidence:   f.MinConfidence,
		CreatedBefore:   f.CreatedBefore,
		CreatedAfter:    f.CreatedAfter,
		HasJudgeVerdict: f.HasJudgeVerdict,
	}
}

// reviewInvalid / reviewForbidden wrap a refusal onto its sentinel.
func reviewInvalid(msg string) error {
	return connector.WithMessage(connector.ErrStatementReviewInvalid, msg)
}
func reviewForbidden(msg string) error {
	return connector.WithMessage(connector.ErrStatementReviewForbidden, msg)
}

// reviewToolRefusal maps a refusal the Document returned onto the sentinels: a
// permission refusal is forbidden, any other is the request's fault.
func reviewToolRefusal(r tools.Result) error {
	if r.Error != nil && r.Error.Category == tools.CategoryPermission {
		return reviewForbidden(r.Text)
	}
	return reviewInvalid(r.Text)
}

// ListReviewQueue is the review queue for the connector: list_statements with
// the queue's filters, through Execute's gates, so every gate and the read
// filter of a direct call apply. scope is tenant or user.
func (d *Document) ListReviewQueue(ctx context.Context, scope string, req connector.ListReviewQueueRequest) (connector.ListReviewQueueResult, error) {
	out := connector.ListReviewQueueResult{Scope: scope, Statements: []json.RawMessage{}}
	if req.Limit < 0 || req.Limit > edgeGetEdgesMax {
		return out, reviewInvalid(fmt.Sprintf("limit is at most %d (0 for the default)", edgeGetEdgesMax))
	}
	in := reviewFilterInput(req.Filter)
	in.Scope = scope
	in.Summarize = req.Summarize
	in.Limit = req.Limit
	in.After = req.After
	in.Order = req.Order
	res, err := d.executeInput(ctx, in, nil)
	if err != nil {
		return out, err
	}
	if res.IsError {
		return out, reviewToolRefusal(res)
	}
	var body struct {
		Statements []json.RawMessage            `json:"statements"`
		Truncated  bool                         `json:"truncated"`
		NextCursor string                       `json:"next_cursor"`
		Summary    []connector.ReviewQueueGroup `json:"summary"`
		Total      int                          `json:"total"`
	}
	if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
		return out, fmt.Errorf("list_statements: decoding the page: %w", err)
	}
	if body.Statements != nil {
		out.Statements = body.Statements
	}
	out.Truncated, out.NextCursor = body.Truncated, body.NextCursor
	if req.Summarize {
		out.Summary, out.Total = body.Summary, body.Total
		if out.Summary == nil {
			out.Summary = []connector.ReviewQueueGroup{}
		}
		for i := range out.Summary {
			out.Summary[i].Filter = summaryGroupFilter(req.Filter, out.Summary[i])
		}
	}
	return out, nil
}

// reviewCandidate is one statement a bulk ruling selected, with its ends'
// display fields as the queue renders them.
type reviewCandidate struct {
	s    statementRow
	ends map[string]string
}

// reviewOp opens the scope for a bulk ruling or a revert: the operator-plane
// check a single ruling makes, then the scope and its schema.
func (d *Document) reviewOp(ctx context.Context, scope string) (sqlmem.ScopeKey, store.MemoryScope, error) {
	if d.SqlMem == nil || d.Store == nil {
		return sqlmem.ScopeKey{}, "", connector.WithMessage(connector.ErrStatementReviewUnavailable, "reviewing statements needs SQL Memory; none is configured")
	}
	if !authorIsOperatorPlane(ctx) {
		return sqlmem.ScopeKey{}, "", reviewForbidden("only the operator plane rules on statements, never an agent inside a run")
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return key, mscope, reviewForbidden(err.Error())
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return key, mscope, err
	}
	return key, mscope, nil
}

// mayRule asks whether the caller may rule on s as as: see RulingAuthority.
// The ends are the statement's documents (fromDoc, toDoc; "" for an end whose
// chunk is gone, which asks about the whole scope).
func (d *Document) mayRule(ctx context.Context, acl *docACL, as RulingAuthority, fromDoc, toDoc string) (bool, error) {
	if as == RuleAsReviewer {
		return acl.grants(ctx, "review", fromDoc)
	}
	for _, q := range []struct{ verb, doc string }{{"write", fromDoc}, {"link", fromDoc}, {"link", toDoc}} {
		ok, err := acl.permits(ctx, q.verb, q.doc)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// selectForRuling resolves a bulk ruling's ids or filter to the statements the
// caller may read, in the queue's order, and counts them. At most max are
// returned.
func (d *Document) selectForRuling(ctx context.Context, key sqlmem.ScopeKey, req connector.RuleStatementsRequest, max int) (matched int, picked []reviewCandidate, missing []string, err error) {
	const display = `SELECT ` + statementCols + `, cf.title, cf.document_id, ct.title, ct.document_id
		FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id`
	ends := func(r []any) map[string]string {
		return map[string]string{
			"from_title": asStr(r[statementColCount]), "from_document_id": asStr(r[statementColCount+1]),
			"to_title": asStr(r[statementColCount+2]), "to_document_id": asStr(r[statementColCount+3]),
		}
	}
	if req.Filter == nil {
		acl := d.newDocACL(ctx, key)
		seen := map[string]bool{}
		for _, id := range req.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			res, err := d.query(ctx, key, display+` WHERE e.id = ?`, id)
			if err != nil {
				return 0, nil, nil, err
			}
			ok := len(res.Rows) == 1
			if ok {
				e := ends(res.Rows[0])
				// A statement an end of which the caller may not read is answered
				// exactly as one that does not exist.
				for _, doc := range []string{e["from_document_id"], e["to_document_id"]} {
					if ok && doc != "" {
						if ok, err = acl.readableOrAll(ctx, doc); err != nil {
							return 0, nil, nil, err
						}
					}
				}
				if ok {
					picked = append(picked, reviewCandidate{s: scanStatement(res.Rows[0]), ends: e})
				}
			}
			if !ok {
				missing = append(missing, id)
			}
		}
		return len(picked), picked, missing, nil
	}
	in := reviewFilterInput(*req.Filter)
	where, args, none, refusal, err := d.statementQueueWhere(ctx, key, in)
	if err != nil {
		return 0, nil, nil, err
	}
	if refusal != nil {
		return 0, nil, nil, reviewInvalid(refusal.Text)
	}
	if none {
		return 0, nil, nil, nil
	}
	cond := strings.Join(where, " AND ")
	res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id WHERE `+cond, args...)
	if err != nil {
		return 0, nil, nil, err
	}
	if len(res.Rows) == 1 {
		matched = asInt(res.Rows[0][0])
	}
	res, err = d.query(ctx, key, display+` WHERE `+cond+` ORDER BY e.created_at DESC, e.id DESC LIMIT ?`, append(args, max)...)
	if err != nil {
		return 0, nil, nil, err
	}
	if res.Truncated {
		// The store's row cap is below what this call would rule: refuse rather
		// than rule a different set than the dry run counted.
		return 0, nil, nil, reviewInvalid(fmt.Sprintf("the store returns fewer rows than max (%d); pass a smaller max", max))
	}
	for _, r := range res.Rows {
		picked = append(picked, reviewCandidate{s: scanStatement(r), ends: ends(r)})
	}
	return matched, picked, nil, nil
}

// readableOrAll is readable for a caller that may have no set at all.
func (a *docACL) readableOrAll(ctx context.Context, docID string) (bool, error) {
	if a == nil {
		return true, nil
	}
	return a.readable(ctx, docID)
}

// RuleStatements is a bulk ruling: see connector.Connector.RuleStatements.
// scope is tenant or user; as is the authority the connector decided.
func (d *Document) RuleStatements(ctx context.Context, scope string, req connector.RuleStatementsRequest, as RulingAuthority) (connector.RuleStatementsResult, error) {
	out := connector.RuleStatementsResult{Scope: scope, Action: req.Action, DryRun: req.DryRun == nil || *req.DryRun, Breakdown: []connector.RulingBreakdown{}}
	var accept bool
	switch req.Action {
	case "accept":
		accept = true
	case "reject":
	default:
		return out, reviewInvalid(`action must be "accept" or "reject"`)
	}
	reason := strings.TrimSpace(req.Reason)
	if !accept && reason == "" {
		return out, reviewInvalid("reason is required to reject: say why, so the proposer never re-proposes it and a later reader knows")
	}
	max := req.Max
	switch {
	case max < 0 || max > reviewRulingMax:
		return out, reviewInvalid(fmt.Sprintf("max must be between 1 and %d", reviewRulingMax))
	case max == 0:
		max = reviewRulingMax
	}
	switch {
	case len(req.IDs) > 0 && req.Filter != nil:
		return out, reviewInvalid("name the statements by ids or by a filter, not both")
	case len(req.IDs) == 0 && req.Filter == nil:
		return out, reviewInvalid("name the statements: ids, or a filter (an empty filter is the whole queue)")
	case len(req.IDs) > max:
		return out, reviewInvalid(fmt.Sprintf("%d ids are more than max (%d)", len(req.IDs), max))
	}
	for _, id := range req.IDs {
		if id == "" || len(id) > reviewFieldMax {
			return out, reviewInvalid("ids holds an empty or overlong id")
		}
	}
	if !out.DryRun && req.ExpectCount == nil {
		return out, reviewInvalid("expect_count is required to rule: pass the matched count of the dry run")
	}
	key, mscope, err := d.reviewOp(ctx, scope)
	if err != nil {
		return out, err
	}
	matched, picked, missing, err := d.selectForRuling(ctx, key, req, max)
	if err != nil {
		return out, err
	}
	out.Matched, out.Selected = matched, len(picked)
	acl := d.newDocACL(ctx, key)
	var toRule []reviewCandidate
	for _, c := range picked {
		ok, err := d.mayRule(ctx, acl, as, c.ends["from_document_id"], c.ends["to_document_id"])
		if err != nil {
			return out, err
		}
		if ok {
			toRule = append(toRule, c)
		} else {
			out.Skipped++
		}
	}
	out.ToRule = len(toRule)
	if out.DryRun {
		out.Breakdown = rulingBreakdown(toRule)
		sample := toRule[:min(len(toRule), reviewSampleSize)]
		ids := make([]string, len(sample))
		for i, c := range sample {
			ids[i] = c.s.ID
		}
		ev, counts, err := d.readableEvidenceFor(ctx, key, ids, edgeEvidencePreview)
		if err != nil {
			return out, err
		}
		for _, c := range sample {
			b, _ := json.Marshal(d.statementJSON(ctx, c.s, c.ends, ev[c.s.ID], counts[c.s.ID], false))
			out.Sample = append(out.Sample, b)
		}
		return out, nil
	}
	if *req.ExpectCount != matched {
		return out, connector.WithMessage(connector.ErrStatementReviewQueueChanged,
			fmt.Sprintf("the selection now matches %d statements, not the %d expected; nothing was ruled — run the dry run again", matched, *req.ExpectCount))
	}
	return d.applyBulkRuling(ctx, key, mscope, req, accept, reason, out, toRule, missing)
}

// applyBulkRuling rules on each statement of toRule and records the ruling.
func (d *Document) applyBulkRuling(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, req connector.RuleStatementsRequest, accept bool, reason string, out connector.RuleStatementsResult, toRule []reviewCandidate, missing []string) (connector.RuleStatementsResult, error) {
	op := "reject_statement"
	if accept {
		op = "accept_statement"
	}
	rulingID := newDocID()
	ruledBy := "bulk:" + rulingID
	selector, _ := json.Marshal(struct {
		IDs    []string                     `json:"ids,omitempty"`
		Filter *connector.ReviewQueueFilter `json:"filter,omitempty"`
		Max    int                          `json:"max,omitempty"`
	}{req.IDs, req.Filter, req.Max})
	// The ruling is recorded before anything is ruled, so a statement carrying
	// its ruled_by always has a ruling to be undone with.
	if err := d.exec(ctx, key, `INSERT INTO statement_rulings
		(id, action, ruled_by, actor, reason, selector, matched, ruled, failed, skipped, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		rulingID, req.Action, operatorRuler(ctx), nullIfEmpty(rulingActor(ctx)), nullIfEmpty(reason),
		string(selector), out.Matched, out.Skipped, time.Now().UnixNano()); err != nil {
		return out, err
	}
	out.RulingID = rulingID
	out.Items = make([]connector.RulingItem, 0, len(toRule)+len(missing))
	for _, id := range missing {
		out.Items = append(out.Items, connector.RulingItem{ID: id, Error: rulingNotFound, Message: "no such statement: " + id})
		out.Failed++
	}
	var ruled []reviewCandidate
	for _, c := range toRule {
		item, ok := d.ruleOne(ctx, key, mscope, op, rulingID, ruledBy, accept, reason, c)
		out.Items = append(out.Items, item)
		if ok {
			ruled = append(ruled, c)
			out.Ruled++
		} else {
			out.Failed++
		}
	}
	out.Breakdown = rulingBreakdown(ruled)
	if err := d.exec(ctx, key, `UPDATE statement_rulings SET ruled = ?, failed = ? WHERE id = ?`, out.Ruled, out.Failed, rulingID); err != nil {
		// The statements are ruled and each is recorded; only the header's
		// counts are stale, and the audit record carries them.
		logStatementReviewFault(ctx, "statement ruling counts not recorded", rulingID, err)
	}
	return out, nil
}

// ruleOne rules one statement of a bulk ruling: its record first, then the
// ruling through applyRuling, then the record's fingerprint. A failure is the
// item's alone.
func (d *Document) ruleOne(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op, rulingID, ruledBy string, accept bool, reason string, c reviewCandidate) (connector.RulingItem, bool) {
	item := connector.RulingItem{ID: c.s.ID}
	fail := func(code, msg string) (connector.RulingItem, bool) {
		item.Error, item.Message = code, msg
		return item, false
	}
	if err := d.exec(ctx, key, `INSERT INTO statement_ruling_items (ruling_id, edge_id, prior, fingerprint) VALUES (?, ?, ?, '')`,
		rulingID, c.s.ID, rulingPriorOf(&c.s)); err != nil {
		return fail("failed", "the ruling could not be recorded, so the statement was not ruled: "+err.Error())
	}
	res, err := d.applyRuling(ctx, key, mscope, op, c.s.ID, accept, reason, ruledBy)
	if err != nil || res.refusal != nil {
		if derr := d.exec(ctx, key, `DELETE FROM statement_ruling_items WHERE ruling_id = ? AND edge_id = ?`, rulingID, c.s.ID); derr != nil {
			logStatementReviewFault(ctx, "statement ruling item not removed", rulingID, derr)
		}
		if err != nil {
			return fail("failed", err.Error())
		}
		return fail(res.code, res.refusal.Text)
	}
	item.Status = res.status
	fp, err := d.statementFingerprint(ctx, key, c.s.ID)
	if err == nil {
		err = d.exec(ctx, key, `UPDATE statement_ruling_items SET prior = ?, fingerprint = ? WHERE ruling_id = ? AND edge_id = ?`,
			rulingPriorOf(res.prior), fp, rulingID, c.s.ID)
	}
	if err != nil {
		// Ruled, but a revert will find no fingerprint and leave it: undo it by
		// hand if needed.
		item.Message = "ruled, but not recorded for undo"
		logStatementReviewFault(ctx, "statement ruling fingerprint not recorded", rulingID, err)
	}
	return item, true
}

// rulingBreakdown counts candidates by kind and method, largest first.
func rulingBreakdown(cs []reviewCandidate) []connector.RulingBreakdown {
	counts := map[[2]string]int{}
	for _, c := range cs {
		counts[[2]string{c.s.Kind, c.s.Method}]++
	}
	out := make([]connector.RulingBreakdown, 0, len(counts))
	for k, n := range counts {
		out = append(out, connector.RulingBreakdown{Kind: k[0], Method: k[1], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// rulingPrior is what a ruling replaces on a statement, and what its revert
// puts back.
type rulingPrior struct {
	Status            string   `json:"status"`
	OperatorRuledAt   *int64   `json:"operator_ruled_at,omitempty"`
	RuledBy           string   `json:"ruled_by,omitempty"`
	JudgedAt          *int64   `json:"judged_at,omitempty"`
	JudgeReason       string   `json:"judge_reason,omitempty"`
	JudgedBy          string   `json:"judged_by,omitempty"`
	VerdictConfidence *float64 `json:"verdict_confidence,omitempty"`
}

func rulingPriorOf(s *statementRow) string {
	b, _ := json.Marshal(rulingPrior{
		Status: s.Status, OperatorRuledAt: s.OperatorRuledAt, RuledBy: s.RuledBy, JudgedAt: s.JudgedAt,
		JudgeReason: s.JudgeReason, JudgedBy: s.JudgedBy, VerdictConfidence: s.VerdictConfidence,
	})
	return string(b)
}

// statementFingerprint hashes everything about a statement a later change
// would touch: its envelope as stored (attributes byte for byte) and every
// evidence row's source and span. Equal fingerprints mean nothing changed it —
// no ruling, no maintenance, no new or moved evidence.
func (d *Document) statementFingerprint(ctx context.Context, key sqlmem.ScopeKey, id string) (string, error) {
	res, err := d.query(ctx, key, `SELECT status, from_id, to_id, kind, method, confidence, valid_at, invalid_at,
		expired_at, verified_at, discriminator, attributes, judged_at, judge_reason, judged_by, verdict_confidence,
		operator_ruled_at, ruled_by FROM chunk_edges WHERE id = ?`, id)
	if err != nil {
		return "", err
	}
	if len(res.Rows) != 1 {
		return "", fmt.Errorf("statement %s is gone", id)
	}
	h := sha256.New()
	for _, v := range res.Rows[0] {
		fmt.Fprintf(h, "%v\x00", v)
	}
	ev, err := d.query(ctx, key, `SELECT id, chunk_id, session_id, event_seq, ref_scope, ref_natural_key, char_start, char_end, stale
		FROM chunk_edge_evidence WHERE edge_id = ? ORDER BY id`, id)
	if err != nil {
		return "", err
	}
	if ev.Truncated {
		return "", fmt.Errorf("statement %s has more evidence rows than the store returns", id)
	}
	for _, r := range ev.Rows {
		h.Write([]byte{'\x01'})
		for _, v := range r {
			fmt.Fprintf(h, "%v\x00", v)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RevertRuling undoes a bulk ruling: see connector.Connector.RevertRuling.
func (d *Document) RevertRuling(ctx context.Context, scope string, req connector.RevertRulingRequest, as RulingAuthority) (connector.RevertRulingResult, error) {
	out := connector.RevertRulingResult{Scope: scope, RulingID: req.RulingID, Items: []connector.RulingItem{}}
	if req.RulingID == "" || len(req.RulingID) > reviewFieldMax {
		return out, reviewInvalid("ruling_id is required: the id a bulk ruling returned")
	}
	key, mscope, err := d.reviewOp(ctx, scope)
	if err != nil {
		return out, err
	}
	res, err := d.query(ctx, key, `SELECT action, selector FROM statement_rulings WHERE id = ?`, req.RulingID)
	if err != nil {
		return out, err
	}
	if len(res.Rows) == 0 {
		return out, connector.WithMessage(connector.ErrStatementReviewRulingNotFound, "no bulk ruling "+req.RulingID+" in this scope")
	}
	out.Action = asStr(res.Rows[0][0])
	if out.Action == rulingActionRevert {
		return out, reviewInvalid("ruling " + req.RulingID + " is a revert, which is not undone: rule on its statements again")
	}
	// A bulk ruling's statements record bulk:<id>; a review policy's record
	// the rule that ruled each (policy:<rule>@<version>), which revertOne
	// takes from the statement, its fingerprint proving nothing changed it.
	ruledBy := "bulk:" + req.RulingID
	if isPolicyRuling(asStr(res.Rows[0][1])) {
		ruledBy = ""
	}
	items, err := d.query(ctx, key, `SELECT edge_id, prior, fingerprint FROM statement_ruling_items WHERE ruling_id = ? ORDER BY edge_id`, req.RulingID)
	if err != nil {
		return out, err
	}
	if items.Truncated {
		return out, fmt.Errorf("revert_ruling: the store returned fewer of the ruling's statements than it holds")
	}
	acl := d.newDocACL(ctx, key)
	for _, r := range items.Rows {
		id, priorText, fp := asStr(r[0]), asStr(r[1]), asStr(r[2])
		item, outcome, err := d.revertOne(ctx, key, mscope, acl, as, ruledBy, id, priorText, fp)
		if err != nil {
			return out, err
		}
		switch outcome {
		case revertSkipped:
			out.Skipped++
			continue
		case revertChanged:
			out.Changed++
		case revertDone:
			out.Reverted++
		}
		out.Items = append(out.Items, item)
	}
	d.recordRevert(ctx, key, req.RulingID, len(items.Rows), &out)
	return out, nil
}

// rulingActionRevert is a revert's action in statement_rulings.
const rulingActionRevert = "revert"

// recordRevert records a revert as a row of its own and marks the ruling it
// undid with it. A failed write is logged, not returned: the statements are
// reverted either way, and the audit record carries the counts.
func (d *Document) recordRevert(ctx context.Context, key sqlmem.ScopeKey, rulingID string, considered int, out *connector.RevertRulingResult) {
	revertID, now := newDocID(), time.Now().UnixNano()
	selector, _ := json.Marshal(map[string]string{"reverts": rulingID})
	if err := d.exec(ctx, key, `INSERT INTO statement_rulings
		(id, action, ruled_by, actor, reason, selector, matched, ruled, failed, skipped, created_at)
		VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
		revertID, rulingActionRevert, operatorRuler(ctx), nullIfEmpty(rulingActor(ctx)), string(selector),
		considered, out.Reverted, out.Changed, out.Skipped, now); err != nil {
		logStatementReviewFault(ctx, "statement ruling revert not recorded", rulingID, err)
		revertID = ""
	} else {
		out.RevertID = revertID
	}
	if err := d.exec(ctx, key, `UPDATE statement_rulings SET reverted_at = ?, reverted_by = ? WHERE id = ?`,
		now, nullIfEmpty(revertID), rulingID); err != nil {
		logStatementReviewFault(ctx, "statement ruling revert not marked", rulingID, err)
	}
}

// ListRulings lists the scope's bulk rulings and reverts, newest first: see
// connector.Connector.ListRulings. An operator sees each of them; a reviewer
// only those it made (actor). A ruling's statements are never named: what it
// selected by id is a count.
func (d *Document) ListRulings(ctx context.Context, scope string, req connector.ListRulingsRequest, as RulingAuthority) (connector.ListRulingsResult, error) {
	out := connector.ListRulingsResult{Scope: scope, Rulings: []connector.RulingRecord{}}
	limit := req.Limit
	switch {
	case limit < 0 || limit > rulingsPageMax:
		return out, reviewInvalid(fmt.Sprintf("limit must be between 1 and %d", rulingsPageMax))
	case limit == 0:
		limit = rulingsPageDefault
	}
	where, args := []string{"1 = 1"}, []any{}
	if req.After != "" {
		c, ok := decodeRulingCursor(req.After)
		if !ok {
			return out, reviewInvalid("after is not a cursor this listing returned: pass next_cursor from the previous page")
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, c.CreatedAt, c.CreatedAt, c.ID)
	}
	key, _, err := d.reviewOp(ctx, scope)
	if err != nil {
		return out, err
	}
	if as == RuleAsReviewer {
		actor := rulingActor(ctx)
		if actor == "" {
			return out, nil
		}
		where = append(where, "actor = ?")
		args = append(args, actor)
	}
	res, err := d.query(ctx, key, `SELECT id, action, ruled_by, actor, reason, selector, matched, ruled, failed, skipped,
		created_at, reverted_at, reverted_by FROM statement_rulings WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return out, err
	}
	rows := res.Rows
	// Truncated: the store's row cap cut the page short; the rest is a page on.
	if more := len(rows) > limit || res.Truncated; more && len(rows) > 0 {
		rows = rows[:min(len(rows), limit)]
		last := rows[len(rows)-1]
		out.NextCursor = rulingCursor{CreatedAt: asInt64Val(last[10]), ID: asStr(last[0])}.encode()
	}
	for _, r := range rows {
		rec := connector.RulingRecord{
			ID: asStr(r[0]), Action: asStr(r[1]), RuledBy: asStr(r[2]), Actor: asStr(r[3]), Reason: asStr(r[4]),
			Selector: rulingSelectorOf(asStr(r[5])), Matched: asInt(r[6]), Ruled: asInt(r[7]), Failed: asInt(r[8]),
			Skipped: asInt(r[9]), CreatedAt: asInt64Val(r[10]), RevertedBy: asStr(r[12]),
		}
		if r[11] != nil {
			rec.Reverted, rec.RevertedAt = true, asInt64Val(r[11])
		}
		out.Rulings = append(out.Rulings, rec)
	}
	return out, nil
}

const (
	// rulingsPageDefault / rulingsPageMax size a page of ListRulings.
	rulingsPageDefault = 20
	rulingsPageMax     = 100
)

// rulingSelectorOf reads a ruling's stored selector for the listing: the ids
// only as a count, and a review policy's ruling with the policy it applied.
func rulingSelectorOf(text string) connector.RulingSelector {
	var sel struct {
		IDs     []string                     `json:"ids"`
		Filter  *connector.ReviewQueueFilter `json:"filter"`
		Max     int                          `json:"max"`
		Reverts string                       `json:"reverts"`
		Policy  *connector.RulingPolicy      `json:"policy"`
	}
	_ = json.Unmarshal([]byte(text), &sel)
	return connector.RulingSelector{Filter: sel.Filter, IDsCount: len(sel.IDs), Max: sel.Max, Reverts: sel.Reverts, Policy: sel.Policy}
}

// rulingCursor is ListRulings' paging position: the last row's
// (created_at, id).
type rulingCursor struct {
	CreatedAt int64  `json:"c"`
	ID        string `json:"i"`
}

func (c rulingCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeRulingCursor(s string) (rulingCursor, bool) {
	var c rulingCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" || len(c.ID) > reviewFieldMax {
		return c, false
	}
	return c, true
}

// rulingActorKey carries the subject making a ruling (WithRulingActor).
type rulingActorKey struct{}

// WithRulingActor records who makes a ruling, for a ctx whose run identity
// names another subject: an operator ruling on a member's user scope acts as
// itself, and its ruling is recorded as its own.
func WithRulingActor(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, rulingActorKey{}, subject)
}

// rulingActor is the subject a ruling, a revert or a listing is made by:
// WithRulingActor's, else the run identity's user.
func rulingActor(ctx context.Context) string {
	if v, ok := ctx.Value(rulingActorKey{}).(string); ok {
		return v
	}
	return tools.RunIdentity(ctx).UserID
}

type revertOutcome int

const (
	revertDone revertOutcome = iota
	revertChanged
	revertSkipped
)

// revertOne restores one statement of a ruling, or says why not. ruledBy is
// what the ruling recorded on it; "" for a review policy's ruling, whose
// statements each record their own rule.
func (d *Document) revertOne(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL, as RulingAuthority, ruledBy, id, priorText, fp string) (connector.RulingItem, revertOutcome, error) {
	item := connector.RulingItem{ID: id}
	changed := func(msg string) (connector.RulingItem, revertOutcome, error) {
		item.Error, item.Message = "changed", msg
		return item, revertChanged, nil
	}
	s, err := d.statementByID(ctx, key, id)
	if err != nil {
		return item, 0, err
	}
	if s == nil {
		return changed("the statement was removed since the ruling")
	}
	fromDoc, toDoc, err := d.statementEndDocs(ctx, key, s)
	if err != nil {
		return item, 0, err
	}
	readable := true
	for _, doc := range []string{fromDoc, toDoc} {
		if readable && doc != "" {
			if readable, err = acl.readableOrAll(ctx, doc); err != nil {
				return item, 0, err
			}
		}
	}
	if !readable {
		return item, revertSkipped, nil
	}
	if ok, err := d.mayRule(ctx, acl, as, fromDoc, toDoc); err != nil || !ok {
		return item, revertSkipped, err
	}
	if ruledBy == "" && strings.HasPrefix(s.RuledBy, "policy:") {
		ruledBy = s.RuledBy
	}
	if s.RuledBy != ruledBy || ruledBy == "" {
		return changed("ruled again since (" + s.RuledBy + ")")
	}
	now, err := d.statementFingerprint(ctx, key, id)
	if err != nil {
		return item, 0, err
	}
	if fp == "" || now != fp {
		return changed("changed since the ruling: new evidence, maintenance or a judge's verdict")
	}
	var prior rulingPrior
	if err := json.Unmarshal([]byte(priorText), &prior); err != nil || prior.Status == "" {
		return changed("the ruling's record of it cannot be read")
	}
	upd, err := d.docSQL().Exec(ctx, key, d.docSQL().Rebind(`UPDATE chunk_edges SET status = ?, operator_ruled_at = ?, ruled_by = ?,
		judged_at = ?, judge_reason = ?, judged_by = ?, verdict_confidence = ?
		WHERE id = ? AND status = ? AND ruled_by = ?`), []any{prior.Status, prior.OperatorRuledAt, nullIfEmpty(prior.RuledBy),
		prior.JudgedAt, nullIfEmpty(prior.JudgeReason), nullIfEmpty(prior.JudgedBy), prior.VerdictConfidence,
		id, s.Status, ruledBy}, 0)
	if err != nil {
		return item, 0, err
	}
	if upd.RowsAffected == 0 {
		return changed("changed while it was reverted")
	}
	d.publishEdgeChange(ctx, key, mscope, "revert_ruling", id, s.FromID, s.ToID, s.Kind, prior.Status)
	item.Status = prior.Status
	return item, revertDone, nil
}

// statementEndDocs returns the documents holding a statement's two ends ("" for
// an end whose chunk is gone).
func (d *Document) statementEndDocs(ctx context.Context, key sqlmem.ScopeKey, s *statementRow) (string, string, error) {
	var docs [2]string
	for i, id := range []string{s.FromID, s.ToID} {
		row, ok, err := d.getChunkRow(ctx, key, id)
		if err != nil {
			return "", "", err
		}
		if ok {
			docs[i] = row.DocumentID
		}
	}
	return docs[0], docs[1], nil
}

// logStatementReviewFault records a bookkeeping write that failed after the
// statements themselves were ruled: the ruling stands, its record is short.
func logStatementReviewFault(ctx context.Context, msg, rulingID string, err error) {
	slog.ErrorContext(ctx, "document: "+msg, "ruling_id", rulingID, "tenant", tools.RunIdentity(ctx).TenantID, "err", err)
}
