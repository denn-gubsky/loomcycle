package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// Selecting a summary group exactly: what the queue's summary counts, a
// filter must select, statement for statement.

// setJudgeAttributes writes a statement's attributes as the runtime's judge
// write would (a writer may not send attributes.judge).
func setJudgeAttributes(t *testing.T, d *Document, ctx context.Context, id, attrs string) {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "tenant")
	if err == nil {
		err = d.exec(ctx, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ?`, attrs, id)
	}
	if err != nil {
		t.Fatalf("set attributes of %s: %v", id, err)
	}
}

// A stored judge verdict of "" is no verdict on both tiers: the summary files
// it under "" (no verdict), so has_judge_verdict=false must select it and
// has_judge_verdict=true must not.
func TestListStatements_EmptyJudgeVerdictIsNoVerdict(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		setJudgeAttributes(t, d, op, c.repeals, `{"judge":{"verdict":""}}`)
		none := listIDs(t, d, op, `{"op":"list_statements","scope":"tenant","has_judge_verdict":false}`)
		if !none[c.repeals] || len(none) != 3 {
			t.Errorf("has_judge_verdict=false = %v, want the empty verdict (%s) among the 3 without one", none, c.repeals)
		}
		some := listIDs(t, d, op, `{"op":"list_statements","scope":"tenant","has_judge_verdict":true}`)
		if some[c.repeals] || len(some) != 1 {
			t.Errorf("has_judge_verdict=true = %v, want only the recorded verdict", some)
		}
		out, res := docExec(t, d, op, `{"op":"list_statements","scope":"tenant","summarize":true}`)
		if res.IsError {
			t.Fatal(res.Text)
		}
		noVerdict := 0
		for _, g := range out["summary"].([]any) {
			if m := g.(map[string]any); m["judge_verdict"] == "" {
				noVerdict += int(m["count"].(float64))
			}
		}
		if noVerdict != len(none) {
			t.Errorf("the summary counts %d without a verdict, the filter selects %d", noVerdict, len(none))
		}
	})
}

// selectCorpus is a tenant whose summary groups a filter could mistake: a
// document filed under two Path names, one filed nowhere, methods
// rule:citation and rule:citation_title (one a prefix of the other) and a
// verdict recorded as "". In the statement names, "cite" is rule:citation and
// "citation" is rule:citation_title.
type selectCorpus struct {
	twoPaths, board, unfiled, notes string // documents
	ids                             map[string]string
}

func seedSelectCorpus(t *testing.T, d *Document, base context.Context) selectCorpus {
	t.Helper()
	op := reviewOperatorCtx(base, nil)
	key, _, err := d.resolveScope(op, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	c := selectCorpus{ids: map[string]string{}}
	chunks := map[string]string{}
	mk := func(name, path string) string {
		out, res := docExec(t, d, op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q}`, name, path))
		if res.IsError {
			t.Fatalf("create_document %s: %s", path, res.Text)
		}
		doc := out["document_id"].(string)
		out, res = docExec(t, d, op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":%q,"body":"text"}`, doc, name))
		if res.IsError {
			t.Fatalf("create_chunk %s: %s", name, res.Text)
		}
		chunks[name] = out["id"].(string)
		return doc
	}
	// Filed under /board/reg/a and /aa/x/a: the summary files it under the
	// first name in sort order (/aa/x), from_under_path /board takes it too.
	c.twoPaths = mk("A", "/board/reg/a")
	if _, err := d.registerDocDirent(op, key, c.twoPaths, "/aa/x/a"); err != nil {
		t.Fatal(err)
	}
	c.board = mk("B", "/board/reg/b")
	c.notes = mk("K", "/kb/notes/k")
	// Filed nowhere: its one name removed.
	c.unfiled = mk("U", "/tmp/u")
	if ok, err := d.Store.DirentDelete(op, direntTenant(op), "tenant", direntScopeID(key), "/tmp/", "u"); err != nil || !ok {
		t.Fatalf("unfile U: %v %v", ok, err)
	}
	w := reviewWriterCtx(base)
	for _, s := range []struct{ name, from, to, kind, method string }{
		{"A>B cites cite", "A", "B", "cites", "rule:citation"},
		{"A>K cites citation", "A", "K", "cites", "rule:citation_title"},
		{"B>A cites cite", "B", "A", "cites", "rule:citation"},
		{"U>B cites cite", "U", "B", "cites", "rule:citation"},
		{"B>K repeals citation", "B", "K", "repeals", "rule:citation_title"},
		{"K>B cites cite", "K", "B", "cites", "rule:citation"},
		{"K>A elaborates", "K", "A", "elaborates", "extractor:relate"},
		{"A>U repeals cite", "A", "U", "repeals", "rule:citation"},
	} {
		out, res := docExec(t, d, w, fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":%q,"method":%q}`,
			chunks[s.from], chunks[s.to], s.kind, s.method))
		if res.IsError || out["status"] != edgeStatusProposed {
			t.Fatalf("propose %s: %v %s", s.name, out, res.Text)
		}
		c.ids[s.name] = out["id"].(string)
	}
	setJudgeAttributes(t, d, op, c.ids["B>K repeals citation"], `{"judge":{"verdict":""}}`)
	setJudgeAttributes(t, d, op, c.ids["K>B cites cite"], `{"judge":{"verdict":"accept"}}`)
	return c
}

