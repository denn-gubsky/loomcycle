package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// RFC GQ P3.3 — ontology releases.

// releaseFixtureOn is the change fixture on d: the retired ontology confirmed, no release
// taken yet.
func releaseFixtureOn(t *testing.T, d *Document, base context.Context) changeFixture {
	t.Helper()
	return changeFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}, agent: inRunCtx(base)}
}

// listedReleases is what a reader lists: complete releases, newest first.
func listedReleases(t *testing.T, d *Document, ctx context.Context) []OntologyRelease {
	t.Helper()
	rels, err := d.ontologyReleases(ctx, mustTenantKey(t, d, ctx), 0)
	if err != nil {
		t.Fatalf("list releases: %v", err)
	}
	return rels
}

func takeRelease(t *testing.T, d *Document, ctx context.Context, reason string) OntologyRelease {
	t.Helper()
	rel, err := d.TakeOntologyRelease(ctx, reason, nil)
	if err != nil {
		t.Fatalf("take a %s release: %v", reason, err)
	}
	return rel
}

// storedEntry is one release entry with its body, as stored.
type storedEntry struct {
	documentID, parentID, typ, status, title, body, fields string
	position                                               int
}

func releaseEntries(t *testing.T, d *Document, ctx context.Context, seq int64) map[string]storedEntry {
	t.Helper()
	res, err := d.query(ctx, mustTenantKey(t, d, ctx), `SELECT e.chunk_id, e.document_id, coalesce(e.parent_id, ''), e.position,
		coalesce(e.type, ''), coalesce(e.status, ''), e.title, b.body, b.fields
		FROM ontology_release_entries e JOIN ontology_release_bodies b ON b.sha = e.body_sha
		WHERE e.release_seq = ?`, seq)
	if err != nil {
		t.Fatalf("read release %d's entries: %v", seq, err)
	}
	out := map[string]storedEntry{}
	for _, r := range res.Rows {
		out[asStr(r[0])] = storedEntry{documentID: asStr(r[1]), parentID: asStr(r[2]), position: asInt(r[3]),
			typ: asStr(r[4]), status: asStr(r[5]), title: asStr(r[6]), body: asStr(r[7]), fields: asStr(r[8])}
	}
	return out
}

// countRows counts a release table's rows.
func releaseCount(t *testing.T, d *Document, ctx context.Context, q string, args ...any) int {
	t.Helper()
	res, err := d.query(ctx, mustTenantKey(t, d, ctx), q, args...)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("%s: %v", q, err)
	}
	return asInt(res.Rows[0][0])
}

// TestOntologyRelease_ConfirmStoresEveryEntryWithParentPositionStatusBodyFields: a
// release holds every in-force chunk of the ontology document — the root, the section
// headers, a retired entry — each with its parent, position, type, status, title, body
// and fields as stored, and a header naming the content version and fingerprint in force.
func TestOntologyRelease_ConfirmStoresEveryEntryWithParentPositionStatusBodyFields(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		f.retire("Memo")
		// As a confirm leaves them: the instances adopted, each with its registry key.
		if _, err := d.AdoptInstances(f.ctx, false); err != nil {
			t.Fatalf("adopt: %v", err)
		}
		read := f.read()
		key := mustTenantKey(t, d, f.ctx)

		rel := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		if rel.ContentVersion != currentContentVersion(t, d, f.ctx) || rel.Fingerprint != f.fingerprintNow() ||
			rel.Reason != OntologyReleaseConfirm || rel.Seq != 1 {
			t.Errorf("header = %+v, want seq 1, the content version %s and the fingerprint in force",
				rel, currentContentVersion(t, d, f.ctx))
		}

		res, err := d.query(f.ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ?`, read.DocumentID)
		if err != nil {
			t.Fatal(err)
		}
		got := releaseEntries(t, d, f.ctx, rel.Seq)
		if len(got) != len(res.Rows) || rel.Entries != len(res.Rows) {
			t.Errorf("release holds %d entries (header says %d), the document %d chunks", len(got), rel.Entries, len(res.Rows))
		}
		for _, r := range res.Rows {
			row := scanChunkRow(res.Columns, r)
			b, err := d.readBody(f.ctx, "tenant", key, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := storedEntry{documentID: read.DocumentID, parentID: row.ParentID, position: row.Position, typ: row.Type,
				status: row.Status, title: row.Title, body: b.Body, fields: string(b.Fields)}
			if got[row.ID] != want {
				t.Errorf("entry %s (%s) = %+v, want %+v", row.ID, row.Title, got[row.ID], want)
			}
		}
		if e := got[f.chunk("Memo")]; e.status != memrank.OntologyStatusRetired {
			t.Errorf("the retired entry is stored with status %q", e.status)
		}
		if e := got[f.chunk("Document")]; !strings.Contains(e.body, "`title` text") {
			t.Errorf("a class's field spec is not in its stored body: %q", e.body)
		}
		if e := got[f.chunk("Acme")]; !strings.Contains(e.fields, "registry_key") {
			t.Errorf("an adopted instance's fields are not stored: %q", e.fields)
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_releases WHERE seq = 1 AND complete = 1 AND export_md LIKE '%co_supervises%'`); n != 1 {
			t.Error("the header is not complete, or its export_md does not hold the ontology")
		}
	})
}

