package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Memory get and list read document rows by key: a chunk body, a fact homed in
// one, a derived unit. For a caller whose access-control set denies reading a
// document, they must answer as Document op=search and the Memory search do —
// a denied row reads as a missing one, and a listing leaves it out.

// memRowsACLCorpus is memACLCorpus with a derived unit of the secret chunk and a
// fact homed in the secret document's chunk. It returns the keys under test.
func memRowsACLCorpus(t *testing.T) (m *Memory, open, limited context.Context, secret, unit, public string) {
	t.Helper()
	m, open, limited, s := memACLCorpus(t)
	bg := context.Background()
	unit = memrank.UnitKey(s.secretChunk, memrank.UnitClaim, 0)
	uv, _ := json.Marshal(memrank.UnitValue{ChunkID: s.secretChunk, Kind: memrank.UnitClaim, Text: "the plan claims a savepoint"})
	if err := m.Store.MemorySet(bg, "tnt", store.MemoryScopeTenant, "", unit, uv, 0); err != nil {
		t.Fatal(err)
	}
	return m, open, limited, chunkBodyKey(s.secretChunk), unit, chunkBodyKey(s.publicChunk)
}

func TestMemoryGet_ACLDeniedDocumentRowReadsAsMissing(t *testing.T) {
	m, _, limited, secret, unit, public := memRowsACLCorpus(t)
	// The same caller asking for a key nobody wrote.
	missing := memRun(t, m, limited, `{"op":"get","scope":"tenant","key":"notes/never-written"}`)
	if missing.IsError {
		t.Fatalf("missing get: %s", missing.Text)
	}
	for _, key := range []string{secret, unit} {
		for _, extra := range []string{``, `,"include_provenance":true`} {
			got := memRun(t, m, limited, `{"op":"get","scope":"tenant","key":"`+key+`"`+extra+`}`)
			if got.IsError || got.Text != missing.Text {
				t.Errorf("get %s%s: %s, want the missing-key answer %s", key, extra, got.Text, missing.Text)
			}
		}
	}
	if got := memRun(t, m, limited, `{"op":"get","scope":"tenant","key":"`+public+`"}`); got.IsError || got.Text == missing.Text {
		t.Errorf("get of the readable chunk: %s", got.Text)
	}
}

func TestMemoryGet_ACLDeniedHomedFactReadsAsMissing(t *testing.T) {
	m, open, limited, secret, _, _ := memRowsACLCorpus(t)
	v, _ := json.Marshal("savepoint nesting secret fact")
	if err := m.Store.MemorySetProvenance(context.Background(), "tnt", store.MemoryScopeTenant, "", secret, v, 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "decision"}); err != nil {
		t.Fatal(err)
	}
	missing := memRun(t, m, limited, `{"op":"get","scope":"tenant","key":"notes/never-written"}`)
	if got := memRun(t, m, limited, `{"op":"get","scope":"tenant","key":"`+secret+`"}`); got.Text != missing.Text {
		t.Errorf("a fact homed in a read-denied document: %s, want %s", got.Text, missing.Text)
	}
	if got := memRun(t, m, open, `{"op":"get","scope":"tenant","key":"`+secret+`"}`); !strings.Contains(got.Text, "secret fact") {
		t.Errorf("an unrestricted caller lost the fact: %s", got.Text)
	}
}

func TestMemoryList_ACLDeniedDocumentRowsAreAbsent(t *testing.T) {
	m, _, limited, secret, unit, public := memRowsACLCorpus(t)
	for _, prefix := range []string{"", "doc.", "doc.chunk:", "doc.unit:"} {
		got := memRun(t, m, limited, `{"op":"list","scope":"tenant","prefix":"`+prefix+`","limit":1000}`)
		if got.IsError {
			t.Fatalf("list %q: %s", prefix, got.Text)
		}
		for _, k := range []string{secret, unit} {
			if strings.Contains(got.Text, k) {
				t.Errorf("list %q: names the read-denied row %s", prefix, k)
			}
		}
		if prefix != "doc.unit:" && !strings.Contains(got.Text, public) {
			t.Errorf("list %q: the readable chunk %s is missing: %.300s", prefix, public, got.Text)
		}
	}
}

// TestMemoryGetList_UnrestrictedCallerAnswerIsUnchanged — a caller with no set,
// and one whose set denies no document, read every row and get the same bytes.
func TestMemoryGetList_UnrestrictedCallerAnswerIsUnchanged(t *testing.T) {
	m, open, _, secret, unit, _ := memRowsACLCorpus(t)
	unlimited := tools.UpdateRunPolicy(open, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*"}})
	})
	for _, body := range []string{
		`{"op":"get","scope":"tenant","key":"` + secret + `"}`,
		`{"op":"get","scope":"tenant","key":"` + unit + `","include_provenance":true}`,
		`{"op":"list","scope":"tenant","prefix":"doc.","limit":1000}`,
	} {
		plain := memRun(t, m, open, body)
		if plain.IsError || !strings.Contains(plain.Text, "savepoint") {
			t.Fatalf("%s: %s, want the row", body, plain.Text)
		}
		if got := memRun(t, m, unlimited, body); got.Text != plain.Text {
			t.Errorf("%s: a set that denies nothing changed the answer:\n got %s\nwant %s", body, got.Text, plain.Text)
		}
	}
}
