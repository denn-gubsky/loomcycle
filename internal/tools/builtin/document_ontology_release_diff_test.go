package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// RFC GQ P3.4 — reading ontology releases: the diff.

func (f changeFixture) diff(from, to int64) connector.DiffOntologyReleasesResult {
	f.t.Helper()
	out, err := f.d.DiffOntologyReleases(f.ctx, from, to)
	if err != nil {
		f.t.Fatalf("diff %d..%d: %v", from, to, err)
	}
	return out
}

func diffJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestOntologyDiff_TwoReleasesDifferByExactlyTheEdit (the RFC's check for P3): a
// meaning edit of one class between two releases is the diff's one entry — that class,
// changed in its meaning only, with the meaning before and after — and a release
// diffed with itself, or the newest with the ontology in force, has no entry.
func TestOntologyDiff_TwoReleasesDifferByExactlyTheEdit(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		first := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		event := f.chunk("event")
		if r := f.change(f.ctx, "update_chunk", event, map[string]any{"body": "Something that happened.\n- `name` text\n"}); r.IsError {
			t.Fatalf("edit event: %s", r.Text)
		}
		second := takeRelease(t, d, f.ctx, OntologyReleaseApply)

		got := f.diff(first.Seq, second.Seq)
		if got.From.Seq != first.Seq || got.From.ContentVersion != first.ContentVersion ||
			got.To.Seq != second.Seq || got.To.ContentVersion != second.ContentVersion {
			t.Errorf("sides = %+v / %+v, want releases %d and %d with their content versions", got.From, got.To, first.Seq, second.Seq)
		}
		if len(got.Changes) != 1 {
			t.Fatalf("diff = %s, want exactly the edit of event", diffJSON(got.Changes))
		}
		c := got.Changes[0]
		if c.ChunkID != event || c.Kind != planKindClass || c.Change != connector.OntologyDiffChanged ||
			diffJSON(c.Changed) != `["meaning"]` || c.Before == nil || c.After == nil ||
			c.Before.Meaning != "" || c.After.Meaning != "Something that happened." || c.After.Name != "event" ||
			string(c.Before.Directives) != string(c.After.Directives) {
			t.Errorf("entry = %s, want event changed in its meaning only", diffJSON(c))
		}

		for _, pair := range [][2]int64{{first.Seq, first.Seq}, {second.Seq, 0}} {
			if got := f.diff(pair[0], pair[1]); len(got.Changes) != 0 || len(got.Issues) != 0 {
				t.Errorf("diff %d..%d = %s %v, want no change", pair[0], pair[1], diffJSON(got.Changes), got.Issues)
			}
		}
		if now := f.diff(second.Seq, 0).To; !now.Current || now.ContentVersion != currentContentVersion(t, d, f.ctx) {
			t.Errorf("to current = %+v, want the content version in force", now)
		}
		if _, err := d.DiffOntologyReleases(f.ctx, second.Seq+1, 0); connector.OntologyReleaseCode(err) != "release_not_found" {
			t.Errorf("a diff from a release that does not exist = %v, want release_not_found", err)
		}
	})
}

// TestOntologyDiff_RenameIsNotAnAddAndARemove: renaming a class with a subclass and a
// concept with a narrower one is two changed entries, each renamed only. The subclass
// and the narrower concept, whose parent's name (and, for the concept, tag and path)
// moved with the rename, are not reported.
func TestOntologyDiff_RenameIsNotAnAddAndARemove(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := releaseFixtureOn(t, d, base)
		first := takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
		renamed := map[string]string{}
		for old, now := range map[string]string{"Policy": "Rule", "Risk Management": "Risk"} {
			id := f.chunk(old)
			if r := f.change(f.ctx, "update_chunk", id, map[string]any{"title": now, "plan_fingerprint": f.planFingerprint(id)}); r.IsError {
				t.Fatalf("rename %s: %s", old, r.Text)
			}
			renamed[id] = old
		}
		second := takeRelease(t, d, f.ctx, OntologyReleaseApply)

		for _, to := range []int64{second.Seq, 0} {
			got := f.diff(first.Seq, to)
			if len(got.Changes) != 2 {
				t.Fatalf("diff to %d = %s, want the two renames only", to, diffJSON(got.Changes))
			}
			for _, c := range got.Changes {
				old, ok := renamed[c.ChunkID]
				if !ok || c.Change != connector.OntologyDiffChanged || diffJSON(c.Changed) != `["renamed"]` ||
					c.Before.Name != old || c.After.Name == old {
					t.Errorf("entry = %s, want a rename of one of %v", diffJSON(c), renamed)
				}
			}
		}
	})
}
