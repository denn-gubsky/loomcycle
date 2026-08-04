package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// JSON Canvas import/export (RFC BS Phase 4).
//
// export_canvas renders a document's CONTENT chunks (never the root container) as
// JSON Canvas v1.0 "text" nodes + their within-document cross-reference edges;
// import_canvas builds a new document from a canvas. These pin: the export→import
// →export round-trip (same node texts + layouts, same edge multiset), spec
// conformance (integer coords, type "text", toEnd "arrow"), a hand-written
// Obsidian-style canvas import (bodies == node texts, layout rows == node coords,
// label → edge kind), and auto-grid determinism (an un-placed chunk exports to a
// deterministic grid slot that survives a round-trip).
//
// Every case touches SQL Memory, so all run on BOTH tiers via historyBothTiers
// (sqlite + postgres, the pg half skipped without the aux DSN). The top-level
// name carries the TestDocumentCanvas_ prefix the CI postgres-tier -run filter
// keys on — a postgres-tier test absent from that filter never runs in CI.

func TestDocumentCanvas_CoreBothTiers(t *testing.T) {
	historyBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		t.Run("round_trip", func(t *testing.T) { assertCanvasRoundTrip(t, d, ctx) })
		t.Run("spec_conformance", func(t *testing.T) { assertCanvasSpecConformance(t, d, ctx) })
		t.Run("obsidian_import", func(t *testing.T) { assertCanvasObsidianImport(t, d, ctx) })
		t.Run("auto_grid", func(t *testing.T) { assertCanvasAutoGrid(t, d, ctx) })
		t.Run("delete_cascade", func(t *testing.T) { assertCanvasDeleteCascade(t, d, ctx) })
	})
}

// --- op wrappers (user scope) ---

func canvasCreateDoc(t *testing.T, d *Document, ctx context.Context, title string) (docID, rootID string) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q}`, title))
	if r.IsError {
		t.Fatalf("create_document(%s): %s", title, r.Text)
	}
	return out["document_id"].(string), out["root_chunk_id"].(string)
}

func canvasCreateChunk(t *testing.T, d *Document, ctx context.Context, docID, rootID, title, body string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":%q,"body":%q}`,
		docID, rootID, title, body))
	if r.IsError {
		t.Fatalf("create_chunk(%s): %s", title, r.Text)
	}
	return out["id"].(string)
}

func canvasLink(t *testing.T, d *Document, ctx context.Context, from, to, kind string) {
	t.Helper()
	_, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":%q}`, from, to, kind))
	if r.IsError {
		t.Fatalf("link_chunks(%s->%s): %s", from, to, r.Text)
	}
}

// canvasStoreLayout writes a chunk_layout row directly (there is no dedicated op
// — a layout is authored on the import side or by a spatial UI). It resolves the
// same scope key the tool ops use, so the row lands where export_canvas reads.
func canvasStoreLayout(t *testing.T, d *Document, ctx context.Context, chunkID string, x, y, w, h int, color string) {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.exec(ctx, key,
		`INSERT INTO chunk_layout (chunk_id, x, y, width, height, color) VALUES (?, ?, ?, ?, ?, ?)`,
		chunkID, x, y, w, h, nullIfEmpty(color)); err != nil {
		t.Fatalf("store layout: %v", err)
	}
}

func canvasExport(t *testing.T, d *Document, ctx context.Context, docID string) canvasDoc {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_canvas","scope":"user","document_id":%q}`, docID))
	if r.IsError {
		t.Fatalf("export_canvas(%s): %s", docID, r.Text)
	}
	raw, _ := json.Marshal(out["canvas"])
	var cv canvasDoc
	if err := json.Unmarshal(raw, &cv); err != nil {
		t.Fatalf("decode canvas: %v (raw=%s)", err, raw)
	}
	return cv
}

