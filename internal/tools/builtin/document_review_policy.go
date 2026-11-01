package builtin

// document_review_policy.go — the tenant's review policy at work (RFC FS P2).
//
// The policy is the ontology's Review section (memory/ontology_review.go): an
// ordered list of rules, each deciding a proposal accept, reject, judge or
// human. It runs at two moments:
//
//   - when a proposal is filed (propose_statement, classify): the first rule
//     that matches decides it. accept and reject rule it as an operator's
//     ruling would — through applyRuling, recorded as a ruling of the run that
//     filed it, so revert_ruling undoes the run's policy rulings together —
//     and the statement records ruled_by policy:<rule>@<ontology version>.
//     judge marks it (attributes.review_route) for the judge; human leaves it.
//   - on demand over the queue (ApplyReviewPolicy), through the bulk path.
//
// WITHOUT a Review section nothing here runs at filing: the per-kind
// @auto_assert check in proposeStatement stays exactly as it was, and the
// policy it implies (memory.ImpliedReviewPolicy) only serves describe_ontology
// and an on-demand application. A Review section that does not parse rules
// nothing — every proposal waits for a person — and @auto_assert does not apply
// beside it either: the section is the whole policy.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// reviewRouteJudge is attributes.review_route on a proposal the policy left
// for the judge.
const reviewRouteJudge = "judge"

// policyInForce is the tenant's review policy as the runtime applies it.
type policyInForce struct {
	policy memrank.ReviewPolicy
	// version is the ontology version the policy was read at: what a ruling
	// records (policy:<rule>@<version>).
	version int
	// section: the Review section governs; false for the policy @auto_assert
	// implies.
	section bool
	// concepts are the chunk ids of the schemes' concepts (target:concept).
	concepts map[string]bool
}

// reviewPolicyInForce reads the policy in force. ok is false when none is: no
// confirmed ontology (a draft governs nothing), or a store fault reading it.
func (d *Document) reviewPolicyInForce(ctx context.Context) (policyInForce, bool) {
	if d.Store == nil || d.SqlMem == nil {
		return policyInForce{}, false
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.DocumentID == "" || !read.Confirmed {
		return policyInForce{}, false
	}
	pf := policyInForce{version: read.RootRevision, concepts: map[string]bool{}}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			pf.concepts[c.ChunkID] = true
		}
	}
	if read.Review != nil {
		pf.policy, pf.section = *read.Review, true
		return pf, true
	}
	// The kinds relationView marks auto-asserting, read the same way.
	var kinds []string
	if hasSection(read.Sections, memrank.SectionRelations) {
		for _, r := range memrank.EffectiveRelations(read.Relations, true, true) {
			if r.AutoAssert {
				kinds = append(kinds, r.Name)
			}
		}
	}
	pf.policy = memrank.ImpliedReviewPolicy(kinds)
	return pf, true
}

// policyCandidate is a statement with what the policy's rules ask of it.
type policyCandidate struct {
	reviewCandidate
	facts memrank.ReviewFacts
	// revision: a correction filed beside another statement (attributes.revision_of).
	// The policy never rules one: accepting it would set a second live
	// statement beside the one it revises, as @auto_assert never did.
	revision bool
}

// policyCandidateSelect reads a statement, its ends and the facts the rules
// ask: the to-end document's status (a placeholder's is unresolved), whether
// the to-end is a subject node, and the evidence rows, all and unresolved (no
// chunk, or stale).
const policyCandidateSelect = `SELECT ` + statementCols + `, cf.title, cf.document_id, ct.title, ct.document_id,
	COALESCE(dt.status, ''),
	CASE WHEN dt.root_chunk_id = e.to_id AND EXISTS (SELECT 1 FROM chunk_memory_meta m
		WHERE m.chunk_id = e.to_id AND COALESCE(m.natural_key, '') <> '') THEN 1 ELSE 0 END,
	(SELECT COUNT(*) FROM chunk_edge_evidence v WHERE v.edge_id = e.id),
	(SELECT COUNT(*) FROM chunk_edge_evidence v WHERE v.edge_id = e.id AND (COALESCE(v.chunk_id, '') = '' OR v.stale <> 0))
	FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id
	LEFT JOIN documents dt ON dt.id = ct.document_id`

