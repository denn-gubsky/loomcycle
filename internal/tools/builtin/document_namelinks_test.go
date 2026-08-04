package builtin

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC BS Phase 2a — inline `[[name]]` links materialized as typed graph edges.
//
// A chunk body's `[[target]]` name-links become `references` edges tagged auto=1,
// re-derived on every body write. These tests pin the parser (`![[…]]` embeds and
// `|display` aliases), the scope-confined resolver (Path + title), the reconcile
// (idempotent, auto=1-only sweep, manual-edge preservation), and the hooks
// (create_chunk / body-bearing update_chunk only). The SQL-portability-sensitive
// paths run on BOTH SQL Memory tiers via the same pgDocumentOrSkip harness the
// Phase-1 tags test uses; the pure parser/gate/id-stability cases are sqlite-only
// (tier-independent Go logic).

// --- edge-reading helpers ---

// edgeRow is a chunk_edges row for name-link assertions.
type edgeRow struct {
	from, to, kind string
	auto           bool
}

// edgesFrom returns every chunk_edges row with from_id == fromID in the user
// scope, sorted (to_id, kind) for stable comparison.
func edgesFrom(t *testing.T, d *Document, ctx context.Context, fromID string) []edgeRow {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	res, err := d.query(ctx, key, `SELECT from_id, to_id, kind, auto FROM chunk_edges WHERE from_id = ? ORDER BY to_id, kind`, fromID)
	if err != nil {
		t.Fatalf("edgesFrom query: %v", err)
	}
	out := make([]edgeRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, edgeRow{from: asStr(r[0]), to: asStr(r[1]), kind: asStr(r[2]), auto: asInt(r[3]) == 1})
	}
	return out
}

