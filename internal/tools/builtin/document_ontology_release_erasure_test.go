package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// Subject erasure reaching ontology releases.

// releaseErasureFixture starts from a tenant as it stands before an erasure: a
// confirmed ontology whose instances Acme and Globex are adopted as subjects, each
// instance's meaning naming its contact, Acme's subject bound to the principal u-acme,
// and three releases taken while both were declared: one before adoption, two after
// (sharing one body). It returns the subjects bound to u-acme.
func releaseErasureFixture(t *testing.T, d *Document, base context.Context) (changeFixture, []SubjectBinding) {
	t.Helper()
	f := releaseFixtureOn(t, d, base)
	key := mustTenantKey(t, d, f.ctx)
	for name, who := range map[string]string{"Acme": "Jane Roe", "Globex": "John Poe"} {
		id := f.chunk(name)
		b, err := d.readBody(f.ctx, "tenant", key, id)
		if err != nil {
			t.Fatal(err)
		}
		if msg := f.update(f.ctx, id, map[string]any{"body": strings.TrimRight(b.Body, "\n") + "\n\nRun by " + who + ".\n"}); msg != "" {
			t.Fatalf("edit %s: %s", name, msg)
		}
	}
	// Before adoption the instance records no key: only its chunk id ties this copy to
	// the subject.
	takeRelease(t, d, f.ctx, OntologyReleaseCatchUp)
	if _, err := d.AdoptInstances(f.ctx, false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	acmeKey := instanceKeyOf(t, d, f, "Acme")
	if _, err := d.BindTenantSubjectPrincipal(f.ctx, acmeKey, "u-acme"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
	takeRelease(t, d, f.ctx, OntologyReleaseApply)
	bound, err := d.TenantSubjectsBoundTo(f.ctx, "u-acme")
	if err != nil || len(bound) != 1 {
		t.Fatalf("bound to u-acme: %+v %v", bound, err)
	}
	return f, bound
}

// instanceKeyOf is the subject key an adopted instance records.
func instanceKeyOf(t *testing.T, d *Document, f changeFixture, name string) string {
	t.Helper()
	read := f.read()
	for _, in := range read.Instances {
		if in.Name == name && in.RegistryKey != "" {
			return in.RegistryKey
		}
	}
	t.Fatalf("instance %s has no registry key", name)
	return ""
}

// TestReleaseErasure_RedactsTheBoundSubjectsInstanceAndKeepsAnother: an erasure redacts
// Acme's entry in every release (title, body and fields) and drops its body row, and
// keeps Globex's, its header, seq and recorded content version; the dry run counts the
// same and writes nothing; a second run finds nothing left.
func TestReleaseErasure_RedactsTheBoundSubjectsInstanceAndKeepsAnother(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, bound := releaseErasureFixture(t, d, base)
		acme, globex := f.chunk("Acme"), f.chunk("Globex")
		before := listedReleases(t, d, f.ctx)

		dry, err := d.EraseSubjectsFromReleases(f.ctx, bound, true)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if dry.Entries != 3 || dry.Releases != 3 || dry.InForce != 1 {
			t.Errorf("dry run = %+v, want 3 entries in 3 releases, 1 in force", dry)
		}
		// By the key alone (as for an instance added back under a new chunk id): the two
		// copies taken after adoption, which record it.
		if byKey, err := d.EraseSubjectsFromReleases(f.ctx, []SubjectBinding{{NaturalKey: bound[0].NaturalKey}}, true); err != nil || byKey.Entries != 2 {
			t.Errorf("dry run by key = %+v %v, want the 2 copies recording the key", byKey, err)
		}
		if e := releaseEntries(t, d, f.ctx, before[0].Seq)[acme]; !strings.Contains(e.body, "Jane Roe") {
			t.Fatalf("the dry run changed Acme's entry: %+v", e)
		}

		got, err := d.EraseSubjectsFromReleases(f.ctx, bound, false)
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
		if got != dry {
			t.Errorf("erasure = %+v, want what the dry run counted, %+v", got, dry)
		}
		for _, rel := range before {
			n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries
				WHERE release_seq = ? AND chunk_id = ? AND title = ? AND body_sha = ?`, rel.Seq, acme, releaseErasedTitle, releaseErasedSHA)
			if n != 1 {
				t.Errorf("release %d: Acme's entry is not marked erased", rel.Seq)
			}
			if e := releaseEntries(t, d, f.ctx, rel.Seq)[globex]; !strings.Contains(e.body, "John Poe") || e.title != "Globex" {
				t.Errorf("release %d: Globex's entry = %+v, want it kept", rel.Seq, e)
			}
			if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_releases WHERE seq = ?
				AND export_md NOT LIKE '%Jane Roe%' AND export_md LIKE '%John Poe%' AND export_md LIKE '%[erased]%'`, rel.Seq); n != 1 {
				t.Errorf("release %d: export_md still holds Acme's text, or lost Globex's", rel.Seq)
			}
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_bodies WHERE body LIKE '%Jane Roe%' OR fields LIKE ?`,
			"%"+instanceKeyOf(t, d, f, "Acme")+"%"); n != 0 {
			t.Errorf("%d body row(s) still hold Acme's text or key", n)
		}
		after := listedReleases(t, d, f.ctx)
		for i := range before {
			want := before[i]
			want.Redacted = true // the one change: flagged
			if after[i] != want {
				t.Errorf("header %d = %+v, want it kept as %+v", i, after[i], want)
			}
		}
		if again, err := d.EraseSubjectsFromReleases(f.ctx, bound, false); err != nil || again.Entries != 0 {
			t.Errorf("second erasure = %+v %v, want nothing left", again, err)
		}
	})
}

// TestReleaseErasure_DiffReportsTheEntryAsErased: after the erasure a release's diff,
// against another release or the ontology in force, lists Acme as erased with no state
// and no other change; Globex does not appear; a rollback to the release leaves Acme.
func TestReleaseErasure_DiffReportsTheEntryAsErased(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, bound := releaseErasureFixture(t, d, base)
		acme := f.chunk("Acme")
		rels := listedReleases(t, d, f.ctx)
		if _, err := d.EraseSubjectsFromReleases(f.ctx, bound, false); err != nil {
			t.Fatalf("erase: %v", err)
		}
		for _, to := range []int64{rels[0].Seq, 0} {
			diff, err := d.DiffOntologyReleases(f.ctx, rels[1].Seq, to)
			if err != nil {
				t.Fatalf("diff to %d: %v", to, err)
			}
			if len(diff.Changes) != 1 {
				t.Fatalf("diff to %d = %+v, want Acme alone", to, diff.Changes)
			}
			c := diff.Changes[0]
			if c.ChunkID != acme || c.Change != connector.OntologyDiffErased || c.Before != nil || c.After != nil {
				t.Errorf("diff to %d: %+v, want Acme erased with no state", to, c)
			}
		}
		exp, err := d.expandRollback(f.ctx, mustTenantKey(t, d, f.ctx), "tenant", rels[1].Seq)
		if err != nil {
			t.Fatalf("plan a rollback: %v", err)
		}
		for _, ch := range exp.direct {
			if ch.Entry == acme {
				t.Errorf("the rollback plans %s on the erased entry", ch.Op)
			}
		}
		if len(exp.direct)+len(exp.fixed) != 0 {
			t.Errorf("the rollback plans %+v %+v, want nothing", exp.direct, exp.fixed)
		}
	})
}

// TestReleaseErasure_ACandidateTheReleaseCannotClassifyIsRedacted: a copy cut off
// before it completed cannot be parsed (no reader lists it), so nothing shows what its
// entries are. Acme's entry in it holds Acme's text all the same, and is redacted with
// the complete releases' copies; Globex's beside it stays.
func TestReleaseErasure_ACandidateTheReleaseCannotClassifyIsRedacted(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, bound := releaseErasureFixture(t, d, base)
		acme := f.chunk("Acme")
		cut := errors.New("cut off")
		d.afterReleaseBatch = func() error { return cut }
		if _, err := d.TakeOntologyRelease(f.ctx, OntologyReleaseApply, nil); !errors.Is(err, cut) {
			t.Fatalf("take = %v, want the cut", err)
		}
		d.afterReleaseBatch = nil
		const incomplete = `SELECT count(*) FROM ontology_release_entries e JOIN ontology_releases r ON r.seq = e.release_seq
			WHERE r.complete = 0 AND e.title = ?`
		if releaseCount(t, d, f.ctx, incomplete, "Acme") != 1 {
			t.Fatal("the cut copy holds no entry of Acme; the test does not reach the state it is for")
		}

		got, err := d.EraseSubjectsFromReleases(f.ctx, bound, false)
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
		if got.Entries != 4 {
			t.Errorf("erasure = %+v, want Acme's 3 complete copies and the cut one", got)
		}
		if n := releaseCount(t, d, f.ctx, incomplete+` AND e.chunk_id = ? AND e.body_sha = ?`,
			releaseErasedTitle, acme, releaseErasedSHA); n != 1 {
			t.Error("Acme's entry in the cut copy is not erased")
		}
		if n := releaseCount(t, d, f.ctx, incomplete, "Globex"); n != 1 {
			t.Error("Globex's entry in the cut copy did not stay")
		}
	})
}

// TestReleaseErasure_OnlyInstanceEntriesAreRedacted (minimal pair): a subject root's
// instance_chunk is an ordinary field an operator write can set, and a key can sit in
// any entry's fields. With Acme's subject pointing at the Document class and the class
// carrying Acme's key, the erasure still redacts Acme's instance copies and leaves the
// class in every release untouched.
func TestReleaseErasure_OnlyInstanceEntriesAreRedacted(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, bound := releaseErasureFixture(t, d, base)
		key := mustTenantKey(t, d, f.ctx)
		class, acme := f.chunk("Document"), f.chunk("Acme")
		if err := d.mergeSubjectFields(f.ctx, key, "tenant", bound[0].RootChunkID, map[string]any{"instance_chunk": class}); err != nil {
			t.Fatalf("point the subject at the class: %v", err)
		}
		if msg := f.update(f.ctx, class, map[string]any{"fields": map[string]any{"registry_key": bound[0].NaturalKey}}); msg != "" {
			t.Fatalf("put the key on the class: %s", msg)
		}
		takeRelease(t, d, f.ctx, OntologyReleaseApply)
		rels := listedReleases(t, d, f.ctx)

		got, err := d.EraseSubjectsFromReleases(f.ctx, bound, false)
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
		// Acme by its key in the three releases taken after adoption; the copy from
		// before adoption is reached only by its chunk id, which the subject no longer
		// names.
		if got.Entries != 3 {
			t.Errorf("erasure = %+v, want Acme's 3 copies that record its key", got)
		}
		for _, rel := range rels {
			e := releaseEntries(t, d, f.ctx, rel.Seq)
			if c, ok := e[class]; !ok || c.title != "Document" || !strings.Contains(c.body, "`title` text") {
				t.Errorf("release %d: the class entry = %+v (stored %v), want it untouched", rel.Seq, c, ok)
			}
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries WHERE chunk_id = ? AND body_sha = ?`, acme, releaseErasedSHA); n != 3 {
			t.Errorf("%d of Acme's copies erased, want 3", n)
		}
	})
}