// selected is every statement a filter selects in the queue, by id.
func selected(t *testing.T, d *Document, ctx context.Context, f connector.ReviewQueueFilter) map[string]bool {
	t.Helper()
	res, err := d.ListReviewQueue(ctx, "tenant", connector.ListReviewQueueRequest{Filter: f, Limit: 5000})
	if err != nil {
		t.Fatalf("queue %+v: %v", f, err)
	}
	ids := map[string]bool{}
	for _, raw := range res.Statements {
		var s struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &s)
		ids[s.ID] = true
	}
	return ids
}

// Each summary group's own filter selects exactly the group's count, in the
// queue and in a bulk ruling's dry run, with and without a base filter; the
// groups' selections are disjoint and cover the queue.
func TestListReviewQueue_EachSummaryGroupsFilterSelectsExactlyItsCount(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedSelectCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		for _, baseFilter := range []connector.ReviewQueueFilter{{}, {Method: "rule:"}, {FromUnderPath: "/board"}} {
			sum, err := d.ListReviewQueue(op, "tenant", connector.ListReviewQueueRequest{Filter: baseFilter, Summarize: true})
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]string{}
			for _, g := range sum.Summary {
				label := fmt.Sprintf("%+v: group %s|%s|%s|%s", baseFilter, g.Kind, g.Method, g.FromPathPrefix, g.JudgeVerdict)
				if g.Filter == nil {
					t.Errorf("%s carries no filter", label)
					continue
				}
				got := selected(t, d, op, *g.Filter)
				if len(got) != g.Count {
					t.Errorf("%s: its filter %+v selects %d, the group counts %d", label, *g.Filter, len(got), g.Count)
				}
				for id := range got {
					if prev, dup := seen[id]; dup {
						t.Errorf("%s and %s both select %s", prev, label, id)
					}
					seen[id] = label
				}
				dry := ruleDry(t, d, op, connector.RuleStatementsRequest{Action: "accept", Filter: g.Filter}, RuleAsOperator)
				if dry.Matched != g.Count {
					t.Errorf("%s: a ruling by its filter matches %d, the group counts %d", label, dry.Matched, g.Count)
				}
			}
			if len(seen) != sum.Total {
				t.Errorf("%+v: the groups select %d statements, the summary totals %d", baseFilter, len(seen), sum.Total)
			}
		}
		_ = c
	})
}

// Each exact selector against the near miss it exists to tell apart; method,
// the prefix, keeps selecting by prefix.
func TestListReviewQueue_ExactSelectorsTellApartWhatPrefixesConflate(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedSelectCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		ids := func(names ...string) map[string]bool {
			out := map[string]bool{}
			for _, n := range names {
				out[c.ids[n]] = true
			}
			return out
		}
		for _, tc := range []struct {
			name string
			f    connector.ReviewQueueFilter
			want map[string]bool
		}{
			{"method prefix rule:citation takes rule:citation_title too", connector.ReviewQueueFilter{Method: "rule:citation"},
				ids("A>B cites cite", "A>K cites citation", "B>A cites cite", "U>B cites cite", "B>K repeals citation", "K>B cites cite", "A>U repeals cite")},
			{"method_exact rule:citation", connector.ReviewQueueFilter{MethodExact: "rule:citation"},
				ids("A>B cites cite", "B>A cites cite", "U>B cites cite", "K>B cites cite", "A>U repeals cite")},
			{"kind_exact repeals", connector.ReviewQueueFilter{KindExact: "repeals"}, ids("B>K repeals citation", "A>U repeals cite")},
			{"from_under_path /board takes the twice-filed document", connector.ReviewQueueFilter{FromUnderPath: "/board"},
				ids("A>B cites cite", "A>K cites citation", "B>A cites cite", "B>K repeals citation", "A>U repeals cite")},
			{"from_path_prefix /board/reg is its summary group", connector.ReviewQueueFilter{FromPathPrefix: "/board/reg"},
				ids("B>A cites cite", "B>K repeals citation")},
			{"from_path_prefix /aa/x", connector.ReviewQueueFilter{FromPathPrefix: "/aa/x"},
				ids("A>B cites cite", "A>K cites citation", "A>U repeals cite")},
			{"from_path_prefix /board is no group", connector.ReviewQueueFilter{FromPathPrefix: "/board"}, ids()},
			{"from_unfiled", connector.ReviewQueueFilter{FromUnfiled: true}, ids("U>B cites cite")},
			{"has_judge_verdict false takes the empty verdict", connector.ReviewQueueFilter{HasJudgeVerdict: boolPtr(false), KindExact: "repeals"},
				ids("B>K repeals citation", "A>U repeals cite")},
		} {
			got := selected(t, d, op, tc.f)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%s: selects %v, want %v", tc.name, got, tc.want)
			}
		}
		// Asking for a prefix and for unfiled at once is a contradiction.
		if _, err := d.ListReviewQueue(op, "tenant", connector.ListReviewQueueRequest{
			Filter: connector.ReviewQueueFilter{FromPathPrefix: "/aa/x", FromUnfiled: true}}); connector.StatementReviewCode(err) != "invalid_request" {
			t.Errorf("from_path_prefix with from_unfiled: %v, want invalid_request", err)
		}
	})
}