func canvasImport(t *testing.T, d *Document, ctx context.Context, cv canvasDoc, title string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(cv)
	req, _ := json.Marshal(map[string]any{"op": "import_canvas", "scope": "user", "title": title, "canvas": json.RawMessage(body)})
	out, r := docExec(t, d, ctx, string(req))
	if r.IsError {
		t.Fatalf("import_canvas(%s): %s", title, r.Text)
	}
	return out
}

// --- comparators ---

// nodeLayoutByText keys a canvas's nodes by their text → (x,y,width,height). The
// test bodies are all distinct, so text is a stable identity across a round-trip
// (ids and the skipped root differ, coords must not).
func nodeLayoutByText(cv canvasDoc) map[string][4]int {
	m := map[string][4]int{}
	for _, n := range cv.Nodes {
		m[n.Text] = [4]int{n.X, n.Y, n.Width, n.Height}
	}
	return m
}

// edgeKeysByText renders each edge as "label|fromText->toText" (sorted) so two
// canvases compare as an edge MULTISET independent of node ids.
func edgeKeysByText(cv canvasDoc) []string {
	idText := map[string]string{}
	for _, n := range cv.Nodes {
		idText[n.ID] = n.Text
	}
	keys := make([]string, 0, len(cv.Edges))
	for _, e := range cv.Edges {
		keys = append(keys, e.Label+"|"+idText[e.FromNode]+"->"+idText[e.ToNode])
	}
	sort.Strings(keys)
	return keys
}

// --- cases ---

// assertCanvasRoundTrip: a document (root + 3 content chunks with distinct
// bodies, one carrying a stored layout, + 2 edges among them) → export C1 →
// import → export C2, with C1 and C2 equal by node-text→layout and edge multiset.
// The root is skipped both ways, so counts stay 3 nodes / 2 edges each direction.
func assertCanvasRoundTrip(t *testing.T, d *Document, ctx context.Context) {
	docID, rootID := canvasCreateDoc(t, d, ctx, "RT source")
	c1 := canvasCreateChunk(t, d, ctx, docID, rootID, "C1", "body one")
	c2 := canvasCreateChunk(t, d, ctx, docID, rootID, "C2", "body two")
	c3 := canvasCreateChunk(t, d, ctx, docID, rootID, "C3", "body three")
	// A stored layout on c2, with coordinates no auto-grid slot would produce, so
	// "the stored row was used" is unambiguous. Includes a color.
	canvasStoreLayout(t, d, ctx, c2, 1234, 5678, 321, 222, "#ff0000")
	canvasLink(t, d, ctx, c1, c2, "supports")
	canvasLink(t, d, ctx, c2, c3, "refines")

	cv1 := canvasExport(t, d, ctx, docID)
	if len(cv1.Nodes) != 3 {
		t.Fatalf("C1 nodes = %d, want 3 (root must be skipped): %+v", len(cv1.Nodes), cv1.Nodes)
	}
	if len(cv1.Edges) != 2 {
		t.Fatalf("C1 edges = %d, want 2: %+v", len(cv1.Edges), cv1.Edges)
	}
	// The stored layout is what got exported for c2's body.
	if got := nodeLayoutByText(cv1)["body two"]; got != [4]int{1234, 5678, 321, 222} {
		t.Errorf("stored layout not exported: got %v, want [1234 5678 321 222]", got)
	}

	imp := canvasImport(t, d, ctx, cv1, "RT imported")
	if int(imp["chunks_created"].(float64)) != 3 {
		t.Errorf("chunks_created = %v, want 3", imp["chunks_created"])
	}
	if int(imp["edges_created"].(float64)) != 2 {
		t.Errorf("edges_created = %v, want 2", imp["edges_created"])
	}
	newDoc := imp["document_id"].(string)
	if newDoc == "" || newDoc == docID {
		t.Fatalf("import made no fresh document: %v", imp["document_id"])
	}

	cv2 := canvasExport(t, d, ctx, newDoc)
	if len(cv2.Nodes) != 3 || len(cv2.Edges) != 2 {
		t.Fatalf("C2 = %d nodes / %d edges, want 3 / 2", len(cv2.Nodes), len(cv2.Edges))
	}
	if !reflect.DeepEqual(nodeLayoutByText(cv1), nodeLayoutByText(cv2)) {
		t.Errorf("node text→layout differs across round-trip:\n C1=%v\n C2=%v", nodeLayoutByText(cv1), nodeLayoutByText(cv2))
	}
	if !reflect.DeepEqual(edgeKeysByText(cv1), edgeKeysByText(cv2)) {
		t.Errorf("edge multiset differs across round-trip:\n C1=%v\n C2=%v", edgeKeysByText(cv1), edgeKeysByText(cv2))
	}
	// The stored layout + color survives the full round-trip (color is carried
	// even though the set comparison above only pins x/y/w/h).
	for _, n := range cv2.Nodes {
		if n.Text == "body two" {
			if n.X != 1234 || n.Y != 5678 || n.Width != 321 || n.Height != 222 || n.Color != "#ff0000" {
				t.Errorf("stored layout+color did not round-trip: %+v", n)
			}
		}
	}
}

