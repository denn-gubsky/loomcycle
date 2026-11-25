package http

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyAPI_ConfirmTakesACatchUpThenAConfirmRelease (RFC GQ P3.3, owner ruling Q2).
//
// The first confirm of a draft nobody released takes a catch_up of the draft and then a
// confirm release, both naming the same content version (a confirm does not move it). A
// re-confirm with nothing changed takes none; one after a direct edit takes only the
// catch_up. Turning the ontology back to draft takes none, and confirming it again takes
// only the confirm, since the content in force already has a release.
func TestOntologyAPI_ConfirmTakesACatchUpThenAConfirmRelease(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{{title: "incident"}}},
	})
	dctx := s.ontologyDocCtx(context.Background(), mi)
	key := sqlmem.ScopeKey{Tenant: mi.Tenant, Scope: "tenant", ScopeID: mi.Tenant}
	type rel struct{ reason, version string }
	releases := func() []rel {
		t.Helper()
		res, err := s.sqlMem.DocPlane().Query(dctx, key,
			`SELECT reason, content_version FROM ontology_releases WHERE complete = 1 ORDER BY seq`, nil)
		if err != nil {
			t.Fatalf("read releases: %v", err)
		}
		var out []rel
		for _, r := range res.Rows {
			a, _ := r[0].(string)
			b, _ := r[1].(string)
			out = append(out, rel{a, b})
		}
		return out
	}
	want := func(stage string, reasons ...string) []rel {
		t.Helper()
		got := releases()
		ok := len(got) == len(reasons)
		for i := 0; ok && i < len(got); i++ {
			ok = got[i].reason == reasons[i]
		}
		if !ok {
			t.Fatalf("%s: releases %v, want %v", stage, got, reasons)
		}
		return got
	}

	draft := s.getOntology(t, mi.Tenant).ContentVersion
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got := want("first confirm", builtin.OntologyReleaseCatchUp, builtin.OntologyReleaseConfirm)
	if got[0].version != draft || got[1].version != draft {
		t.Errorf("first confirm: releases name %v, want both the draft's %s", got, draft)
	}

	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	want("re-confirm with nothing changed", builtin.OntologyReleaseCatchUp, builtin.OntologyReleaseConfirm)

	// A direct edit of the class's meaning moves the content version without a release.
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	exec := func(payload map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(payload)
		res, err := doc.Execute(dctx, b)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", payload["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	incident := s.getOntology(t, mi.Tenant).ClassChunks["incident"]
	g := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": incident})
	exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": incident, "revision": g["revision"], "body": "Something that went wrong.\n"})
	edited := s.getOntology(t, mi.Tenant).ContentVersion
	if edited == draft {
		t.Fatal("the edit did not move the content version; the test cannot tell the releases apart")
	}
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got = want("re-confirm after a direct edit",
		builtin.OntologyReleaseCatchUp, builtin.OntologyReleaseConfirm, builtin.OntologyReleaseCatchUp)
	if got[2].version != edited {
		t.Errorf("the catch_up names %s, want the edited %s", got[2].version, edited)
	}

	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyDraftStatus, http.StatusOK)
	want("back to draft",
		builtin.OntologyReleaseCatchUp, builtin.OntologyReleaseConfirm, builtin.OntologyReleaseCatchUp)
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	want("confirmed again",
		builtin.OntologyReleaseCatchUp, builtin.OntologyReleaseConfirm, builtin.OntologyReleaseCatchUp, builtin.OntologyReleaseConfirm)
}