// Where the ontology declares a sub-kind, kind widens to it and kind_exact
// does not: the summary groups by the stored kind, so its filter is exact.
func TestListReviewQueue_KindExactIsNotWidenedToSubKinds(t *testing.T) {
	f := newRelFixture(t, "# Tenant Ontology\n\n## Relations\n@section relations\n\n### governs\n\n#### regulates\nGoverns by regulation.\n")
	f.propose(t, "governs", f.provA, f.provB)
	f.propose(t, "regulates", f.provB, f.provA)
	count := func(filter connector.ReviewQueueFilter) int {
		t.Helper()
		res, err := f.d.ListReviewQueue(f.ctx, "user", connector.ListReviewQueueRequest{Filter: filter})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Statements)
	}
	if n := count(connector.ReviewQueueFilter{Kind: "governs"}); n != 2 {
		t.Errorf("kind governs selects %d, want 2 (with its sub-kind regulates)", n)
	}
	if n := count(connector.ReviewQueueFilter{KindExact: "governs"}); n != 1 {
		t.Errorf("kind_exact governs selects %d, want 1", n)
	}
	sum, err := f.d.ListReviewQueue(f.ctx, "user", connector.ListReviewQueueRequest{Summarize: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range sum.Summary {
		if n := count(*g.Filter); n != g.Count {
			t.Errorf("group %s: its filter selects %d, the group counts %d", g.Kind, n, g.Count)
		}
	}
	if len(sum.Summary) != 2 {
		t.Errorf("summary = %+v, want a group per kind", sum.Summary)
	}
}

// The exact path selectors list only documents the caller may read: documents
// it may not read — however many — change neither the rows it selects nor
// whether it is answered, so the answer tells it nothing of them.
func TestListReviewQueue_PathSelectorsIgnoreDocumentsTheCallerMayNotRead(t *testing.T) {
	d, base, _ := documentFixture(t)
	seedSelectCorpus(t, d, base)
	reader := reviewOperatorCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/secret/**"}}))
	filters := []connector.ReviewQueueFilter{{FromUnfiled: true}, {FromPathPrefix: "/board/reg"}, {FromPathPrefix: "/secret/h"}}
	answer := func() []string {
		var out []string
		for _, f := range filters {
			res, err := d.ListReviewQueue(reader, "tenant", connector.ListReviewQueueRequest{Filter: f, Limit: 5000})
			ids := []string{}
			for _, raw := range res.Statements {
				var s struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(raw, &s)
				ids = append(ids, s.ID)
			}
			sort.Strings(ids)
			out = append(out, fmt.Sprintf("%+v: %v %v", f, ids, err))
		}
		return out
	}
	before := answer()
	// More hidden filed documents than one call binds.
	key, _, err := d.resolveScope(reader, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= listStatementsMaxDocs; i++ {
		if _, err := d.registerDocDirent(reader, key, fmt.Sprintf("%032x", i), fmt.Sprintf("/secret/h/d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if after := answer(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("hidden documents changed the answer:\n before %v\n after  %v", before, after)
	}
}

// TestListReviewQueue_LimitIsBounded: the queue's limit is checked where the
// request enters, not left to list_statements to clamp, so a request's number
// never sizes anything on its way to the op.
func TestListReviewQueue_LimitIsBounded(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		seedSelectCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		for _, bad := range []int{-1, edgeGetEdgesMax + 1} {
			if _, err := d.ListReviewQueue(op, "tenant", connector.ListReviewQueueRequest{Limit: bad}); connector.StatementReviewCode(err) != "invalid_request" {
				t.Errorf("limit %d: %v, want invalid_request", bad, err)
			}
		}
		if _, err := d.ListReviewQueue(op, "tenant", connector.ListReviewQueueRequest{Limit: edgeGetEdgesMax}); err != nil {
			t.Errorf("limit at the bound: %v", err)
		}
	})
}
