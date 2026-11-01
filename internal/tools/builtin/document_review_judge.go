package builtin

// document_review_judge.go — the judge's verdicts on proposals (RFC FS P3).
//
// The review policy routes some proposals to a judge (attributes.review_route
// judge). The judge is a code agent: it reads what a reviewer would see, asks a
// model, and hands the verdicts to judge_statements, the one op that records
// them. The op, not the agent, decides what a verdict may do:
//
//   - only a run whose agent definition lists judge in statement_methods may
//     call it (a code-js body, so no model ever calls it itself);
//   - every verdict is recorded on its statement (attributes.judge), once: a
//     statement already judged is refused, so a run cannot ask again until a
//     verdict clears the floor, and a reverted ruling is not ruled again;
//   - a verdict RULES only on a proposal the policy routes to the judge now,
//     at or above the floor of the rule that routes it, in rule mode, when the
//     run may rule there as an operator would (write and link). Anything else —
//     below the floor, unsure, recommend, not routed — is only annotated and
//     waits for a person;
//   - it never touches a statement an operator ruled, or one the run may not
//     read (answered as one that does not exist);
//   - a ruling is a machine's verdict (ruled_by judge:<run id>, judged_by
//     judge, no operator mark), recorded under ONE ruling per run, which
//     revert_ruling undoes together;
//   - a random sample of each run's acceptances (the policy's judge_audit, at
//     least one) is marked attributes.judge.audited pending, for a person.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const judgeOp = "judge_statements"

// statementJudgeGrant is the statement_methods value that lets a code agent's
// run record verdicts and rule through judge_statements.
const statementJudgeGrant = "judge"

// HasJudgeGrant reports whether the caller is a run whose agent definition
// lists judge in statement_methods: the only caller judge_statements serves.
func HasJudgeGrant(ctx context.Context) bool {
	return tools.RunID(ctx) != "" && contains(tools.PolicyOf(ctx).StatementMethods, statementJudgeGrant)
}

const (
	// judgeStatementsMax is the most verdicts one call records.
	judgeStatementsMax = 100
	// judgeRationaleMax bounds a verdict's rationale, in characters: one
	// sentence a reviewer reads, never a passage.
	judgeRationaleMax = 500
	// rulingActionJudge is a judge run's ruling in statement_rulings.
	rulingActionJudge = "judge"
)

// Verdicts.
const (
	judgeVerdictAccept = "accept"
	judgeVerdictReject = "reject"
	judgeVerdictUnsure = "unsure"
)

// What came of one verdict: attributes.judge.outcome, and the item's outcome.
const (
	judgeOutcomeRuled        = "ruled"
	judgeOutcomeBelowFloor   = "below_floor"   // escalated: the confidence is under the floor
	judgeOutcomeUnsure       = "unsure"        // escalated: the judge could not decide
	judgeOutcomeNotPermitted = "not_permitted" // escalated: the run may not rule there
	judgeOutcomeRecommend    = "recommend"     // the call's or the routing rule's mode
	judgeOutcomeNotRouted    = "not_routed"    // the policy does not route it to the judge
	judgeOutcomeRefused      = "refused"       // the item only: nothing was recorded
	// judgeOutcomeFailed: the item only. The verdict was recorded to rule, and
	// the ruling failed (the statement changed meanwhile, or a store fault):
	// it waits for a person, its verdict on it.
	judgeOutcomeFailed = "ruling_failed"
)

// Audit states of a judge's acceptance (attributes.judge.audited).
const (
	judgeAuditPending    = "pending"
	judgeAuditConfirmed  = "confirmed"
	judgeAuditOverturned = "overturned"
)

// judgementIn is one verdict as the judge hands it over.
type judgementIn struct {
	ID         string   `json:"id"`
	Verdict    string   `json:"verdict"`
	Confidence *float64 `json:"confidence"`
	Rationale  string   `json:"rationale"`
	// VerdictRunID is the run of the model call that judged it, from the
	// sub-agent's result header. Recorded, with that run's model, only when it
	// is a run this run spawned.
	VerdictRunID string `json:"verdict_run_id,omitempty"`
}

// judgeItem is what came of one verdict.
type judgeItem struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	Audit   string `json:"audit,omitempty"`
}

// judgeResult is judge_statements' answer.
type judgeResult struct {
	RulingID    string      `json:"ruling_id,omitempty"`
	Judged      int         `json:"judged"`
	Ruled       int         `json:"ruled"`
	Accepted    int         `json:"accepted"`
	Rejected    int         `json:"rejected"`
	Escalated   int         `json:"escalated"`
	Recommended int         `json:"recommended"`
	Refused     int         `json:"refused"`
	AuditMarked int         `json:"audit_marked"`
	Items       []judgeItem `json:"items"`
}