// TestOntologyRelease_ProposalsAreNotCopied: an add proposal, a change proposal and a
// rejected proposal stay out of the release; everything else is in it.
func TestOntologyRelease_ProposalsAreNotCopied(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		add := f.mustPropose(map[string]any{"section": "classes", "name": "Ledger"})["chunk_id"].(string)
		change := f.file(map[string]any{"change": "edit", "entry": f.chunk("supervises"), "meaning": "Oversight."})
		rejected := f.mustPropose(map[string]any{"section": "classes", "name": "Rumour"})["chunk_id"].(string)
		if msg := f.setStatus(f.ctx, rejected, memrank.OntologyStatusRejected); msg != "" {
			t.Fatalf("reject: %s", msg)
		}
		rel := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		got := releaseEntries(t, d, f.ctx, rel.Seq)
		for name, id := range map[string]string{"add": add, "change": change, "rejected": rejected} {
			if _, in := got[id]; in {
				t.Errorf("the %s proposal %s was copied", name, id)
			}
		}
		chunks := releaseCount(t, d, f.ctx, `SELECT count(*) FROM chunks WHERE document_id = ?`, f.read().DocumentID)
		if len(got) != chunks-3 {
			t.Errorf("release holds %d entries, want every chunk but the three proposals (%d)", len(got), chunks-3)
		}
	})
}

// TestOntologyRelease_FiftyFirstReleaseRemovesTheOldestAndItsUnsharedBodies: 50
// releases are kept. The 51st removes the first, its entries, and the one body only it
// held; a body it shared with a kept release stays.
func TestOntologyRelease_FiftyFirstReleaseRemovesTheOldestAndItsUnsharedBodies(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		provision := f.chunk("Provision")
		// A body no other chunk has, so only the first release holds it.
		if msg := f.update(f.ctx, provision, map[string]any{"body": "A rule, first wording.\n"}); msg != "" {
			t.Fatalf("word Provision: %s", msg)
		}
		first := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		oldBody := releaseEntries(t, d, f.ctx, first.Seq)[provision].body
		if oldBody != "A rule, first wording.\n" {
			t.Fatalf("the first release stored Provision as %q", oldBody)
		}
		if msg := f.update(f.ctx, provision, map[string]any{"body": "A numbered rule.\n"}); msg != "" {
			t.Fatalf("edit Provision: %s", msg)
		}
		for range ontologyReleasesKept {
			takeRelease(t, d, f.ctx, OntologyReleaseApply)
		}
		rels := listedReleases(t, d, f.ctx)
		if len(rels) != ontologyReleasesKept || rels[len(rels)-1].Seq != 2 || rels[0].Seq != 51 {
			t.Fatalf("listed %d releases, seq %d..%d; want 50, seq 2..51", len(rels), rels[len(rels)-1].Seq, rels[0].Seq)
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries WHERE release_seq = 1`); n != 0 {
			t.Errorf("the removed release's %d entries stayed", n)
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_bodies WHERE sha = ?`, releaseBodySHA(oldBody, "")); n != 0 {
			t.Error("the body only the removed release held stayed")
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_bodies WHERE body = ?`, "A numbered rule.\n"); n != 1 {
			t.Errorf("the kept body is stored %d times, want once", n)
		}
		// Every entry still finds its body, and bodies are stored once per hash: far fewer
		// than 50 releases' worth of entries.
		entries := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries`)
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries e
			LEFT JOIN ontology_release_bodies b ON b.sha = e.body_sha WHERE b.sha IS NULL`); n != 0 {
			t.Errorf("%d kept entries lost their body", n)
		}
		if bodies := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_bodies`); bodies*10 > entries {
			t.Errorf("%d bodies for %d entries: bodies are not shared between releases", bodies, entries)
		}
	})
}

