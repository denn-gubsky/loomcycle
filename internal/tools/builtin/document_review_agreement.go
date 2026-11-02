package builtin

// document_review_agreement.go — how often a person agrees with the judge
// agent (RFC FS P4). The judge rules by default, and what makes that safe is
// the sample of its acceptances a person audits: accepting one confirms it,
// rejecting it overturns it. This counts the outcome per kind.

import (
	"context"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// judgeSampledExpr reads attributes.judge.sampled on alias e: set when a person
// settled an acceptance the judge had marked for audit.
func (d *Document) judgeSampledExpr() string {
	if d.docSQL().Tier() == "postgres" {
		return `(CASE WHEN e.attributes IS NULL OR e.attributes = '' THEN NULL ELSE (e.attributes::jsonb -> 'judge' ->> 'sampled') END)`
	}
	return `(CASE WHEN json_valid(e.attributes) THEN json_extract(e.attributes, '$.judge.sampled') END)`
}

// JudgeAgreement reports, per kind, the judge's acceptances and what people
// made of the sample marked for audit. It is the operator's: the caller has
// checked who asks.
func (d *Document) JudgeAgreement(ctx context.Context, scope string) (connector.JudgeAgreementResult, error) {
	out := connector.JudgeAgreementResult{Scope: scope, Kinds: []connector.JudgeAgreementCounts{}}
	key, _, err := d.reviewOp(ctx, scope)
	if err != nil {
		return out, err
	}
	// Every acceptance the judge made: those it still holds (ruled_by
	// judge:<run>), and those a person settled since (audited set), which a
	// person's ruling re-attributes to that person.
	res, err := d.query(ctx, key, `SELECT e.kind, `+d.judgeAuditExpr()+`, `+d.judgeSampledExpr()+`
		FROM chunk_edges e
		WHERE (e.status = ? AND e.ruled_by LIKE 'judge:%') OR `+d.judgeAuditExpr()+` IS NOT NULL`, edgeStatusAsserted)
	if err != nil {
		return out, err
	}
	byKind := map[string]*connector.JudgeAgreementCounts{}
	for _, r := range res.Rows {
		kind, audited := asStr(r[0]), asStr(r[1])
		c := byKind[kind]
		if c == nil {
			c = &connector.JudgeAgreementCounts{Kind: kind}
			byKind[kind] = c
		}
		c.Accepted++
		switch audited {
		case judgeAuditPending:
			c.Sampled++
			c.Pending++
		case judgeAuditConfirmed, judgeAuditOverturned:
			if !truthy(r[2]) {
				continue // settled unprompted: not part of the sample
			}
			c.Sampled++
			if audited == judgeAuditConfirmed {
				c.Confirmed++
			} else {
				c.Overturned++
			}
		}
	}
	for _, c := range byKind {
		out.Kinds = append(out.Kinds, *c)
		out.Total.Accepted += c.Accepted
		out.Total.Sampled += c.Sampled
		out.Total.Pending += c.Pending
		out.Total.Confirmed += c.Confirmed
		out.Total.Overturned += c.Overturned
	}
	sort.Slice(out.Kinds, func(i, j int) bool { return out.Kinds[i].Kind < out.Kinds[j].Kind })
	for i := range out.Kinds {
		out.Kinds[i].Agreement = agreementRate(out.Kinds[i])
	}
	out.Total.Agreement = agreementRate(out.Total)
	return out, nil
}

// agreementRate is confirmed / settled, nil when none is settled.
func agreementRate(c connector.JudgeAgreementCounts) *float64 {
	settled := c.Confirmed + c.Overturned
	if settled == 0 {
		return nil
	}
	r := float64(c.Confirmed) / float64(settled)
	return &r
}

// truthy reads a JSON boolean as either tier returns it: sqlite's json_extract
// gives 1, postgres's ->> gives "true".
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x == "true" || x == "1"
	case []byte:
		return string(x) == "true" || string(x) == "1"
	}
	return false
}