// planSummaryOf reads a release's stored plan_summary.
func planSummaryOf(t *testing.T, d *Document, ctx context.Context, seq int64) string {
	t.Helper()
	res, err := d.query(ctx, mustTenantKey(t, d, ctx), `SELECT coalesce(plan_summary, '') FROM ontology_releases WHERE seq = ?`, seq)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("read release %d's summary: %v", seq, err)
	}
	return asStr(res.Rows[0][0])
}

// TestReleaseErasure_ScrubsThePlanSummariesAndFlagsTheReleases: a release taken with a
// plan summary naming Acme (its title, chunk id and key) and Globex. The dry run counts
// the summary and changes nothing; the erasure replaces each of Acme's names with the
// erased marker in the summary's string values, keeps it valid JSON with Globex (minimal
// pair) and its other values as they were, and flags every release it changed as
// redacted, on the list, the read and the diff, keeping the recorded content version.
func TestReleaseErasure_ScrubsThePlanSummariesAndFlagsTheReleases(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, bound := releaseErasureFixture(t, d, base)
		acme, globex, acmeKey := f.chunk("Acme"), f.chunk("Globex"), bound[0].NaturalKey
		summary := map[string]any{
			"applied": []any{
				map[string]any{"op": "edit", "entry": acme, "name": "Acme", "note": "Acme renamed; Globex kept"},
				map[string]any{"op": "edit", "entry": globex, "name": "Globex"},
			},
			"subject": acmeKey, "count": 2,
		}
		rel, err := d.TakeOntologyRelease(f.ctx, OntologyReleaseApply, summary)
		if err != nil {
			t.Fatalf("take: %v", err)
		}
		before := planSummaryOf(t, d, f.ctx, rel.Seq)
		versions := map[int64]string{}
		for _, r := range listedReleases(t, d, f.ctx) {
			versions[r.Seq] = r.ContentVersion
		}

		dry, err := d.EraseSubjectsFromReleases(f.ctx, bound, true)
		if err != nil || dry.Summaries != 1 {
			t.Errorf("dry run = %+v %v, want 1 summary", dry, err)
		}
		if planSummaryOf(t, d, f.ctx, rel.Seq) != before {
			t.Fatal("the dry run changed the summary")
		}
		rels, err := d.ListOntologyReleases(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rels {
			if r.Redacted {
				t.Fatalf("release %d is flagged redacted before any erasure", r.Seq)
			}
		}

		got, err := d.EraseSubjectsFromReleases(f.ctx, bound, false)
		if err != nil || got.Summaries != 1 {
			t.Fatalf("erasure = %+v %v, want 1 summary", got, err)
		}
		after := planSummaryOf(t, d, f.ctx, rel.Seq)
		var parsed struct {
			Applied []struct {
				Entry, Name, Note string
			} `json:"applied"`
			Subject string `json:"subject"`
			Count   int    `json:"count"`
		}
		if err := json.Unmarshal([]byte(after), &parsed); err != nil {
			t.Fatalf("the scrubbed summary is not JSON: %v (%s)", err, after)
		}
		for _, name := range []string{"Acme", acme, acmeKey} {
			if strings.Contains(after, name) {
				t.Errorf("the summary still names %q: %s", name, after)
			}
		}
		if len(parsed.Applied) != 2 || parsed.Applied[0].Name != releaseErasedTitle || parsed.Applied[0].Entry != releaseErasedTitle ||
			parsed.Applied[0].Note != releaseErasedTitle+" renamed; Globex kept" || parsed.Applied[1].Name != "Globex" ||
			parsed.Applied[1].Entry != globex || parsed.Subject != releaseErasedTitle || parsed.Count != 2 {
			t.Errorf("scrubbed summary = %+v, want Acme's names erased and Globex's kept", parsed)
		}

		rels, err = d.ListOntologyReleases(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rels {
			if !r.Redacted || r.ContentVersion != versions[r.Seq] {
				t.Errorf("release %d = %+v, want flagged redacted with its content version %s", r.Seq, r, versions[r.Seq])
			}
		}
		if g, err := d.GetOntologyRelease(f.ctx, rel.Seq); err != nil || !g.Release.Redacted {
			t.Errorf("get = %+v %v, want redacted", g.Release, err)
		}
		if diff, err := d.DiffOntologyReleases(f.ctx, rel.Seq, 0); err != nil || !diff.From.Redacted || diff.To.Redacted {
			t.Errorf("diff sides = %+v / %+v %v, want the release redacted and current not", diff.From, diff.To, err)
		}
	})
}

// TestReleaseErasure_AScopeWithoutTheRedactedColumnReadsAndGainsIt starts from a tenant
// scope made before the column: its releases read as not redacted, and an erasure adds
// the column and flags them.
func TestReleaseErasure_AScopeWithoutTheRedactedColumnReadsAndGainsIt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, bound := releaseErasureFixture(t, d, base)
		if err := d.exec(f.ctx, mustTenantKey(t, d, f.ctx), `ALTER TABLE ontology_releases DROP COLUMN redacted`); err != nil {
			t.Fatalf("drop the column: %v", err)
		}
		rels, err := d.ListOntologyReleases(f.ctx)
		if err != nil || len(rels) != 3 || rels[0].Redacted {
			t.Fatalf("list on an old scope = %+v %v, want 3 releases, none redacted", rels, err)
		}
		if _, err := d.EraseSubjectsFromReleases(f.ctx, bound, false); err != nil {
			t.Fatalf("erase: %v", err)
		}
		if rels, err = d.ListOntologyReleases(f.ctx); err != nil || len(rels) != 3 || !rels[0].Redacted {
			t.Errorf("list after the erasure = %+v %v, want 3 releases, redacted", rels, err)
		}
	})
}