func (pf policyInForce) candidate(r []any) policyCandidate {
	n := statementColCount
	c := policyCandidate{reviewCandidate: reviewCandidate{s: scanStatement(r), ends: map[string]string{
		"from_title": asStr(r[n]), "from_document_id": asStr(r[n+1]),
		"to_title": asStr(r[n+2]), "to_document_id": asStr(r[n+3]),
	}}}
	s := c.s
	f := memrank.ReviewFacts{Kind: s.Kind, Method: s.Method, Confidence: s.Confidence}
	evidence, unresolved := asInt(r[n+6]), asInt(r[n+7])
	f.QuotesResolved = evidence > 0 && unresolved == 0
	fromDoc, toDoc := c.ends["from_document_id"], c.ends["to_document_id"]
	if fromDoc != "" && toDoc != "" {
		self := fromDoc == toDoc
		f.Self = &self
	}
	switch {
	case pf.concepts[s.ToID]:
		f.Target = memrank.ReviewTargetConcept
	case asInt(r[n+5]) == 1:
		f.Target = memrank.ReviewTargetSubject
	case toDoc == "":
		// The to-end's chunk is gone: no target condition is met.
	case asStr(r[n+4]) == placeholderStatus:
		f.Target = memrank.ReviewTargetPlaceholder
	default:
		f.Target = memrank.ReviewTargetHeld
	}
	by, _ := s.Attributes[attrClassifiedBy].(string)
	f.RuleMatched = strings.HasPrefix(s.Method, "rule:") ||
		(s.Method == edgeMethodClassify && (by == classifiedByRules || by == classifiedByVote))
	c.facts = f
	_, c.revision = s.Attributes["revision_of"]
	return c
}

// decide is the policy's decision on a candidate: the rule that matched and its
// action, or no rule (index 0) and human.
func (pf policyInForce) decide(c policyCandidate) (memrank.ReviewRule, string) {
	if c.revision {
		return memrank.ReviewRule{}, memrank.ReviewHuman
	}
	r, ok := pf.policy.Decide(c.facts)
	if !ok {
		return memrank.ReviewRule{}, memrank.ReviewHuman
	}
	return r, r.Action
}

// ruledBy is what a statement the rule ruled records.
func (pf policyInForce) ruledBy(r memrank.ReviewRule) string {
	return fmt.Sprintf("policy:%d@%d", r.Index, pf.version)
}

// reason is a policy ruling's reason: the rule, as written.
func (pf policyInForce) reason(r memrank.ReviewRule) string {
	return fmt.Sprintf("review policy rule %d (ontology version %d): %s", r.Index, pf.version, r.Text)
}

// policyCandidateByID reads one statement as a candidate, or nil.
func (d *Document) policyCandidateByID(ctx context.Context, key sqlmem.ScopeKey, pf policyInForce, id string) (*policyCandidate, error) {
	res, err := d.query(ctx, key, policyCandidateSelect+` WHERE e.id = ?`, id)
	if err != nil || len(res.Rows) == 0 {
		return nil, err
	}
	c := pf.candidate(res.Rows[0])
	return &c, nil
}

