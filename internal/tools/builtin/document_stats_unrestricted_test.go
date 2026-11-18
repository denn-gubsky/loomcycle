package builtin

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GM C8: a node's statistics, its annotation's request and its built-in series
// are computed only from the documents every member of the tenant may read, and are
// served the same to every reader of the ontology (D8 (i) retired). C9: a member
// whose document annotation is metadata-only is counted and never averaged.

// restrictForSome stores a list that denies reading paths in the tenant scope, bound
// to the group outsiders, and wires the real access-control resolver over d's store
// as the Audience. A plain member may still read those documents; a member of
// outsiders may not — so not every member may, and they are restricted.
func restrictForSome(t *testing.T, d *Document, paths ...string) {
	t.Helper()
	ctx := context.Background()
	var res []string
	for _, p := range paths {
		res = append(res, "document:tenant:"+p)
	}
	raw, err := json.Marshal([]authz.Rule{{Effect: authz.Deny, Actions: []string{"read"}, Resources: res}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.AclDefPut(ctx, store.AclDefRow{TenantID: "tnt", Name: "outsiders-deny", Rules: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.AclBindingPut(ctx, store.AclBindingRow{TenantID: "tnt", ACLName: "outsiders-deny",
		SubjectKind: string(authz.SubjectGroup), SubjectID: "outsiders"}); err != nil {
		t.Fatal(err)
	}
	d.Audience = aclres.New(d.Store, authz.ModeCompat, 0)
}

// denying is ctx's reader held to a rule set denying it paths: a member of outsiders.
func denying(t *testing.T, ctx context.Context, paths ...string) context.Context {
	t.Helper()
	var res []string
	for _, p := range paths {
		res = append(res, "document:tenant:"+p)
	}
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: res})
	})
}

func hasTerm(v *nodeStatsView, term string) bool { return v != nil && slices.Contains(v.Terms, term) }

func exemplarIDs(v *nodeStatsView) []string {
	var out []string
	if v != nil {
		for _, e := range v.Exemplars {
			out = append(out, e.DocumentID)
		}
	}
	return out
}

// TestNodeStats_RestrictedMemberIsLeftOutOfStatistics — a document a member of the
// tenant may not read is counted in no node, is no exemplar and gives no term; the
// same corpus without the rule counts it.
func TestNodeStats_RestrictedMemberIsLeftOutOfStatistics(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		nodeStatsRun(t, d, k.f.operator, computeAll)
		before, _ := conceptStats(describeStats(t, d, k.f.operator), k.rm)
		if before == nil || *before.Members != 4 || !hasTerm(before, "oversight") {
			t.Fatalf("precondition: without a rule Risk Management counts a2 (4 members, its term oversight): %+v", before)
		}

		restrictForSome(t, d, "/kb/a2")
		// Until the next pass the row is one behind: shown stale, but a2 — no longer a
		// member — is no exemplar of it.
		// outsourcing: its three members were all its exemplars.
		if s, _ := conceptStats(describeStats(t, d, k.f.reader), k.out); s == nil || !s.Stale || len(s.Exemplars) != 2 || slices.Contains(exemplarIDs(s), k.a2) {
			t.Fatalf("before the pass: %+v; want the stale row without a2 among its exemplars", s)
		}
		// A row from before C8 (its hash outside the family) is served to no one.
		key, _, err := d.ontologyTenantKey(k.f.operator)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.exec(k.f.operator, key, `UPDATE node_stats SET inputs_sha256 = 'pre-c8-hash'`); err != nil {
			t.Fatal(err)
		}
		if s, _ := conceptStats(describeStats(t, d, k.f.operator), k.rm); s != nil {
			t.Fatalf("a pre-rule row was served: %+v", s)
		}
		nodeStatsRun(t, d, k.f.operator, computeAll)
		desc := describeStats(t, d, k.f.operator)
		for id, want := range map[string]int{k.rm: 3, k.out: 2} {
			s, _ := conceptStats(desc, id)
			if s == nil || s.Members == nil || *s.Members != want {
				t.Fatalf("concept %s: stats %+v, want %d members (a2 left out)", id, s, want)
			}
			if slices.Contains(exemplarIDs(s), k.a2) || hasTerm(s, "oversight") {
				t.Errorf("concept %s: exemplars %v terms %v carry a2", id, exemplarIDs(s), s.Terms)
			}
		}
		for _, c := range desc.Classes {
			if c.Name == "Document" && (c.Stats == nil || *c.Stats.Members != 3) {
				t.Errorf("class Document stats = %+v, want 3 members (a1 a3 b1)", c.Stats)
			}
		}
	})
}

