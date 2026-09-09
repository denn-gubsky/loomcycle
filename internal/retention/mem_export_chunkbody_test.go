package retention

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestSweeper_MemExportIncludesChunkBodyRows is RFC CX-1: a characterisation test
// pinning a property RFC CV's P2 depends on, BEFORE P2 lands.
//
// WHY IT EXISTS. CV's P2 collapses a fact's `memory/<class>/<slug>` row onto its
// chunk, leaving the fact's text in the k/v plane only as a `doc.chunk:<hex>` row
// with its structure and provenance in SQL Memory. RFC CX-2 was drafted on the
// assumption that the retired-agent export would then silently lose the fact text
// — "a retention subsystem that quietly drops what it is supposed to preserve is
// the worst failure mode available to it".
//
// ⚠️ THAT ASSUMPTION IS WRONG, and this test is the evidence. `exportBaseMemory`
// lists with an EMPTY key prefix, so it already exports every k/v row in the
// scope including `doc.chunk:` bodies; and the reclaim separately calls
// `exportSQLMemScope` before `DropScope`, which carries the chunks and
// `chunk_memory_meta`. Both halves of a post-P2 fact are already covered, so CX-2
// needs no code change.
//
// The test is still worth having — arguably more so. The property is INCIDENTAL:
// it holds because a prefix is empty. Narrowing that prefix to `memory/` looks
// like a harmless optimisation and would silently reinstate CV P2's blocker. This
// test is what makes that a red build instead of a data-loss incident.
func TestSweeper_MemExportIncludesChunkBodyRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	const factText = "Dave moved to Berlin in July 2023."

	seedRetiredAgent(t, st, "acme", "dead", 1)
	seedAgentSQLScope(t, sm, "acme", "dead")
	seedAgentDirent(t, st, "acme", "dead")

	// The POST-P2 shape of a fact: the body lives only as a doc.chunk: row. No
	// `memory/<class>/<slug>` companion, which is exactly what P2 removes.
	body, _ := json.Marshal(map[string]string{"body": factText})
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "dead",
		"doc.chunk:9f2c1b7e4a", json.RawMessage(body), 0); err != nil {
		t.Fatalf("seed chunk body row: %v", err)
	}

	sw := New(st, Config{MemMode: "export+prune", ExportDir: dir, SQLMem: sm, Logger: quietLogger, Now: futureHour})
	if _, err := sw.sweepOnce(ctx); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	// The fact TEXT must appear in the agent-memory export. Asserted on content,
	// not on a row count: a count can be right while the bytes that matter are
	// missing, and it is the text an operator needs back.
	var exported []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if d == nil || d.IsDir() || filepath.Base(filepath.Dir(p)) != "agent-memory" {
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr == nil {
			exported = append(exported, string(b))
		}
		return nil
	})
	if len(exported) == 0 {
		t.Fatalf("no agent-memory export file was written under %s", dir)
	}
	joined := strings.Join(exported, "\n")
	if !strings.Contains(joined, factText) {
		t.Errorf("the agent-memory export does not contain the fact text %q.\n\n"+
			"After RFC CV P2 a fact's text lives ONLY as a doc.chunk: row, so an export that "+
			"skips those rows deletes an agent's memory having preserved none of it. This holds "+
			"today because exportBaseMemory lists with an EMPTY prefix — if that prefix was "+
			"narrowed, restore it.\n\nexport contents:\n%s", factText, joined)
	}

	// And the scope really was dropped afterwards, so this is export-THEN-delete
	// rather than an export that happened to run beside a survivor.
	entries, _, err := st.MemoryList(ctx, "", store.MemoryScopeAgent, "dead", "", 100)
	if err != nil {
		t.Fatalf("MemoryList after sweep: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("base memory survived export+prune: %d rows remain", len(entries))
	}
}