// ruleOnFiling runs the Review section's policy over a statement just filed as
// a proposal (res), and returns res as it then stands. A fault leaves the
// proposal as it was filed — waiting for a person — and is logged: the filing
// itself succeeded.
func (d *Document) ruleOnFiling(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, pf policyInForce, res edgeWriteResult) edgeWriteResult {
	if !pf.section || !pf.policy.Applied() || res.Status != edgeStatusProposed {
		return res
	}
	c, err := d.policyCandidateByID(ctx, key, pf, res.ID)
	if err != nil || c == nil || c.s.Status != edgeStatusProposed {
		if err != nil {
			logStatementReviewFault(ctx, "review policy: the filed proposal could not be read", "", err)
		}
		return res
	}
	rule, action := pf.decide(*c)
	switch action {
	case memrank.ReviewAccept, memrank.ReviewReject:
		accept := action == memrank.ReviewAccept
		rulingID, err := d.filingRuling(ctx, key, pf, action)
		if err != nil {
			logStatementReviewFault(ctx, "review policy: the ruling could not be recorded; the proposal waits for a person", "", err)
			return res
		}
		op := "reject_statement"
		if accept {
			op = "accept_statement"
		}
		item, ok := d.ruleOne(ctx, key, mscope, op, rulingID, pf.ruledBy(rule), accept, pf.reason(rule), c.reviewCandidate)
		ruled, failed := 0, 1
		if ok {
			ruled, failed = 1, 0
			res.Status, res.RuledBy, res.RulingID = item.Status, pf.ruledBy(rule), rulingID
		}
		if err := d.exec(ctx, key, `UPDATE statement_rulings SET matched = matched + 1, ruled = ruled + ?, failed = failed + ? WHERE id = ?`,
			ruled, failed, rulingID); err != nil {
			logStatementReviewFault(ctx, "statement ruling counts not recorded", rulingID, err)
		}
	case memrank.ReviewJudge:
		if _, err := d.routeProposal(ctx, key, c.s, reviewRouteJudge); err != nil {
			logStatementReviewFault(ctx, "review policy: the proposal could not be routed to the judge", "", err)
		} else {
			res.ReviewRoute = reviewRouteJudge
		}
	default:
		// human. A routing an earlier policy made no longer holds.
		if _, err := d.routeProposal(ctx, key, c.s, ""); err != nil {
			logStatementReviewFault(ctx, "review policy: a stale judge routing could not be cleared", "", err)
		}
	}
	return res
}