// TestNodeStats_EveryReaderSeesTheSameStatistics — a reader who may not read a2 and
// the operator get identical statistics and series; nothing is withheld.
func TestNodeStats_EveryReaderSeesTheSameStatistics(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		restrictForSome(t, d, "/kb/a2")
		nodeStatsRun(t, d, k.f.operator, computeAll)
		op, limited := describeStats(t, d, k.f.operator), describeStats(t, d, denying(t, k.f.reader, "/kb/a2"))
		for _, id := range []string{k.rm, k.out, k.rep, k.law} {
			a, _ := conceptStats(op, id)
			b, _ := conceptStats(limited, id)
			if a == nil || !reflect.DeepEqual(a, b) {
				t.Errorf("concept %s: operator %+v, limited reader %+v; want the same, shown", id, a, b)
			}
		}
		if rm, _ := conceptStats(limited, k.rm); rm == nil || rm.Spread == nil || len(rm.Exemplars) != 3 {
			t.Errorf("the limited reader's Risk Management = %+v; want its figures", rm)
		}
		seriesOf := func(desc ontologyDescription) map[string]*nodeSeriesView {
			out := map[string]*nodeSeriesView{}
			for _, s := range desc.Schemes {
				for _, c := range s.Concepts {
					out[c.ChunkID] = c.Series
				}
			}
			return out
		}
		if a, b := seriesOf(op), seriesOf(limited); !reflect.DeepEqual(a, b) || a[k.rm] == nil {
			t.Errorf("series differ by reader or are missing:\n operator %v\n limited  %v", a, b)
		}
	})
}

// TestNodeStats_MetadataOnlyMemberIsCountedButNotAveraged — RFC GP's tier column,
// added here as its P1 will add it: a member whose document annotation is
// metadata-only counts in members and stays out of the centroid (members with a
// vector), the exemplars and the terms. Without the column nothing is marked.
func TestNodeStats_MetadataOnlyMemberIsCountedButNotAveraged(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	nodeStatsRun(t, d, k.f.operator, computeAll)
	if s, _ := conceptStats(describeStats(t, d, k.f.operator), k.rm); s == nil || !hasTerm(s, "contracts") || *s.MembersWithVector != 4 {
		t.Fatalf("precondition: a1 is averaged (term contracts, 4 vectors): %+v", s)
	}
	key, _, err := d.ontologyTenantKey(k.f.operator)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.exec(k.f.operator, key, `ALTER TABLE chunk_annotations ADD COLUMN tier TEXT`); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(k.f.operator, key, `UPDATE chunk_annotations SET tier = 'metadata' WHERE chunk_id = ? AND level = ?`,
		k.docs[k.a1], annotationLevelDocument); err != nil {
		t.Fatal(err)
	}
	if rep := nodeStatsRun(t, d, k.f.operator, `{"op":"node_stats"}`); rep.node(t, nodeKindConcept, k.rm).Status != "stale" {
		t.Errorf("marking a member metadata-only did not make Risk Management stale")
	}
	nodeStatsRun(t, d, k.f.operator, computeAll)
	s, _ := conceptStats(describeStats(t, d, k.f.operator), k.rm)
	if s == nil || *s.Members != 4 || *s.MembersWithVector != 3 {
		t.Fatalf("Risk Management = %+v; want 4 members, 3 averaged", s)
	}
	if slices.Contains(exemplarIDs(s), k.a1) || hasTerm(s, "contracts") {
		t.Errorf("a metadata-only member is an exemplar or gives a term: exemplars %v terms %v", exemplarIDs(s), s.Terms)
	}
}

