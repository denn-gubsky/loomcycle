package builtin

import (
	"context"
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
			if after[i] != before[i] {
				t.Errorf("header %d = %+v, want it kept as %+v", i, after[i], before[i])
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