// assertCanvasSpecConformance decodes the raw export and pins the JSON Canvas
// v1.0 wire contract: nodes are type "text" with INTEGER x/y/width/height, edges
// carry fromNode/toNode and toEnd "arrow". Coordinates are checked as literal
// json.Number strings so a float like "400.0" (which would decode fine into an
// int field) is caught.
func assertCanvasSpecConformance(t *testing.T, d *Document, ctx context.Context) {
	docID, rootID := canvasCreateDoc(t, d, ctx, "spec")
	a := canvasCreateChunk(t, d, ctx, docID, rootID, "A", "alpha")
	b := canvasCreateChunk(t, d, ctx, docID, rootID, "B", "beta")
	canvasLink(t, d, ctx, a, b, "cites")

	_, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_canvas","scope":"user","document_id":%q}`, docID))
	if res.IsError {
		t.Fatalf("export_canvas: %s", res.Text)
	}
	var parsed struct {
		Canvas struct {
			Nodes []struct {
				ID     string      `json:"id"`
				Type   string      `json:"type"`
				X      json.Number `json:"x"`
				Y      json.Number `json:"y"`
				Width  json.Number `json:"width"`
				Height json.Number `json:"height"`
			} `json:"nodes"`
			Edges []struct {
				ID       string `json:"id"`
				FromNode string `json:"fromNode"`
				ToNode   string `json:"toNode"`
				ToEnd    string `json:"toEnd"`
			} `json:"edges"`
		} `json:"canvas"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		t.Fatalf("unmarshal export: %v (text=%s)", err, res.Text)
	}
	if len(parsed.Canvas.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(parsed.Canvas.Nodes))
	}
	for _, n := range parsed.Canvas.Nodes {
		if n.Type != "text" {
			t.Errorf("node %s type = %q, want text", n.ID, n.Type)
		}
		for name, num := range map[string]json.Number{"x": n.X, "y": n.Y, "width": n.Width, "height": n.Height} {
			s := num.String()
			if s == "" || strings.ContainsAny(s, ".eE") {
				t.Errorf("node %s %s = %q, want an integer literal", n.ID, name, s)
			}
		}
	}
	if len(parsed.Canvas.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(parsed.Canvas.Edges))
	}
	if e := parsed.Canvas.Edges[0]; e.FromNode == "" || e.ToNode == "" || e.ToEnd != "arrow" {
		t.Errorf("edge = %+v, want non-empty fromNode/toNode and toEnd=arrow", e)
	}
}

// assertCanvasObsidianImport imports a hand-written JSON Canvas (two text nodes
// at real coordinates + one labelled edge) and verifies via re-export that the
// bodies == the node texts, the layout rows == the node coordinates, and the one
// edge carries the label as its kind.
func assertCanvasObsidianImport(t *testing.T, d *Document, ctx context.Context) {
	canvasJSON := `{
	  "nodes": [
	    {"id":"n1","type":"text","text":"First note","x":100,"y":200,"width":300,"height":150},
	    {"id":"n2","type":"text","text":"Second note","x":500,"y":220,"width":260,"height":180}
	  ],
	  "edges": [
	    {"id":"e1","fromNode":"n1","toNode":"n2","label":"leads to"}
	  ]
	}`
	req, _ := json.Marshal(map[string]any{"op": "import_canvas", "scope": "user", "title": "Obsidian", "canvas": json.RawMessage(canvasJSON)})
	out, r := docExec(t, d, ctx, string(req))
	if r.IsError {
		t.Fatalf("import_canvas: %s", r.Text)
	}
	if int(out["chunks_created"].(float64)) != 2 {
		t.Errorf("chunks_created = %v, want 2", out["chunks_created"])
	}
	if int(out["edges_created"].(float64)) != 1 {
		t.Errorf("edges_created = %v, want 1", out["edges_created"])
	}
	docID := out["document_id"].(string)

	cv := canvasExport(t, d, ctx, docID)
	if len(cv.Nodes) != 2 {
		t.Fatalf("re-export nodes = %d, want 2: %+v", len(cv.Nodes), cv.Nodes)
	}
	byText := map[string]canvasNode{}
	for _, n := range cv.Nodes {
		byText[n.Text] = n
	}
	n1, ok := byText["First note"]
	if !ok {
		t.Fatalf("body != node text: 'First note' missing from %+v", cv.Nodes)
	}
	if n1.X != 100 || n1.Y != 200 || n1.Width != 300 || n1.Height != 150 {
		t.Errorf("n1 layout = (%d,%d %dx%d), want (100,200 300x150)", n1.X, n1.Y, n1.Width, n1.Height)
	}
	n2, ok := byText["Second note"]
	if !ok {
		t.Fatalf("body != node text: 'Second note' missing from %+v", cv.Nodes)
	}
	if n2.X != 500 || n2.Y != 220 || n2.Width != 260 || n2.Height != 180 {
		t.Errorf("n2 layout = (%d,%d %dx%d), want (500,220 260x180)", n2.X, n2.Y, n2.Width, n2.Height)
	}
	if len(cv.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(cv.Edges))
	}
	if cv.Edges[0].Label != "leads to" {
		t.Errorf("edge kind (label) = %q, want 'leads to'", cv.Edges[0].Label)
	}
}

// assertCanvasAutoGrid: content chunks with NO stored layout export to a
// deterministic row-major grid (4 cols, 400x200 + 50 gap) by export order, and
// those coordinates survive an import→re-export unchanged.
func assertCanvasAutoGrid(t *testing.T, d *Document, ctx context.Context) {
	docID, rootID := canvasCreateDoc(t, d, ctx, "grid")
	const n = 6
	for i := 0; i < n; i++ {
		canvasCreateChunk(t, d, ctx, docID, rootID, fmt.Sprintf("g%d", i), fmt.Sprintf("grid body %d", i))
	}
	cv := canvasExport(t, d, ctx, docID)
	if len(cv.Nodes) != n {
		t.Fatalf("nodes = %d, want %d", len(cv.Nodes), n)
	}
	// The node whose body is "grid body i" must sit at the grid slot for index i
	// (which also proves export order == creation/position order).
	for i := 0; i < n; i++ {
		wantX := (i % canvasGridCols) * (canvasNodeW + canvasNodeGap)
		wantY := (i / canvasGridCols) * (canvasNodeH + canvasNodeGap)
		got := nodeLayoutByText(cv)[fmt.Sprintf("grid body %d", i)]
		if got != [4]int{wantX, wantY, canvasNodeW, canvasNodeH} {
			t.Errorf("auto-grid node %d = %v, want [%d %d %d %d]", i, got, wantX, wantY, canvasNodeW, canvasNodeH)
		}
	}
	// Import stores every exported coordinate, so a re-export reads them back
	// identically — the auto-grid layout is stable through a round-trip.
	imp := canvasImport(t, d, ctx, cv, "grid imported")
	cv2 := canvasExport(t, d, ctx, imp["document_id"].(string))
	if !reflect.DeepEqual(nodeLayoutByText(cv), nodeLayoutByText(cv2)) {
		t.Errorf("auto-grid layout did not survive round-trip:\n c1=%v\n c2=%v", nodeLayoutByText(cv), nodeLayoutByText(cv2))
	}
}

// countLayout counts chunk_layout rows for a literal set of chunk ids — literal,
// NOT via a `document_id` subquery, so it still finds ORPHAN rows after the
// chunks themselves are deleted (the whole point of a cascade test).
func countLayout(t *testing.T, d *Document, ctx context.Context, ids []string) int {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	ph, args := inPlaceholders(ids)
	res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_layout WHERE chunk_id IN (`+ph+`)`, args...)
	if err != nil {
		t.Fatalf("count layout: %v", err)
	}
	if len(res.Rows) == 0 {
		return 0
	}
	return asInt(res.Rows[0][0])
}