// filingRuling is the ruling a filing-time policy decision is recorded under:
// one per run, ontology version and action, so revert_ruling undoes what the
// policy ruled on a run's proposals together; one per decision outside a run.
// The first proposal of the run to be ruled creates it.
func (d *Document) filingRuling(ctx context.Context, key sqlmem.ScopeKey, pf policyInForce, action string) (string, error) {
	run := tools.RunID(ctx)
	id := newDocID()
	if run != "" {
		h := sha256.Sum256([]byte("policy\x00" + run + "\x00" + strconv.Itoa(pf.version) + "\x00" + action))
		id = hex.EncodeToString(h[:16])
	}
	selector, _ := json.Marshal(map[string]any{"policy": map[string]any{"version": pf.version, "run_id": run}})
	err := d.exec(ctx, key, `INSERT INTO statement_rulings
		(id, action, ruled_by, actor, reason, selector, matched, ruled, failed, skipped, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0, ?)`,
		id, action, "policy@"+strconv.Itoa(pf.version), nullIfEmpty(tools.RunIdentity(ctx).UserID),
		"the review policy, as each proposal was filed", string(selector), time.Now().UnixNano())
	if err != nil {
		// Another proposal of this run made it first.
		res, qerr := d.query(ctx, key, `SELECT 1 FROM statement_rulings WHERE id = ?`, id)
		if qerr == nil && len(res.Rows) == 1 {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

// routeProposal sets a proposal's attributes.review_route to route ("" removes
// it), when it differs. Only a proposal is routed: a ruling may have landed
// since it was read.
func (d *Document) routeProposal(ctx context.Context, key sqlmem.ScopeKey, s statementRow, route string) (bool, error) {
	if cur, _ := s.Attributes[attrReviewRoute].(string); cur == route {
		return false, nil
	}
	attrs := make(map[string]any, len(s.Attributes)+1)
	for k, v := range s.Attributes {
		attrs[k] = v
	}
	if route == "" {
		delete(attrs, attrReviewRoute)
	} else {
		attrs[attrReviewRoute] = route
	}
	var arg any
	if len(attrs) > 0 {
		b, err := json.Marshal(attrs)
		if err != nil {
			return false, err
		}
		arg = string(b)
	}
	n, err := d.execCount(ctx, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ? AND status = ?`, arg, s.ID, edgeStatusProposed)
	return n > 0, err
}

// isPolicyRuling reports whether a ruling's selector is a policy's: its
// statements record ruled_by policy:<rule>@<version>, not bulk:<id>.
func isPolicyRuling(selector string) bool {
	var sel struct {
		Policy json.RawMessage `json:"policy"`
	}
	return json.Unmarshal([]byte(selector), &sel) == nil && len(sel.Policy) > 0
}

// policyRulingOf is the ruling a policy-ruled statement was last ruled under,
// for get_edge to name: what revert_ruling takes.
func (d *Document) policyRulingOf(ctx context.Context, key sqlmem.ScopeKey, edgeID string) (string, error) {
	res, err := d.query(ctx, key, `SELECT i.ruling_id FROM statement_ruling_items i JOIN statement_rulings r ON r.id = i.ruling_id
		WHERE i.edge_id = ? ORDER BY r.created_at DESC LIMIT 1`, edgeID)
	if err != nil || len(res.Rows) == 0 {
		return "", err
	}
	return asStr(res.Rows[0][0]), nil
}

const (
	// reviewPolicyScanMax is the most proposals one policy application
	// evaluates; a larger selection is refused, to be narrowed by its filter.
	reviewPolicyScanMax = 10000
	// reviewPolicyPage is how many it reads at a time.
	reviewPolicyPage = 1000
)

// policyDecision is what the policy decided for one candidate.
type policyDecision struct {
	c      policyCandidate
	rule   memrank.ReviewRule
	action string
}

// ApplyReviewPolicy runs the policy in force over the queue: see
// connector.Connector.ApplyReviewPolicy. scope is tenant or user; as is the
// authority the connector decided, and only an operator applies the policy.
func (d *Document) ApplyReviewPolicy(ctx context.Context, scope string, req connector.ApplyReviewPolicyRequest, as RulingAuthority) (connector.ApplyReviewPolicyResult, error) {
	out := connector.ApplyReviewPolicyResult{Scope: scope, DryRun: req.DryRun == nil || *req.DryRun, Rules: []connector.PolicyRuleCount{}}
	if as != RuleAsOperator {
		return out, reviewForbidden("applying the review policy is the operator's act; a reviewer rules with rule_statements")
	}
	max := req.Max
	switch {
	case max < 0 || max > reviewRulingMax:
		return out, reviewInvalid(fmt.Sprintf("max must be between 1 and %d", reviewRulingMax))
	case max == 0:
		max = reviewRulingMax
	}
	var filter connector.ReviewQueueFilter
	if req.Filter != nil {
		filter = *req.Filter
	}
	if filter.Status != "" && filter.Status != edgeStatusProposed {
		return out, reviewInvalid("the policy rules proposals: filter.status must be proposed, or left out")
	}
	filter.Status = edgeStatusProposed
	if !out.DryRun && req.ExpectCount == nil {
		return out, reviewInvalid("expect_count is required to rule: pass the matched count of the dry run")
	}
	key, mscope, err := d.reviewOp(ctx, scope)
	if err != nil {
		return out, err
	}
	pf, ok := d.reviewPolicyInForce(ctx)
	if !ok {
		return out, reviewInvalid("no review policy is in force: the tenant ontology is not confirmed, or could not be read")
	}
	out.Source, out.Version = pf.policy.Source, pf.version
	if !pf.policy.Applied() {
		return out, reviewInvalid("the Review section has issues, so the policy rules nothing until they are fixed (describe_ontology lists them): " +
			strings.Join(pf.policy.Issues, "; "))
	}
	matched, cands, err := d.selectPolicyCandidates(ctx, key, pf, filter)
	if err != nil {
		return out, err
	}
	out.Matched = matched
	acl := d.newDocACL(ctx, key)
	counts := map[int]*connector.PolicyRuleCount{}
	var toRule []policyDecision
	var routes []policyDecision
	for _, c := range cands {
		ok, err := d.mayRule(ctx, acl, as, c.ends["from_document_id"], c.ends["to_document_id"])
		if err != nil {
			return out, err
		}
		if !ok {
			out.Skipped++
			continue
		}
		rule, action := pf.decide(c)
		n := counts[rule.Index]
		if n == nil {
			n = &connector.PolicyRuleCount{Rule: rule.Index, Action: action, Text: rule.Text}
			counts[rule.Index] = n
		}
		n.Count++
		switch action {
		case memrank.ReviewAccept, memrank.ReviewReject:
			if action == memrank.ReviewAccept {
				out.Accept++
			} else {
				out.Reject++
			}
			if len(toRule) < max {
				toRule = append(toRule, policyDecision{c, rule, action})
			} else {
				out.Deferred++
			}
		case memrank.ReviewJudge:
			out.Judge++
			routes = append(routes, policyDecision{c, rule, action})
		default:
			out.Human++
			routes = append(routes, policyDecision{c, rule, action})
		}
	}
	out.ToRule = len(toRule)
	for _, n := range counts {
		out.Rules = append(out.Rules, *n)
	}
	sort.Slice(out.Rules, func(i, j int) bool {
		// Rule order, and no rule (0) last.
		a, b := out.Rules[i].Rule, out.Rules[j].Rule
		return (a != 0 && (b == 0 || a < b))
	})
	if out.DryRun {
		return out, d.policySample(ctx, key, toRule, &out)
	}
	if *req.ExpectCount != matched {
		return out, connector.WithMessage(connector.ErrStatementReviewQueueChanged,
			fmt.Sprintf("the selection now matches %d proposals, not the %d expected; nothing was ruled — run the dry run again", matched, *req.ExpectCount))
	}
	if len(toRule) > 0 {
		if err := d.applyPolicyRulings(ctx, key, mscope, pf, req, toRule, &out); err != nil {
			return out, err
		}
	}
	for _, r := range routes {
		route := ""
		if r.action == memrank.ReviewJudge {
			route = reviewRouteJudge
		}
		changed, err := d.routeProposal(ctx, key, r.c.s, route)
		if err != nil {
			logStatementReviewFault(ctx, "review policy: a proposal's judge routing could not be written", out.RulingID, err)
			continue
		}
		if changed {
			out.Marked++
		}
	}
	return out, nil
}

// selectPolicyCandidates reads the proposals the filter selects, as the queue
// lists them (newest first) and with what the rules ask, and counts them.
func (d *Document) selectPolicyCandidates(ctx context.Context, key sqlmem.ScopeKey, pf policyInForce, filter connector.ReviewQueueFilter) (int, []policyCandidate, error) {
	// Field by field, never through JSON: see reviewFilterInput.
	in := reviewFilterInput(filter)
	where, args, none, refusal, err := d.statementQueueWhere(ctx, key, in)
	if err != nil {
		return 0, nil, err
	}
	if refusal != nil {
		return 0, nil, reviewInvalid(refusal.Text)
	}
	if none {
		return 0, nil, nil
	}
	cond := strings.Join(where, " AND ")
	res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id WHERE `+cond, args...)
	if err != nil {
		return 0, nil, err
	}
	matched := 0
	if len(res.Rows) == 1 {
		matched = asInt(res.Rows[0][0])
	}
	if matched > reviewPolicyScanMax {
		return 0, nil, reviewInvalid(fmt.Sprintf("the selection matches %d proposals, more than the %d one call evaluates; narrow the filter (kind, method, from_under_path, created_before)",
			matched, reviewPolicyScanMax))
	}
	var cands []policyCandidate
	for offset := 0; offset < matched; {
		res, err := d.query(ctx, key, policyCandidateSelect+` WHERE `+cond+` ORDER BY e.created_at DESC, e.id DESC LIMIT ? OFFSET ?`,
			append(append([]any{}, args...), reviewPolicyPage, offset)...)
		if err != nil {
			return 0, nil, err
		}
		for _, r := range res.Rows {
			cands = append(cands, pf.candidate(r))
		}
		offset += len(res.Rows)
		// Truncated: the store's row cap cut the page short; the next one follows.
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < reviewPolicyPage) {
			break
		}
	}
	return matched, cands, nil
}

// policySample fills a dry run's sample: up to reviewSampleSize of what it would
// rule, each as the queue renders it, with the rule and action that decided it.
func (d *Document) policySample(ctx context.Context, key sqlmem.ScopeKey, toRule []policyDecision, out *connector.ApplyReviewPolicyResult) error {
	sample := toRule[:min(len(toRule), reviewSampleSize)]
	ids := make([]string, len(sample))
	for i, p := range sample {
		ids[i] = p.c.s.ID
	}
	ev, counts, err := d.readableEvidenceFor(ctx, key, ids, edgeEvidencePreview)
	if err != nil {
		return err
	}
	for _, p := range sample {
		m := d.statementJSON(ctx, p.c.s, p.c.ends, ev[p.c.s.ID], counts[p.c.s.ID], false)
		m["review_rule"], m["review_action"] = p.rule.Index, p.action
		b, _ := json.Marshal(m)
		out.Sample = append(out.Sample, b)
	}
	return nil
}

// applyPolicyRulings rules toRule as one ruling, each statement through ruleOne
// (applyRuling) with its own rule's ruled_by and reason.
func (d *Document) applyPolicyRulings(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, pf policyInForce,
	req connector.ApplyReviewPolicyRequest, toRule []policyDecision, out *connector.ApplyReviewPolicyResult) error {
	rulingID := newDocID()
	selector, _ := json.Marshal(struct {
		Policy map[string]any               `json:"policy"`
		Filter *connector.ReviewQueueFilter `json:"filter,omitempty"`
		Max    int                          `json:"max,omitempty"`
	}{map[string]any{"version": pf.version, "source": pf.policy.Source}, req.Filter, req.Max})
	// Recorded before anything is ruled, so a statement carrying a policy's
	// ruled_by always has a ruling to be undone with.
	if err := d.exec(ctx, key, `INSERT INTO statement_rulings
		(id, action, ruled_by, actor, reason, selector, matched, ruled, failed, skipped, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		rulingID, "policy", operatorRuler(ctx), nullIfEmpty(tools.RunIdentity(ctx).UserID),
		fmt.Sprintf("the review policy (ontology version %d)", pf.version), string(selector), out.Matched, out.Skipped,
		time.Now().UnixNano()); err != nil {
		return err
	}
	out.RulingID = rulingID
	out.Items = make([]connector.RulingItem, 0, len(toRule))
	for _, p := range toRule {
		accept := p.action == memrank.ReviewAccept
		op := "reject_statement"
		if accept {
			op = "accept_statement"
		}
		item, ok := d.ruleOne(ctx, key, mscope, op, rulingID, pf.ruledBy(p.rule), accept, pf.reason(p.rule), p.c.reviewCandidate)
		out.Items = append(out.Items, item)
		if ok {
			out.Ruled++
		} else {
			out.Failed++
		}
	}
	if err := d.exec(ctx, key, `UPDATE statement_rulings SET ruled = ?, failed = ? WHERE id = ?`, out.Ruled, out.Failed, rulingID); err != nil {
		logStatementReviewFault(ctx, "statement ruling counts not recorded", rulingID, err)
	}
	return nil
}
