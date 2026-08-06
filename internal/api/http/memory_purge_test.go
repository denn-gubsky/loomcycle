package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// purgeStore records which embeddings were deleted, and starts with one on every row —
// the shape a scope has after being swept by a build that predates the scaffold rule.
type purgeStore struct {
	store.Store
	values  map[string]string // key → stored body text
	embeds  map[string]bool   // key → has an embedding
	deleted []string
}

func newPurgeStore(base store.Store, values map[string]string) *purgeStore {
	p := &purgeStore{Store: base, values: values, embeds: map[string]bool{}}
	for k := range values {
		p.embeds[k] = true
	}
	return p
}

func (p *purgeStore) MemoryList(_ context.Context, _ string, _ store.MemoryScope,
	_, keyPrefix string, limit int) ([]store.MemoryEntry, bool, error) {
	var out []store.MemoryEntry
	for k, body := range p.values {
		if !strings.HasPrefix(k, keyPrefix) {
			continue
		}
		v, _ := json.Marshal(map[string]any{"body": body})
		out = append(out, store.MemoryEntry{Key: k, Value: v})
	}
	if limit > 0 && len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func (p *purgeStore) MemoryEmbedDelete(_ context.Context, _ string, _ store.MemoryScope,
	_, key string) error {
	p.deleted = append(p.deleted, key)
	delete(p.embeds, key)
	return nil
}

func postPurge(t *testing.T, srv *Server, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/purge_stale_embeddings?scope=user&scope_id=alice&tenant=&prefix=doc.chunk:"+query, nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryPurgeStaleEmbeddings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// purgeFixture is the measured shape: scaffolding-only bodies that were embedded before
// the write path rejected them, alongside real prose that must be left alone.
func purgeFixture(t *testing.T) (*Server, *purgeStore) {
	t.Helper()
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ps := newPurgeStore(srv.store, map[string]string{
		"doc.chunk:scaffold-sh":   "```sh",
		"doc.chunk:scaffold-bash": "```bash",
		"doc.chunk:scaffold-rule": "---",
		"doc.chunk:scaffold-hash": "#",
		"doc.chunk:real-prose":    "SAVEPOINT nesting is LIFO",
		"doc.chunk:real-fenced":   "```sh\nmake build-all\n```",
	})
	srv.store = ps
	return srv, ps
}

// TestPurgeStaleEmbeddings_RemovesScaffoldingOnly is the whole point, and the second
// half is the part that matters: a purge that deleted one row too many would silently
// un-index real content, which is far worse than the noise it set out to remove.
func TestPurgeStaleEmbeddings_RemovesScaffoldingOnly(t *testing.T) {
	srv, ps := purgeFixture(t)

	out := postPurge(t, srv, "&dry_run=false")
	if n, _ := out["purged"].(float64); n != 4 {
		t.Errorf("purged = %v, want 4 scaffolding rows: %v", out["purged"], out)
	}
	for _, keep := range []string{"doc.chunk:real-prose", "doc.chunk:real-fenced"} {
		if !ps.embeds[keep] {
			t.Errorf("%s lost its embedding — real content was un-indexed", keep)
		}
	}
	// A body that merely BEGINS with a fence is a code example, not scaffolding. Getting
	// that wrong would delete every code sample from the index.
	for _, d := range ps.deleted {
		if d == "doc.chunk:real-fenced" {
			t.Error("a fenced code EXAMPLE was treated as scaffolding")
		}
	}
}

// TestPurgeStaleEmbeddings_DryRunDeletesNothing — this endpoint removes index entries,
// so the safe default matters more here than on the sweeps that only add.
func TestPurgeStaleEmbeddings_DryRunDeletesNothing(t *testing.T) {
	srv, ps := purgeFixture(t)

	out := postPurge(t, srv, "")
	if out["dry_run"] != true {
		t.Errorf("dry_run should default TRUE, got %v", out["dry_run"])
	}
	if n, _ := out["stale"].(float64); n != 4 {
		t.Errorf("stale = %v, want 4 — a dry run must still REPORT what it would do", out["stale"])
	}
	if n, _ := out["purged"].(float64); n != 0 {
		t.Errorf("purged = %v on a dry run", out["purged"])
	}
	if len(ps.deleted) != 0 {
		t.Errorf("a dry run deleted %v", ps.deleted)
	}
}

// TestPurgeStaleEmbeddings_AgreesWithTheBackfill pins the invariant the two sweeps rest
// on: whatever the backfill would EMBED, the purge must not remove. They derive text
// through the same function, so this is a guard against them drifting apart — a purge
// with its own notion of "indexable" would delete rows the next backfill re-creates,
// and the pair would fight forever.
func TestPurgeStaleEmbeddings_AgreesWithTheBackfill(t *testing.T) {
	// A TABLE, not a tautology. A first version of this compared `text != ""` against
	// `text == ""` and asserted they differ — which they do by construction, so it
	// tested nothing. The real question is whether each body lands on the side the
	// WRITE path puts it on, so the expected classification has to be written down.
	for _, tc := range []struct {
		body      string
		embedable bool
	}{
		// Scaffolding: the writer rejects these, so the sweep must not create them and
		// the purge must remove them. This is the concrete regression — before the
		// shared predicate the backfill embedded every one.
		{"```sh", false},
		{"```bash", false},
		{"---", false},
		{"#", false},
		{"", false},
		{"   ", false},
		{"42", false}, // no letters
		// Real content, including bodies that merely BEGIN with a fence. Treating a code
		// example as scaffolding would delete every sample from the index.
		{"SAVEPOINT nesting is LIFO", true},
		{"```sh\nmake build\n```", true},
		{"# A real heading", true},
		{"-- a SQL comment", true},
	} {
		v, _ := json.Marshal(map[string]any{"body": tc.body})
		got := embedTextForRow(store.MemoryEntry{Key: "doc.chunk:x", Value: v}) != ""
		if got != tc.embedable {
			verb := "would NOT embed"
			if got {
				verb = "WOULD embed"
			}
			t.Errorf("the backfill %s %q; the write path disagrees. When the sweep and the "+
				"writer differ, one creates what the other rejects and the purge cannot tell "+
				"which is right", verb, tc.body)
		}
	}
}

// TestPurgeStaleEmbeddings_AdminMustNameATenant — the same refusal as the backfill, and
// it matters more here: this operation DELETES, so resolving to the default tenant would
// purge a tenant the operator never named.
func TestPurgeStaleEmbeddings_AdminMustNameATenant(t *testing.T) {
	srv, _ := purgeFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/purge_stale_embeddings?scope=user&scope_id=alice&prefix=doc.chunk:", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryPurgeStaleEmbeddings(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 when an admin names no tenant", rec.Code)
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "tenant_required" {
		t.Errorf("code = %v, want tenant_required", e["code"])
	}
}

// TestPurgeStaleEmbeddings_TruncatedScanSaysSo — a low `stale` count on a partial scan
// is not evidence the scope is clean, and an operator reading it as such would stop
// sweeping too early.
func TestPurgeStaleEmbeddings_TruncatedScanSaysSo(t *testing.T) {
	srv, _ := purgeFixture(t)
	out := postPurge(t, srv, "&limit=2")
	if out["truncated"] != true {
		t.Errorf("truncated = %v with limit=2 over 6 rows", out["truncated"])
	}
	body, _ := json.Marshal(out["notes"])
	if !strings.Contains(string(body), "SCAN INCOMPLETE") {
		t.Errorf("notes do not warn that the scan was partial: %s", body)
	}
}