// judgeStatements is Document op=judge_statements.
func (d *Document) judgeStatements(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if !HasJudgeGrant(ctx) {
		return errPermission(judgeOp+": permission: only a run of a code agent whose definition lists judge in statement_methods records a judge's verdicts",
			"Leave the proposals for a person; an operator rules from the review queue."), nil
	}
	write := in.Write
	switch write {
	case "":
		write = memrank.JudgeModeRule
	case memrank.JudgeModeRule, memrank.JudgeModeRecommend:
	default:
		return errValidation(judgeOp+": write must be rule or recommend", "Omit write to rule above the floor."), nil
	}
	if len(in.Judgements) == 0 || len(in.Judgements) > judgeStatementsMax {
		return errValidation(fmt.Sprintf("%s: judgements must hold 1 to %d verdicts", judgeOp, judgeStatementsMax),
			"Send the verdicts in batches."), nil
	}
	seen := map[string]bool{}
	for i := range in.Judgements {
		j := &in.Judgements[i]
		j.Rationale = strings.TrimSpace(j.Rationale)
		switch {
		case j.ID == "" || len(j.ID) > reviewFieldMax:
			return errValidation(fmt.Sprintf("%s: judgements[%d].id is empty or too long", judgeOp, i), ""), nil
		case seen[j.ID]:
			return errValidation(fmt.Sprintf("%s: judgements[%d]: %s is judged twice in one call", judgeOp, i, j.ID), ""), nil
		case j.Verdict != judgeVerdictAccept && j.Verdict != judgeVerdictReject && j.Verdict != judgeVerdictUnsure:
			return errValidation(fmt.Sprintf("%s: judgements[%d].verdict must be accept, reject or unsure", judgeOp, i), ""), nil
		case j.Confidence == nil || math.IsNaN(*j.Confidence) || *j.Confidence < 0 || *j.Confidence > 1:
			return errValidation(fmt.Sprintf("%s: judgements[%d].confidence must be a number between 0 and 1", judgeOp, i), ""), nil
		case j.Rationale == "" || utf8.RuneCountInString(j.Rationale) > judgeRationaleMax || strings.ContainsAny(j.Rationale, "\x00"):
			return errValidation(fmt.Sprintf("%s: judgements[%d].rationale must be one sentence of 1 to %d characters", judgeOp, i, judgeRationaleMax), ""), nil
		case len(j.VerdictRunID) > reviewFieldMax:
			return errValidation(fmt.Sprintf("%s: judgements[%d].verdict_run_id is too long", judgeOp, i), ""), nil
		}
		seen[j.ID] = true
	}
	// A policy that cannot be read routes nothing to the judge: every verdict
	// is then only recorded.
	pf, inForce := d.reviewPolicyInForce(ctx)
	run := tools.RunID(ctx)
	acl := d.newDocACL(ctx, key)
	out := judgeResult{Items: make([]judgeItem, 0, len(in.Judgements))}
	var accepted []string
	for _, j := range in.Judgements {
		item, rulingID, err := d.judgeOne(ctx, key, mscope, acl, pf, inForce, write, run, j)
		if err != nil {
			return errFrom(judgeOp+": "+err.Error(), err), nil
		}
		if rulingID != "" {
			out.RulingID = rulingID
		}
		switch item.Outcome {
		case judgeOutcomeRefused:
			out.Refused++
		case judgeOutcomeRuled:
			out.Judged++
			out.Ruled++
			if item.Status == edgeStatusAsserted {
				out.Accepted++
				accepted = append(accepted, j.ID)
			} else {
				out.Rejected++
			}
		case judgeOutcomeBelowFloor, judgeOutcomeUnsure, judgeOutcomeNotPermitted, judgeOutcomeFailed:
			out.Judged++
			out.Escalated++
		default:
			out.Judged++
			out.Recommended++
		}
		out.Items = append(out.Items, item)
	}
	if out.RulingID != "" {
		rate := memrank.DefaultJudgeAudit
		if inForce && pf.policy.JudgeAudit > 0 {
			rate = pf.policy.JudgeAudit
		}
		marked := d.sampleJudgeAudit(ctx, key, out.RulingID, rate, accepted)
		for i := range out.Items {
			if marked[out.Items[i].ID] {
				out.Items[i].Audit = judgeAuditPending
				out.AuditMarked++
			}
		}
		attempted := out.Ruled
		for _, it := range out.Items {
			if it.Outcome == judgeOutcomeFailed {
				attempted++
			}
		}
		if err := d.exec(ctx, key, `UPDATE statement_rulings SET matched = matched + ?, ruled = ruled + ?, failed = failed + ? WHERE id = ?`,
			attempted, out.Ruled, attempted-out.Ruled, out.RulingID); err != nil {
			logStatementReviewFault(ctx, "statement ruling counts not recorded", out.RulingID, err)
		}
	}
	return jsonResult(out)
}