// TestOntologyRelease_InterruptedCopyIsNotListed: a copy cut off after its first batch
// leaves an incomplete header that no reader lists and the catch-up check does not count;
// the next release removes it and its entries.
func TestOntologyRelease_InterruptedCopyIsNotListed(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		cut := errors.New("cut off")
		d.afterReleaseBatch = func() error { return cut }
		if _, err := d.TakeOntologyRelease(f.ctx, OntologyReleaseConfirm, nil); !errors.Is(err, cut) {
			t.Fatalf("take = %v, want the cut", err)
		}
		d.afterReleaseBatch = nil
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries WHERE release_seq = 1`); n == 0 {
			t.Fatal("the cut copy wrote no entries; the test does not reach the state it is for")
		}
		if rels := listedReleases(t, d, f.ctx); len(rels) != 0 {
			t.Errorf("the incomplete release is listed: %+v", rels)
		}
		// Not counted by the catch-up check either: the version in force has no release.
		rel, took, err := d.CatchUpOntologyRelease(f.ctx)
		if err != nil || !took {
			t.Fatalf("catch_up = %v %v, want a release taken", took, err)
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_releases WHERE complete = 0`); n != 0 {
			t.Errorf("%d incomplete headers stayed after the next release", n)
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_release_entries WHERE release_seq <> ?`, rel.Seq); n != 0 {
			t.Errorf("the cut copy's %d entries stayed", n)
		}
		if rels := listedReleases(t, d, f.ctx); len(rels) != 1 || rels[0].Reason != OntologyReleaseCatchUp {
			t.Errorf("listed %+v, want the one catch_up", rels)
		}
	})
}

// TestOntologyRelease_EditDuringTheCopyIsRetried: an edit landing while the copy is read
// makes it be taken again, so the release holds the edited ontology and the content
// version that goes with it; an ontology that never stands still gets no release.
func TestOntologyRelease_EditDuringTheCopyIsRetried(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		provision := f.chunk("Provision")
		before := currentContentVersion(t, d, f.ctx)
		edits := 0
		d.duringReleaseCopy = func() {
			if edits == 0 {
				edits++
				if msg := f.update(f.ctx, provision, map[string]any{"body": "Edited mid-copy.\n"}); msg != "" {
					t.Fatalf("edit: %s", msg)
				}
			}
		}
		rel := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		d.duringReleaseCopy = nil
		after := currentContentVersion(t, d, f.ctx)
		if after == before {
			t.Fatal("the edit did not move the content version; the test cannot tell the copies apart")
		}
		if rel.ContentVersion != after || rel.Fingerprint != f.fingerprintNow() {
			t.Errorf("release names %s, want the edited ontology's %s", rel.ContentVersion, after)
		}
		if got := releaseEntries(t, d, f.ctx, rel.Seq)[provision].body; got != "Edited mid-copy.\n" {
			t.Errorf("Provision is stored as %q, want the edit", got)
		}

		n := 0
		d.duringReleaseCopy = func() {
			n++
			if msg := f.update(f.ctx, provision, map[string]any{"body": fmt.Sprintf("Edit %d.\n", n)}); msg != "" {
				t.Fatalf("edit: %s", msg)
			}
		}
		if _, err := d.TakeOntologyRelease(f.ctx, OntologyReleaseConfirm, nil); !errors.Is(err, ErrOntologyMovedDuringRelease) {
			t.Errorf("take under constant edits = %v, want ErrOntologyMovedDuringRelease", err)
		}
		d.duringReleaseCopy = nil
		if n != ontologyReleaseRetries+1 {
			t.Errorf("copied %d times, want %d", n, ontologyReleaseRetries+1)
		}
		if rels := listedReleases(t, d, f.ctx); len(rels) != 1 {
			t.Errorf("listed %d releases, want only the first", len(rels))
		}
	})
}

// TestOntologyRelease_DeepSchemeKeepsAllEightLevels: a concept chain eight levels deep
// under its scheme is stored whole, each level under its own parent, and rendered in the
// release's export.
func TestOntologyRelease_DeepSchemeKeepsAllEightLevels(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		read := f.read()
		parent := f.chunk("topics")
		chain := make([]string, 0, 8)
		for level := 1; level <= 8; level++ {
			out := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
				"parent_id": parent, "title": fmt.Sprintf("Level %d", level), "body": fmt.Sprintf("Concept at depth %d.", level)})
			parent = out["id"].(string)
			chain = append(chain, parent)
		}
		rel := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		got := releaseEntries(t, d, f.ctx, rel.Seq)
		up := f.chunk("topics")
		for i, id := range chain {
			e, ok := got[id]
			if !ok || e.parentID != up || e.title != fmt.Sprintf("Level %d", i+1) || e.body != fmt.Sprintf("Concept at depth %d.", i+1) {
				t.Errorf("level %d: %+v (stored %v), want under %s", i+1, e, ok, up)
			}
			up = id
		}
		if n := releaseCount(t, d, f.ctx, `SELECT count(*) FROM ontology_releases WHERE seq = ? AND export_md LIKE '%Concept at depth 8.%'`, rel.Seq); n != 1 {
			t.Error("the export does not reach the eighth level")
		}
	})
}

// TestOntologyRelease_AcceptOnAnOntologyWithNoReleaseTakesCatchUpThenApply starts from a
// tenant as it is today: a confirmed ontology with things resting on it and no release.
// The first accepted change takes a catch_up of the ontology before it, then an apply
// release after it, with the report as its plan summary. The next accept takes only an
// apply; a direct edit between accepts brings the catch_up back.
func TestOntologyRelease_AcceptOnAnOntologyWithNoReleaseTakesCatchUpThenApply(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		seedFingerprintDependents(f.retiredFixture)
		if rels := listedReleases(t, d, f.ctx); len(rels) != 0 {
			t.Fatalf("the fixture starts with releases: %+v", rels)
		}
		original := currentContentVersion(t, d, f.ctx)

		accept := func(entry, meaning string) {
			t.Helper()
			id := f.file(map[string]any{"change": "edit", "entry": f.chunk(entry), "meaning": meaning})
			rep, err := f.accept(id, f.fingerprintNow())
			if err != nil {
				t.Fatalf("accept: %v", err)
			}
			for _, w := range rep.Warnings {
				if strings.Contains(w, "release") {
					t.Errorf("accept warned: %s", w)
				}
			}
		}
		reasons := func() []string {
			var out []string
			for _, r := range listedReleases(t, d, f.ctx) {
				out = append(out, r.Reason)
			}
			return out
		}

		accept("supervises", "One person oversees another.")
		rels := listedReleases(t, d, f.ctx)
		if len(rels) != 2 || rels[1].Reason != OntologyReleaseCatchUp || rels[0].Reason != OntologyReleaseApply {
			t.Fatalf("releases = %v, want catch_up then apply", reasons())
		}
		if rels[1].ContentVersion != original {
			t.Errorf("the catch_up holds %s, want the ontology before the change (%s)", rels[1].ContentVersion, original)
		}
		if now := currentContentVersion(t, d, f.ctx); rels[0].ContentVersion != now || now == original {
			t.Errorf("the apply release holds %s, want the changed ontology %s", rels[0].ContentVersion, now)
		}
		var summary map[string]any
		res, err := d.query(f.ctx, mustTenantKey(t, d, f.ctx), `SELECT plan_summary FROM ontology_releases WHERE seq = ?`, rels[0].Seq)
		if err != nil || len(res.Rows) != 1 || json.Unmarshal([]byte(asStr(res.Rows[0][0])), &summary) != nil {
			t.Fatalf("plan_summary: %v %v", err, res)
		}
		if summary["op"] != memrank.ChangeOpEdit || summary["target_name"] != "supervises" {
			t.Errorf("plan_summary = %v, want the accept's report", summary)
		}

		accept("filed_under", "Filed under a concept.")
		if got := reasons(); len(got) != 3 || got[0] != OntologyReleaseApply {
			t.Errorf("after a second accept releases = %v, want one more apply and no catch_up", got)
		}

		if msg := f.update(f.ctx, f.chunk("Provision"), map[string]any{"body": "A numbered rule.\n"}); msg != "" {
			t.Fatalf("direct edit: %s", msg)
		}
		accept("supervises", "Oversight.")
		if got := reasons(); len(got) != 5 || got[1] != OntologyReleaseCatchUp || got[0] != OntologyReleaseApply {
			t.Errorf("after a direct edit and an accept releases = %v, want a catch_up then an apply", got)
		}
	})
}
