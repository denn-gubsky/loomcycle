package retention

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