// assertCanvasDeleteCascade pins BOTH cascade sites: import_canvas writes a
// chunk_layout row per node, delete_chunk drops the row for the deleted chunk
// (leaving its sibling's), and delete_document drops the rest. Queried by literal
// chunk id so a MISSING cascade shows up as an orphaned row rather than passing
// vacuously once the chunks are gone.
func assertCanvasDeleteCascade(t *testing.T, d *Document, ctx context.Context) {
	canvasJSON := `{
	  "nodes": [
	    {"id":"a","type":"text","text":"cascade a","x":10,"y":20,"width":30,"height":40},
	    {"id":"b","type":"text","text":"cascade b","x":50,"y":60,"width":70,"height":80}
	  ],
	  "edges": []
	}`
	req, _ := json.Marshal(map[string]any{"op": "import_canvas", "scope": "user", "title": "cascade", "canvas": json.RawMessage(canvasJSON)})
	out, r := docExec(t, d, ctx, string(req))
	if r.IsError {
		t.Fatalf("import_canvas: %s", r.Text)
	}
	docID := out["document_id"].(string)

	// The content chunks (children of the root) carry the layout rows.
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	cres, err := d.query(ctx, key, `SELECT id FROM chunks WHERE document_id = ? AND parent_id IS NOT NULL`, docID)
	if err != nil {
		t.Fatalf("query content chunks: %v", err)
	}
	var ids []string
	for _, row := range cres.Rows {
		ids = append(ids, asStr(row[0]))
	}
	if len(ids) != 2 {
		t.Fatalf("content chunks = %d, want 2", len(ids))
	}
	if n := countLayout(t, d, ctx, ids); n != 2 {
		t.Fatalf("layout rows after import = %d, want 2", n)
	}

	// delete_chunk one leaf → only its layout row goes.
	if _, dr := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_chunk","scope":"user","id":%q}`, ids[0])); dr.IsError {
		t.Fatalf("delete_chunk: %s", dr.Text)
	}
	if n := countLayout(t, d, ctx, ids[:1]); n != 0 {
		t.Errorf("orphan layout row for deleted chunk = %d, want 0", n)
	}
	if n := countLayout(t, d, ctx, ids[1:]); n != 1 {
		t.Errorf("sibling layout row = %d, want 1 (a sibling delete must not touch it)", n)
	}

	// delete_document → the remaining layout row goes too.
	if _, dr := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_document","scope":"user","id":%q}`, docID)); dr.IsError {
		t.Fatalf("delete_document: %s", dr.Text)
	}
	if n := countLayout(t, d, ctx, ids); n != 0 {
		t.Errorf("orphan layout rows after delete_document = %d, want 0", n)
	}
}