// getEdgesEdges runs the get_edges op and returns its edge maps.
func getEdgesEdges(t *testing.T, d *Document, ctx context.Context, docID string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"get_edges","scope":"user","document_id":"`+docID+`"}`)
	if r.IsError {
		t.Fatalf("get_edges: %s", r.Text)
	}
	raw, _ := out["edges"].([]any)
	edges := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		edges = append(edges, e.(map[string]any))
	}
	return edges
}

// --- the tier-portable core (runs on sqlite AND postgres) ---

// TestDocumentNameLinks_CoreBothTiers exercises the SQL-portability-sensitive
// name-link paths — the migrateEdgeAuto DDL, the reconcile INSERT…SELECT…WHERE
// NOT EXISTS guard + the auto=1-only DELETE, both resolver branches, and the
// get_edges auto surfacing — on BOTH SQL Memory tiers. The postgres half is
// skipped without the aux DSN.
func TestDocumentNameLinks_CoreBothTiers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertNameLinkCore(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertNameLinkCore(t, d, ctx)
	})
}

func assertNameLinkCore(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()

	// A target document, named in the Path tree with a distinctive title.
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Alpha Target","path":"/nl/alpha"}`)
	if r.IsError {
		t.Fatalf("create target doc: %s", r.Text)
	}
	tgtRoot := out["root_chunk_id"].(string)

	// A source document to hang link-bearing chunks off.
	out, r = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Source","path":"/nl/source"}`)
	if r.IsError {
		t.Fatalf("create source doc: %s", r.Text)
	}
	srcDoc := out["document_id"].(string)
	srcRoot := out["root_chunk_id"].(string)

	// (1) A PATH link `[[/nl/alpha]]` → one references/auto=1 edge to the target's
	// ROOT chunk.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-path","body":"see [[/nl/alpha]] for context"}`)
	if r.IsError {
		t.Fatalf("create c-path: %s", r.Text)
	}
	cPath := out["id"].(string)
	cPathRev := int(out["revision"].(float64))
	if got := edgesFrom(t, d, ctx, cPath); len(got) != 1 || got[0].to != tgtRoot || got[0].kind != "references" || !got[0].auto {
		t.Fatalf("path-link edges = %+v, want one references/auto edge to %s", got, tgtRoot)
	}

	// (2) A TITLE link `[[Alpha Target]]` → resolves to the same document root.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-title","body":"also [[Alpha Target]] over here"}`)
	if r.IsError {
		t.Fatalf("create c-title: %s", r.Text)
	}
	cTitle := out["id"].(string)
	if got := edgesFrom(t, d, ctx, cTitle); len(got) != 1 || got[0].to != tgtRoot || !got[0].auto {
		t.Fatalf("title-link edges = %+v, want one auto edge to %s", got, tgtRoot)
	}

	// (3) An UNRESOLVED link `[[/nl/nope]]` → no edge (the link stays literal text).
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-nope","body":"dangling [[/nl/nope]]"}`)
	if r.IsError {
		t.Fatalf("create c-nope: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 0 {
		t.Errorf("unresolved link produced edges: %+v", got)
	}

	// (4) get_edges surfaces auto=true for the parser edge (the cTitle→target one).
	found := false
	for _, e := range getEdgesEdges(t, d, ctx, srcDoc) {
		if e["from_id"] == cTitle && e["to_id"] == tgtRoot {
			found = true
			if e["auto"] != true {
				t.Errorf("get_edges parser edge auto = %v, want true: %v", e["auto"], e)
			}
		}
	}
	if !found {
		t.Errorf("get_edges did not surface the cTitle→target parser edge")
	}

	// (5) Editing the body to REMOVE the `[[…]]` deletes the auto edge.
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cPath+`","revision":`+strconv.Itoa(cPathRev)+`,"body":"no links anymore"}`); r.IsError {
		t.Fatalf("update c-path to remove link: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, cPath); len(got) != 0 {
		t.Errorf("removing the link left edges behind: %+v", got)
	}

	// (6) A MANUAL link_chunks edge (auto=0) survives a body reconcile and is
	// neither duplicated nor flipped when the body links to the SAME target.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-manual","body":"plain"}`)
	if r.IsError {
		t.Fatalf("create c-manual: %s", r.Text)
	}
	cManual := out["id"].(string)
	cManualRev := int(out["revision"].(float64))
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+cManual+`","to_id":"`+tgtRoot+`","kind":"references"}`); r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, cManual); len(got) != 1 || got[0].auto {
		t.Fatalf("manual edge = %+v, want one auto=0 references edge", got)
	}
	// Body write whose link resolves to the SAME target: the manual edge stays put.
	out, r = docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cManual+`","revision":`+strconv.Itoa(cManualRev)+`,"body":"now with [[/nl/alpha]]"}`)
	if r.IsError {
		t.Fatalf("update c-manual body (same target): %s", r.Text)
	}
	cManualRev = int(out["revision"].(float64))
	if got := edgesFrom(t, d, ctx, cManual); len(got) != 1 || got[0].to != tgtRoot || got[0].auto {
		t.Errorf("manual edge not preserved after same-target reconcile: %+v (want one auto=0 edge to target)", got)
	}
	// Removing the body link sweeps only auto=1 — the manual edge survives.
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+cManual+`","revision":`+strconv.Itoa(cManualRev)+`,"body":"plain again"}`); r.IsError {
		t.Fatalf("update c-manual body (remove link): %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, cManual); len(got) != 1 || got[0].auto {
		t.Errorf("manual edge swept by the auto=1 reconcile: %+v (want it to survive)", got)
	}
}

// --- sqlite-only behavioural tests (tier-independent parser / gate / id logic) ---