// judgeOne records one verdict, and rules on it when it may. rulingID is the
// run's ruling when this verdict ruled.
func (d *Document) judgeOne(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL, pf policyInForce, inForce bool,
	write, run string, j judgementIn) (judgeItem, string, error) {
	item := judgeItem{ID: j.ID}
	refuse := func(code, msg string) (judgeItem, string, error) {
		item.Outcome, item.Error, item.Message = judgeOutcomeRefused, code, msg
		return item, "", nil
	}
	c, err := d.policyCandidateByID(ctx, key, pf, j.ID)
	if err != nil {
		return item, "", err
	}
	readable := c != nil
	if c != nil {
		for _, doc := range []string{c.ends["from_document_id"], c.ends["to_document_id"]} {
			if readable && doc != "" {
				if readable, err = acl.readableOrAll(ctx, doc); err != nil {
					return item, "", err
				}
			}
		}
	}
	if !readable {
		return refuse(rulingNotFound, "no such statement: "+j.ID)
	}
	s := c.s
	switch {
	case s.OperatorRuledAt != nil:
		return refuse("operator_ruled", "an operator ruled on this statement; a judge never touches it")
	case s.Status != edgeStatusProposed:
		return refuse(rulingNotProposal, "the statement is "+s.Status+", so there is nothing to judge")
	}
	if _, judged := s.Attributes[attrJudge]; judged {
		return refuse("already_judged", "the statement already carries a judge's verdict; it waits for a person")
	}
	// What the verdict may do: rule only on what the policy routes to the
	// judge NOW, by the rule that routes it.
	outcome, floor := judgeOutcomeNotRouted, -1.0
	if route, _ := s.Attributes[attrReviewRoute].(string); route == reviewRouteJudge && inForce && pf.section {
		if rule, action := pf.decide(*c); action == memrank.ReviewJudge {
			floor = pf.policy.EffectiveFloor(rule)
			switch {
			case write == memrank.JudgeModeRecommend || rule.Mode == memrank.JudgeModeRecommend:
				outcome = judgeOutcomeRecommend
			case j.Verdict == judgeVerdictUnsure:
				outcome = judgeOutcomeUnsure
			case *j.Confidence < floor:
				outcome = judgeOutcomeBelowFloor
			default:
				outcome = judgeOutcomeRuled
			}
		}
	}
	if outcome == judgeOutcomeRuled {
		ok, err := d.mayRule(ctx, acl, RuleAsOperator, c.ends["from_document_id"], c.ends["to_document_id"])
		if err != nil {
			return item, "", err
		}
		if !ok {
			outcome = judgeOutcomeNotPermitted
		}
	}
	rec := map[string]any{
		"verdict": j.Verdict, "confidence": *j.Confidence, "rationale": j.Rationale,
		"run_id": run, "judged_at": time.Now().UnixNano(), "outcome": outcome,
	}
	if floor >= 0 {
		rec["floor"] = floor
	}
	if name := tools.AgentName(ctx); name != "" {
		rec["agent"] = name
	}
	if model, ok := d.verdictModel(ctx, run, j.VerdictRunID); ok {
		rec["verdict_run_id"], rec["model"] = j.VerdictRunID, model
	}
	recorded, err := d.recordJudgement(ctx, key, s.ID, rec)
	if err != nil {
		return item, "", err
	}
	if !recorded {
		return refuse(rulingChanged, "the statement changed while the verdict was recorded")
	}
	item.Outcome, item.Status = outcome, edgeStatusProposed
	if outcome != judgeOutcomeRuled {
		return item, "", nil
	}
	// Re-read: the candidate predates the verdict just recorded, and a policy
	// application may have re-routed it meanwhile.
	fresh, err := d.statementByID(ctx, key, s.ID)
	if err != nil {
		return item, "", err
	}
	if fresh == nil {
		return refuse(rulingNotFound, "no such statement: "+j.ID)
	}
	if route, _ := fresh.Attributes[attrReviewRoute].(string); route != reviewRouteJudge || fresh.Status != edgeStatusProposed {
		item.Outcome, item.Error, item.Message = judgeOutcomeFailed, rulingChanged, "the statement was re-routed or ruled while it was judged; it waits for a person"
		return item, "", nil
	}
	rulingID, err := d.judgeRuling(ctx, key, run)
	if err != nil {
		logStatementReviewFault(ctx, "judge: the ruling could not be recorded; the verdict waits for a person", "", err)
		item.Outcome, item.Message = judgeOutcomeFailed, "the ruling could not be recorded; the verdict waits for a person"
		return item, "", nil
	}
	accept := j.Verdict == judgeVerdictAccept
	op := "reject_statement"
	if accept {
		op = "accept_statement"
	}
	// The statement was proposed again since an earlier ruling of this run
	// was reverted: that record no longer describes it.
	if err := d.exec(ctx, key, `DELETE FROM statement_ruling_items WHERE ruling_id = ? AND edge_id = ?`, rulingID, s.ID); err != nil {
		logStatementReviewFault(ctx, "judge: a stale ruling record could not be removed", rulingID, err)
	}
	ruled, ok := d.ruleOne(ctx, key, mscope, op, rulingID, "judge:"+run, accept, judgeReason(j), reviewCandidate{s: *fresh, ends: c.ends})
	if !ok {
		item.Outcome, item.Error, item.Message = judgeOutcomeFailed, ruled.Error, ruled.Message
		return item, rulingID, nil
	}
	item.Status = ruled.Status
	return item, rulingID, nil
}