// TestNodeStats_OpenModeTreatsEveryDocumentAsUnrestricted — with no rule in the
// tenant every document counts, under compat and under enforce (the member
// baseline allows reading the tenant scope), and with no resolver at all.
func TestNodeStats_OpenModeTreatsEveryDocumentAsUnrestricted(t *testing.T) {
	for _, mode := range []authz.Mode{"", authz.ModeCompat, authz.ModeEnforce} {
		t.Run(cmp.Or(string(mode), "no-resolver"), func(t *testing.T) {
			d, base, _ := documentFixture(t)
			k := newStatsCorpus(t, d, base)
			if mode != "" {
				d.Audience = aclres.New(d.Store, mode, 0)
			}
			nodeStatsRun(t, d, k.f.operator, computeAll)
			if s, _ := conceptStats(describeStats(t, d, k.f.reader), k.rm); s == nil || *s.Members != 4 {
				t.Errorf("Risk Management = %+v, want all 4 members", s)
			}
		})
	}
}

// TestDescribeOntology_WithCountsStaysPerCaller — with_counts counts what the
// caller may read (a2 included for whoever may read it, b1 left out for whoever may
// not), while with_stats' members are the unrestricted documents for everyone.
func TestDescribeOntology_WithCountsStaysPerCaller(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	restrictForSome(t, d, "/kb/a2")
	nodeStatsRun(t, d, k.f.operator, computeAll)
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		counts int
	}{{"operator", k.f.operator, 4}, {"denied b1", denying(t, k.f.reader, "/kb/b1"), 3}, {"denied a2", denying(t, k.f.reader, "/kb/a2"), 3}} {
		s, members := conceptStats(describeStats(t, d, tc.ctx), k.rm)
		if members == nil || *members != tc.counts {
			t.Errorf("%s: with_counts Risk Management = %v, want %d", tc.name, members, tc.counts)
		}
		if s == nil || *s.Members != 3 {
			t.Errorf("%s: with_stats Risk Management = %+v, want the unrestricted 3", tc.name, s)
		}
	}
}

// TestMeasure_SeriesCountOnlyUnrestrictedMembers — count, share's basis and unplaced
// leave a restricted document out, and every reader reads the same rows.
func TestMeasure_SeriesCountOnlyUnrestrictedMembers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newMeasureCorpus(t, d, ctx, conceptOntologyMD)
		k.doc(t, day(2026, 6, 3), k.rep) // /kb/m1
		k.doc(t, day(2026, 6, 4), k.rep) // /kb/m2, restricted
		k.doc(t, day(2026, 6, 5), "")    // /kb/m3, placed nowhere
		k.doc(t, day(2026, 6, 6), "")    // /kb/m4, placed nowhere, restricted
		restrictForSome(t, d, "/kb/m2", "/kb/m4")
		k.compute(t)
		june := monthStart(2026, 6)
		for _, reader := range []context.Context{k.f.operator, denying(t, k.f.reader, "/kb/m2", "/kb/m4")} {
			count := measureRun(t, d, reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep)).node(t, k.rep)
			if len(count.Points) != 1 || count.Points[0].Value != 1 {
				t.Errorf("count of reporting = %+v, want June's 1", count.Points)
			}
			share := measureRun(t, d, reader, fmt.Sprintf(`{"op":"measure","measure":"share","node":%q}`, k.rep)).node(t, k.rep)
			if len(share.Points) != 1 || share.Points[0].Basis == nil || *share.Points[0].Basis != 2 || share.Points[0].Value != 0.5 {
				t.Errorf("share of reporting = %+v, want 1 of a basis of 2", share.Points)
			}
			unplaced := measureRun(t, d, reader, `{"op":"measure","measure":"unplaced","scheme":"topics"}`)
			if p := unplaced.Nodes[0].Points; len(p) != 1 || p[0].PeriodStart != june || p[0].Value != 1 {
				t.Errorf("unplaced in topics = %+v, want June's 1 (m3)", p)
			}
		}
	})
}

