package http

import (
	"context"
	"encoding/json"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyState_ReportsEntryChunksAndRetiredStatus (RFC GQ P2.2): the Ontology page
// retires a class or an instance by its chunk, so GET /v1/_ontology names each one's
// chunk, and a retired class reads back with its entry_status.
func TestOntologyState_ReportsEntryChunksAndRetiredStatus(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes", kids: []ontologyTestChunk{{title: "Memo"}, {title: "Ticket"}}},
		{title: "Instances", body: "@section instances", kids: []ontologyTestChunk{{title: "Acme", body: "@a organization"}}},
	})
	s.confirmOntology(t, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
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

	got := s.getOntology(t, mi.Tenant)
	memo := got.ClassChunks["Memo"]
	if memo == "" || got.ClassChunks["Ticket"] == "" || got.InstanceChunks["Acme"] == "" {
		t.Fatalf("class_chunks = %v, instance_chunks = %v; want Memo, Ticket and Acme named", got.ClassChunks, got.InstanceChunks)
	}
	if title, _ := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": memo})["title"].(string); title != "Memo" {
		t.Errorf("class_chunks[Memo] is the chunk titled %q", title)
	}

	g := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": memo})
	exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": memo, "revision": g["revision"], "status": meminject.OntologyStatusRetired})
	got = s.getOntology(t, mi.Tenant)
	for _, term := range got.Terms {
		want := ""
		if term.Name == "Memo" {
			want = meminject.OntologyStatusRetired
		}
		if term.EntryStatus != want {
			t.Errorf("term %s entry_status = %q, want %q", term.Name, term.EntryStatus, want)
		}
	}
	if got.ClassChunks["Memo"] != memo {
		t.Errorf("a retired class is no longer named: %v", got.ClassChunks)
	}
}