// judgeReason is a judge ruling's reason, as judge_reason records it.
func judgeReason(j judgementIn) string {
	return fmt.Sprintf("judge (%s, confidence %.2f): %s", j.Verdict, *j.Confidence, j.Rationale)
}

// verdictModel is the model that answered a verdict: the provider and model of
// the run verdictRunID names, when that run is one this run spawned.
func (d *Document) verdictModel(ctx context.Context, run, verdictRunID string) (string, bool) {
	if verdictRunID == "" || d.Store == nil {
		return "", false
	}
	r, err := d.Store.GetRun(ctx, verdictRunID)
	if err != nil || r.ParentRunID != run {
		return "", false
	}
	model := r.Model
	if r.Provider != "" {
		model = r.Provider + "/" + r.Model
	}
	return model, true
}

// recordJudgement writes rec as a proposal's attributes.judge, conditional on
// the attributes as read (so of two judges the second is refused) and on the
// statement still being proposed. false: it changed meanwhile.
func (d *Document) recordJudgement(ctx context.Context, key sqlmem.ScopeKey, id string, rec map[string]any) (bool, error) {
	res, err := d.query(ctx, key, `SELECT attributes FROM chunk_edges WHERE id = ? AND status = ?`, id, edgeStatusProposed)
	if err != nil || len(res.Rows) == 0 {
		return false, err
	}
	raw := res.Rows[0][0]
	attrs := map[string]any{}
	if text := asStr(raw); text != "" {
		if err := json.Unmarshal([]byte(text), &attrs); err != nil {
			return false, fmt.Errorf("statement %s: attributes: %w", id, err)
		}
	}
	if _, judged := attrs[attrJudge]; judged {
		return false, nil
	}
	attrs[attrJudge] = rec
	arg, err := attributesArg(attrs)
	if err != nil {
		return false, err
	}
	cond, args := `attributes IS NULL`, []any{arg, id, edgeStatusProposed}
	if raw != nil {
		cond, args = `attributes = ?`, append(args, asStr(raw))
	}
	n, err := d.execCount(ctx, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ? AND status = ? AND `+cond, args...)
	return n > 0, err
}

// judgeRulingID is the one ruling a judge run's rulings are recorded under.
func judgeRulingID(run string) string {
	h := sha256.Sum256([]byte("judge\x00" + run))
	return hex.EncodeToString(h[:16])
}

// judgeRuling is the run's ruling, created by its first ruling.
func (d *Document) judgeRuling(ctx context.Context, key sqlmem.ScopeKey, run string) (string, error) {
	id := judgeRulingID(run)
	res, err := d.query(ctx, key, `SELECT 1 FROM statement_rulings WHERE id = ?`, id)
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 1 {
		return id, nil
	}
	judge := map[string]any{"run_id": run}
	if name := tools.AgentName(ctx); name != "" {
		judge["agent"] = name
	}
	selector, _ := json.Marshal(map[string]any{"judge": judge})
	err = d.exec(ctx, key, `INSERT INTO statement_rulings
		(id, action, ruled_by, actor, reason, selector, matched, ruled, failed, skipped, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0, ?)`,
		id, rulingActionJudge, "judge:"+run, nullIfEmpty(rulingActor(ctx)),
		"the judge's verdicts at or above the review policy's floor", string(selector), time.Now().UnixNano())
	if err != nil {
		// Another call of this run made it first.
		res, qerr := d.query(ctx, key, `SELECT 1 FROM statement_rulings WHERE id = ?`, id)
		if qerr == nil && len(res.Rows) == 1 {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

// judgeRulingRun is the run a judge ruling's selector names, or "".
func judgeRulingRun(selector string) string {
	var sel struct {
		Judge *struct {
			RunID string `json:"run_id"`
		} `json:"judge"`
	}
	if json.Unmarshal([]byte(selector), &sel) != nil || sel.Judge == nil {
		return ""
	}
	return sel.Judge.RunID
}

// reviewRouteExpr reads attributes.review_route on alias e, NULL when unset.
func (d *Document) reviewRouteExpr() string {
	if d.docSQL().Tier() == "postgres" {
		return `NULLIF((CASE WHEN e.attributes IS NULL OR e.attributes = '' THEN NULL ELSE (e.attributes::jsonb ->> 'review_route') END), '')`
	}
	return `NULLIF((CASE WHEN json_valid(e.attributes) THEN json_extract(e.attributes, '$.review_route') END), '')`
}

// judgeAuditExpr reads attributes.judge.audited on alias e, NULL when unset.
func (d *Document) judgeAuditExpr() string {
	if d.docSQL().Tier() == "postgres" {
		return `NULLIF((CASE WHEN e.attributes IS NULL OR e.attributes = '' THEN NULL ELSE (e.attributes::jsonb -> 'judge' ->> 'audited') END), '')`
	}
	return `NULLIF((CASE WHEN json_valid(e.attributes) THEN json_extract(e.attributes, '$.judge.audited') END), '')`
}

// sampleJudgeAudit marks a random share (rate) of this call's acceptances for
// a person's audit, and one of them when the run's ruling holds no audited
// acceptance yet — so every run that accepted anything is sampled. Each mark
// refreshes the ruling's fingerprint of the statement, which the mark changed,
// so the run's revert still applies. A mark that fails is logged: the
// acceptance stands, unsampled.
func (d *Document) sampleJudgeAudit(ctx context.Context, key sqlmem.ScopeKey, rulingID string, rate float64, accepted []string) map[string]bool {
	marked := map[string]bool{}
	if len(accepted) == 0 {
		return marked
	}
	var pick []string
	for _, id := range accepted {
		if rand.Float64() < rate {
			pick = append(pick, id)
		}
	}
	if len(pick) == 0 {
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM statement_ruling_items i JOIN chunk_edges e ON e.id = i.edge_id
			WHERE i.ruling_id = ? AND `+d.judgeAuditExpr()+` IS NOT NULL`, rulingID)
		if err != nil {
			logStatementReviewFault(ctx, "judge: the audit sample could not be counted", rulingID, err)
			return marked
		}
		if len(res.Rows) == 1 && asInt(res.Rows[0][0]) == 0 {
			pick = append(pick, accepted[rand.IntN(len(accepted))])
		}
	}
	for _, id := range pick {
		if err := d.markJudgeAudit(ctx, key, rulingID, id); err != nil {
			logStatementReviewFault(ctx, "judge: an acceptance could not be marked for audit", rulingID, err)
			continue
		}
		marked[id] = true
	}
	return marked
}