// TestSweeper_MemExportCarriesTemporalColumns — the reclamation export must
// carry a fact's dates, not just its text.
//
// Unlike the snapshot path, this one needed no fix: exportBaseMemory marshals
// store.MemoryEntry straight out of MemoryList, so it inherited the three
// columns when MemoryList started reading them. That makes the property
// INCIDENTAL in the same way the chunk-body coverage above is incidental — it
// holds because of a decision made in a different file. An export is the last
// copy of an agent's memory before the rows are pruned, so a silently undated
// export is unrecoverable; this test is what turns a future regression in the
// projection into a red build instead of a quiet loss.
func TestSweeper_MemExportCarriesTemporalColumns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	seedRetiredAgent(t, st, "acme", "dead", 1)
	seedAgentSQLScope(t, sm, "acme", "dead")
	seedAgentDirent(t, st, "acme", "dead")

	observed := time.Date(2023, 7, 7, 19, 56, 0, 0, time.UTC)
	body, _ := json.Marshal(map[string]string{"body": "Dave moved to Berlin in July 2023."})
	if err := st.MemorySetTimed(ctx, "", store.MemoryScopeAgent, "dead",
		"doc.chunk:9f2c1b7e4a", json.RawMessage(body), 0, store.MemoryProvenance{},
		store.MemoryTimes{ObservedAt: observed}); err != nil {
		t.Fatalf("seed dated chunk body row: %v", err)
	}

	sw := New(st, Config{MemMode: "export+prune", ExportDir: dir, SQLMem: sm, Logger: quietLogger, Now: futureHour})
	if _, err := sw.sweepOnce(ctx); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	var blob string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if d == nil || d.IsDir() || filepath.Base(filepath.Dir(p)) != "agent-memory" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil {
			blob = string(b)
		}
		return nil
	})
	if blob == "" {
		t.Fatalf("no agent-memory export file was written under %s", dir)
	}
	// Asserted on the parsed INSTANT, not on a literal string: the export
	// marshals time.Time in the host's local zone (e.g. "+03:00"), so a
	// string match on a "Z" form passes or fails by machine timezone rather
	// than by whether the date survived.
	var exported []struct {
		Key        string    `json:"key"`
		ObservedAt time.Time `json:"observed_at"`
	}
	if err := json.Unmarshal([]byte(blob), &exported); err != nil {
		t.Fatalf("unmarshal export: %v\n\n%s", err, blob)
	}
	var found bool
	for _, e := range exported {
		if e.Key != "doc.chunk:9f2c1b7e4a" {
			continue
		}
		found = true
		if !e.ObservedAt.UTC().Equal(observed) {
			t.Errorf("exported observed_at = %v, want %v.\n\nAn export is the LAST copy of "+
				"these rows before the prune deletes them, so a date missing here is gone for "+
				"good. The field rides along only because exportBaseMemory marshals "+
				"store.MemoryEntry from MemoryList — if that projection stops reading the "+
				"column, this is where it costs data.\n\nexport:\n%s",
				e.ObservedAt.UTC(), observed, blob)
		}
	}
	if !found {
		t.Fatalf("the seeded row is not in the export:\n%s", blob)
	}

	// PRE-EXISTING WART, recorded here rather than fixed: `omitempty` on a
	// time.Time does nothing (encoding/json does not treat a zero struct as
	// empty), so an UNDATED row exports as "valid_at": "0001-01-01T00:00:00Z"
	// — a wrong date where the honest answer is no date, in the one file an
	// operator reads after the rows are gone. Fixing it means pointer fields
	// on store.MemoryEntry, which changes every API response that marshals
	// it, so it wants its own change. The snapshot archive type does use
	// *time.Time + omitempty and correctly omits the key.
	if !strings.Contains(blob, `"valid_at": "0001-01-01T00:00:00Z"`) {
		t.Log("note: the year-1 serialisation of an undated valid_at is gone — if that was " +
			"deliberate, drop this assertion and the comment above it")
	}
}