// TestMeasure_PointsFromBeforeTheRuleAreNotServed — a row stored before C8 (a hash
// without the family) is read by nothing: not the latest answer, not shown_at, not
// the pass, which computes the point again.
func TestMeasure_PointsFromBeforeTheRuleAreNotServed(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newMeasureCorpus(t, d, ctx, conceptOntologyMD)
		k.doc(t, day(2026, 6, 3), k.rep)
		key, _, err := d.ontologyTenantKey(k.f.operator)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.ensureSchema(k.f.operator, key); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour).UnixNano()
		if err := d.exec(k.f.operator, key, `INSERT INTO measure_points (measure, node_id, subject_id, period_start, period,
			value_sum, members, groups, sources, basis, fallback_share, inputs_sha256, members_sha256, computed_at)
			VALUES (?, ?, '', ?, 'month', 7, 7, 0, 0, 0, 0, 'pre-c8-hash', 'pre-c8-members', ?)`,
			measureCount, k.rep, monthStart(2026, 6), old); err != nil {
			t.Fatal(err)
		}
		if rep := nodeStatsRun(t, d, k.f.operator, `{"op":"node_stats"}`); rep.Series == nil || rep.Series.Fresh != 0 {
			t.Fatalf("the pass compared with the pre-rule row: %+v", rep.Series)
		}
		k.compute(t)
		latest := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q}`, k.rep)).node(t, k.rep)
		if len(latest.Points) != 1 || latest.Points[0].Value != 1 {
			t.Errorf("latest = %+v, want the recomputed 1", latest.Points)
		}
		then := measureRun(t, d, k.f.reader, fmt.Sprintf(`{"op":"measure","measure":"count","node":%q,"shown_at":%d}`, k.rep, old+1))
		if p := then.node(t, k.rep).Points; len(p) != 0 {
			t.Errorf("shown_at before the rule = %+v, want nothing", p)
		}
		if n := len(rowsOf(measureRows(t, d, k.f.operator), measureCount, k.rep, monthStart(2026, 6))); n != 2 {
			t.Errorf("June count rows = %d, want the pre-rule row kept beside the new one", n)
		}
	})
}

// TestNodeAnnotation_RequestLeavesOutRestrictedMembers — a node's request counts,
// samples and draws terms from the unrestricted members only.
func TestNodeAnnotation_RequestLeavesOutRestrictedMembers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		restrictForSome(t, d, "/kb/a2")
		nodeStatsRun(t, d, k.f.operator, computeAll)
		in := nodeInputs(t, d, k.f.operator, "concept", k.rm)
		if in["members"] != float64(3) {
			t.Errorf("Risk Management request members = %v, want 3", in["members"])
		}
		for _, e := range anyList(in["exemplars"]) {
			if e.(map[string]any)["document_id"] == k.a2 {
				t.Errorf("a2 is an exemplar of the request")
			}
		}
		if strings.Contains(fmt.Sprint(in["terms"]), "oversight") || fmt.Sprint(in["member_classes"]) != "[map[count:3 name:Document]]" {
			t.Errorf("terms %v, classes %v; want neither from a2", in["terms"], in["member_classes"])
		}
	})
}

// TestNodeAnnotation_EveryReaderSeesTheRecord — a written record is served to a
// reader who may not read a restricted member exactly as to the operator, staleness
// included; a written record from before C8 is served to no one.
func TestNodeAnnotation_EveryReaderSeesTheRecord(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newStatsCorpus(t, d, ctx)
		restrictForSome(t, d, "/kb/b1")
		op, limited := k.f.operator, denying(t, k.f.reader, "/kb/b1")
		nodeWrite(t, d, op, "concept", k.rm, "Risk management")
		nodeWrite(t, d, op, "concept", k.rep, "Reporting")
		a, b := nodeRecords(t, d, op, "concept", k.rm), nodeRecords(t, d, limited, "concept", k.rm)
		if len(a) != 1 || !reflect.DeepEqual(a, b) {
			t.Fatalf("operator %v, limited reader %v; want the same record", a, b)
		}
		if a[0].(map[string]any)["stale"] != false {
			t.Errorf("record staleness = %v, want told and fresh", a[0].(map[string]any)["stale"])
		}
		key, _, err := d.ontologyTenantKey(op)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.exec(op, key, `UPDATE chunk_annotations SET inputs_sha256 = 'pre-c8-hash' WHERE chunk_id = ? AND level = 'concept'`, k.rep); err != nil {
			t.Fatal(err)
		}
		for _, ctx := range []context.Context{op, limited} {
			if got := nodeRecords(t, d, ctx, "concept", k.rep); len(got) != 0 {
				t.Errorf("a pre-rule written record was served: %v", got)
			}
		}
		list, _ := docExec(t, d, limited, `{"op":"list_annotations","scope":"tenant","level":"concept"}`)
		if n := len(anyList(list["annotations"])); n != 1 {
			t.Errorf("list_annotations level concept = %d records, want 1 (Risk Management)", n)
		}
	})
}