// TestDocumentNameLinks_EdgeAutoMigration pins migrateEdgeAuto: a scope
// provisioned BEFORE this change (chunk_edges without the `auto` column) gets the
// column on the next ensureSchema, and every pre-existing edge reads back as
// manual (auto=0) — so an edge that predates the parser is never mistaken for a
// parser-generated one (which reconcile would otherwise sweep). Fail-before:
// without the migration the `SELECT auto` errors (no such column).
func TestDocumentNameLinks_EdgeAutoMigration(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	d := &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", TenantID: "tnt"})
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	now := time.Now().UnixNano()

	// Provision the OLD pre-RFC-BS-2a chunk_edges shape (no `auto` column) with a
	// pre-existing edge, before any ensureSchema runs.
	if err := d.exec(ctx, key, `CREATE TABLE chunk_edges (from_id TEXT NOT NULL, to_id TEXT NOT NULL, kind TEXT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY (from_id, to_id, kind))`); err != nil {
		t.Fatalf("old chunk_edges DDL: %v", err)
	}
	if err := d.exec(ctx, key, `INSERT INTO chunk_edges (from_id, to_id, kind, created_at) VALUES (?, ?, ?, ?)`, "a", "b", "references", now); err != nil {
		t.Fatalf("insert pre-existing edge: %v", err)
	}

	// ensureSchema runs migrateEdgeAuto: add the column + default existing rows to 0.
	if err := d.ensureSchema(ctx, key); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	res, err := d.query(ctx, key, `SELECT auto FROM chunk_edges WHERE from_id = ? AND to_id = ? AND kind = ?`, "a", "b", "references")
	if err != nil {
		t.Fatalf("read back auto: %v", err)
	}
	if len(res.Rows) != 1 || asInt(res.Rows[0][0]) != 0 {
		t.Errorf("pre-existing edge auto = %v, want 0 (manual)", res.Rows)
	}
}

// TestDocumentNameLinks_EmbedIsNotALink pins that `![[…]]` (a transclusion embed,
// a separate later step) is NOT a link and materializes no edge. Fail-before:
// dropping the leading-`!` check in parseNameLinks makes the embed create an edge.
func TestDocumentNameLinks_EmbedIsNotALink(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"T","path":"/nl/t"}`)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"S"}`)
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)

	out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c","body":"embedded ![[/nl/t]] here"}`)
	if r.IsError {
		t.Fatalf("create chunk: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 0 {
		t.Errorf("embed ![[…]] produced a link edge: %+v", got)
	}
}

// TestDocumentNameLinks_SelfLinkDropped pins that a chunk linking to its own
// title produces no self-edge (the chunk row exists when reconcile runs, so the
// title resolves to the chunk itself and is dropped). Fail-before: removing the
// `to == fromChunkID` self-check creates a self-referential edge.
func TestDocumentNameLinks_SelfLinkDropped(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"S"}`)
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)

	out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"Selfie","body":"I am [[Selfie]]"}`)
	if r.IsError {
		t.Fatalf("create self-linking chunk: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 0 {
		t.Errorf("self link produced a self-edge: %+v", got)
	}
}

// TestDocumentNameLinks_AliasAndDedup pins two parser details:
//   - an aliased `[[target|display]]` link resolves by the target BEFORE the
//     pipe (fail-before: without the strip, the `|`/spaces fail path validation
//     so the alias-only chunk resolves to nothing);
//   - the same target named twice yields exactly one edge (idempotent; the
//     chunk_edges primary key + the NOT EXISTS guard together prevent a second
//     row, so this is a correctness check rather than an isolated fail-before).
func TestDocumentNameLinks_AliasAndDedup(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"T","path":"/nl/t"}`)
	tgtRoot := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"S"}`)
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)

	// Alias-only chunk: isolates the `|display` strip — no bare occurrence to mask it.
	out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-alias","body":"[[/nl/t|see the target]]"}`)
	if r.IsError {
		t.Fatalf("create c-alias: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 1 || got[0].to != tgtRoot || !got[0].auto {
		t.Errorf("aliased link = %+v, want one auto edge to %s", got, tgtRoot)
	}

	// The same target named twice → exactly one edge.
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-dup","body":"[[/nl/t]] and again [[/nl/t]]"}`)
	if r.IsError {
		t.Fatalf("create c-dup: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 1 || got[0].to != tgtRoot {
		t.Errorf("duplicated link = %+v, want a single edge to %s", got, tgtRoot)
	}
}

// TestDocumentNameLinks_EdgeSurvivesTargetRename pins that a materialized edge is
// id-bound: moving the target's Path dirent does not break an existing edge (the
// edge holds the resolved chunk id, not a path), while the resolver reads the
// CURRENT tree for new writes (old path stops resolving, new path resolves).
func TestDocumentNameLinks_EdgeSurvivesTargetRename(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Renamed Target","path":"/nl/orig"}`)
	tgtRoot, tgtDoc := out["root_chunk_id"].(string), out["document_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"S"}`)
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)

	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c","body":"[[/nl/orig]]"}`)
	c := out["id"].(string)
	if got := edgesFrom(t, d, ctx, c); len(got) != 1 || got[0].to != tgtRoot {
		t.Fatalf("precondition edge = %+v, want one to %s", got, tgtRoot)
	}

	// Move the target's dirent /nl/orig → /nl/moved (same document id).
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if _, err := d.Store.DirentDelete(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), "/nl/", "orig"); err != nil {
		t.Fatalf("dirent delete: %v", err)
	}
	if _, err := d.registerDocDirent(ctx, key, tgtDoc, "/nl/moved"); err != nil {
		t.Fatalf("register moved dirent: %v", err)
	}

	// The already-materialized edge is untouched by the move.
	if got := edgesFrom(t, d, ctx, c); len(got) != 1 || got[0].to != tgtRoot {
		t.Errorf("edge did not survive the dirent move: %+v", got)
	}
	// A NEW write to the OLD path no longer resolves.
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-old","body":"[[/nl/orig]]"}`)
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 0 {
		t.Errorf("stale path resolved after move: %+v", got)
	}
	// A NEW write to the NEW path resolves to the same target root.
	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c-new","body":"[[/nl/moved]]"}`)
	if got := edgesFrom(t, d, ctx, out["id"].(string)); len(got) != 1 || got[0].to != tgtRoot {
		t.Errorf("moved path did not resolve after rename: %+v", got)
	}
}