// markJudgeAudit sets attributes.judge.audited pending on an acceptance the
// run's ruling holds, and the ruling's fingerprint of it after.
func (d *Document) markJudgeAudit(ctx context.Context, key sqlmem.ScopeKey, rulingID, id string) error {
	res, err := d.query(ctx, key, `SELECT attributes FROM chunk_edges WHERE id = ? AND status = ? AND ruled_by = ?`,
		id, edgeStatusAsserted, "judge:"+tools.RunID(ctx))
	if err != nil {
		return err
	}
	if len(res.Rows) == 0 {
		return fmt.Errorf("statement %s is no longer the run's acceptance", id)
	}
	raw := asStr(res.Rows[0][0])
	attrs := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		return err
	}
	rec, _ := attrs[attrJudge].(map[string]any)
	if rec == nil {
		return fmt.Errorf("statement %s carries no verdict", id)
	}
	rec["audited"] = judgeAuditPending
	arg, err := attributesArg(attrs)
	if err != nil {
		return err
	}
	n, err := d.execCount(ctx, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ? AND attributes = ?`, arg, id, raw)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("statement %s changed while it was marked", id)
	}
	fp, err := d.statementFingerprint(ctx, key, id)
	if err != nil {
		return err
	}
	return d.exec(ctx, key, `UPDATE statement_ruling_items SET fingerprint = ? WHERE ruling_id = ? AND edge_id = ?`, fp, rulingID, id)
}