// TestDocumentNameLinks_BodylessUpdateLeavesEdges pins that an update_chunk that
// does not write the body leaves name-link edges alone — in BOTH forms: a
// tags-only update (never enters the body/fields block) and a fields-only update
// (enters the block but preserves the body verbatim). Removing the target's
// dirent first makes a wrongly-triggered reconcile observable: it would re-derive
// the preserved-but-now-unresolvable body and delete a still-valid edge.
// Fail-before: dropping the inner `hasBody` gate makes the fields-only update
// reconcile and drop the edge.
func TestDocumentNameLinks_BodylessUpdateLeavesEdges(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"T","path":"/nl/t"}`)
	tgtRoot := out["root_chunk_id"].(string)
	out, _ = docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"S"}`)
	srcDoc, srcRoot := out["document_id"].(string), out["root_chunk_id"].(string)

	out, _ = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+srcDoc+`","parent_id":"`+srcRoot+`","title":"c","body":"[[/nl/t]]"}`)
	c := out["id"].(string)
	rev := int(out["revision"].(float64))
	if got := edgesFrom(t, d, ctx, c); len(got) != 1 {
		t.Fatalf("precondition edge = %+v, want one", got)
	}

	// Remove the target dirent so the STORED body would no longer resolve IF
	// reconcile were to run.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if _, err := d.Store.DirentDelete(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), "/nl/", "t"); err != nil {
		t.Fatalf("dirent delete: %v", err)
	}

	// (a) A tags-only update never enters the body/fields block → edges untouched.
	out, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c+`","revision":`+strconv.Itoa(rev)+`,"tags":["x"]}`)
	if r.IsError {
		t.Fatalf("tags-only update: %s", r.Text)
	}
	rev = int(out["revision"].(float64))
	if got := edgesFrom(t, d, ctx, c); len(got) != 1 || got[0].to != tgtRoot {
		t.Errorf("tags-only update disturbed name-link edges: %+v", got)
	}

	// (b) A fields-only update enters the block but preserves the body → the inner
	// `hasBody` gate must keep reconcile from running.
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+c+`","revision":`+strconv.Itoa(rev)+`,"fields":{"color":"blue"}}`); r.IsError {
		t.Fatalf("fields-only update: %s", r.Text)
	}
	if got := edgesFrom(t, d, ctx, c); len(got) != 1 || got[0].to != tgtRoot {
		t.Errorf("fields-only update disturbed name-link edges: %+v", got)
	}
}
