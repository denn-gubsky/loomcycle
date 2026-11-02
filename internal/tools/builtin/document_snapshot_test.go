package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// query_chunks rows carry the chunk's updated_at, so a client holding a snapshot
// can tell which chunks changed since; an edit moves it forward.
func TestQueryChunks_RowsCarryUpdatedAt(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Doc"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := out["document_id"].(string)
	out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","title":"Part","body":"one"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	chunkID := out["id"].(string)

	updatedAt := func() float64 {
		t.Helper()
		out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","document_id":"`+docID+`"}`)
		if r.IsError {
			t.Fatalf("query_chunks: %s", r.Text)
		}
		var got float64
		for _, c := range out["chunks"].([]any) {
			m := c.(map[string]any)
			v, ok := m["updated_at"].(float64)
			if !ok || v <= 0 {
				t.Fatalf("query_chunks row has no positive updated_at: %v", m)
			}
			if m["id"] == chunkID {
				got = v
			}
		}
		if got == 0 {
			t.Fatalf("query_chunks did not list chunk %s", chunkID)
		}
		return got
	}
	before := updatedAt()
	time.Sleep(2 * time.Millisecond)
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+chunkID+`","body":"two","revision":1}`); r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	if after := updatedAt(); after <= before {
		t.Errorf("updated_at after an edit = %v, want later than %v", after, before)
	}
}

// snapResp is graph_snapshot's answer.
type snapResp struct {
	Page struct {
		Documents  int    `json:"documents"`
		NextCursor string `json:"next_cursor"`
		ClampedTo  int    `json:"clamped_to"`
	} `json:"page"`
	Codes struct {
		Types       []string `json:"types"`
		Statuses    []string `json:"statuses"`
		Kinds       []string `json:"kinds"`
		FactClasses []string `json:"fact_classes"`
		Methods     []string `json:"methods"`
	} `json:"codes"`
	Documents *struct {
		IDs          []string   `json:"ids"`
		Titles       []string   `json:"titles"`
		RootChunkIDs []string   `json:"root_chunk_ids"`
		Paths        [][]string `json:"paths"`
	} `json:"documents"`
	Chunks *struct {
		IDs            []string   `json:"ids"`
		DocumentIdx    []int      `json:"document_idx"`
		ParentIdx      []int      `json:"parent_idx"`
		ValidAt        []*int64   `json:"valid_at"`
		IsFact         []bool     `json:"is_fact"`
		FactConfidence []*float64 `json:"fact_confidence"`
	} `json:"chunks"`
	Edges *struct {
		IDs      []string `json:"ids"`
		FromIdx  []int    `json:"from_idx"`
		ToIdx    []int    `json:"to_idx"`
		External []struct {
			Idx        int    `json:"idx"`
			ID         string `json:"id"`
			DocumentID string `json:"document_id"`
		} `json:"external"`
		Truncated bool `json:"truncated"`
	} `json:"edges"`
	Dirents *struct {
		Paths       []string `json:"paths"`
		Kinds       []string `json:"kinds"`
		DocumentIdx []int    `json:"document_idx"`
		DocumentIDs []string `json:"document_ids"`
	} `json:"dirents"`
}

// endID is the chunk id an edge end names: a chunk on the page or an external.
func (s snapResp) endID(t *testing.T, idx int) string {
	t.Helper()
	if idx >= 0 {
		return s.Chunks.IDs[idx]
	}
	for _, e := range s.Edges.External {
		if e.Idx == idx {
			return e.ID
		}
	}
	t.Fatalf("edge end %d names no external entry", idx)
	return ""
}

func graphSnap(t *testing.T, d *Document, ctx context.Context, body string) (snapResp, string) {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var s snapResp
	if err := json.Unmarshal([]byte(res.Text), &s); err != nil {
		t.Fatalf("decode %s: %v", res.Text, err)
	}
	return s, res.Text
}

// snapshotAll pages body (a JSON object without its closing brace) to the end.
func snapshotAll(t *testing.T, d *Document, ctx context.Context, body string) ([]snapResp, []string) {
	t.Helper()
	var pages []snapResp
	var texts []string
	cursor := ""
	for range 10000 {
		b := body + "}"
		if cursor != "" {
			b = body + `,"cursor":"` + cursor + `"}`
		}
		s, text := graphSnap(t, d, ctx, b)
		pages, texts = append(pages, s), append(texts, text)
		if s.Page.NextCursor == "" {
			return pages, texts
		}
		cursor = s.Page.NextCursor
	}
	t.Fatal("graph_snapshot never ran out of pages")
	return nil, nil
}

// snapDoc files a document at path with one chunk under its root.
func snapDoc(t *testing.T, d *Document, ctx context.Context, scope, path, title string) (doc, root, part string) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":%q,"title":%q,"path":%q}`, scope, title, path))
	if r.IsError {
		t.Fatalf("create_document %s: %s", path, r.Text)
	}
	doc, root = out["document_id"].(string), out["root_chunk_id"].(string)
	out, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":%q,"document_id":%q,"title":"Part","body":"text"}`, scope, doc))
	if r.IsError {
		t.Fatalf("create_chunk %s: %s", path, r.Text)
	}
	return doc, root, out["id"].(string)
}

func snapChunk(t *testing.T, d *Document, ctx context.Context, scope, doc, parent, title string) string {
	t.Helper()
	body := fmt.Sprintf(`{"op":"create_chunk","scope":%q,"document_id":%q,"title":%q,"body":"text"`, scope, doc, title)
	if parent != "" {
		body += fmt.Sprintf(`,"parent_id":%q`, parent)
	}
	out, r := docExec(t, d, ctx, body+"}")
	if r.IsError {
		t.Fatalf("create_chunk %s: %s", title, r.Text)
	}
	return out["id"].(string)
}

func snapLink(t *testing.T, d *Document, ctx context.Context, scope, from, to string) {
	t.Helper()
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":%q,"from_id":%q,"to_id":%q,"kind":"cites"}`, scope, from, to)); r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}
}

// A member who may read two of five documents gets exactly those two, their
// chunks and the relations among them — the relations into the other three are
// dropped — and every page, cursor included, is byte for byte what it gets once
// the other three no longer exist.
func TestGraphSnapshot_MemberSeesOnlyReadableDocuments(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		docs, parts := make([]string, 5), make([]string, 5)
		for i := range docs {
			docs[i], _, parts[i] = snapDoc(t, d, open, "tenant", fmt.Sprintf("/kb/d%d", i+1), fmt.Sprintf("Doc %d", i+1))
		}
		for _, l := range [][2]int{{0, 1}, {0, 2}, {1, 0}, {3, 4}, {2, 0}} {
			snapLink(t, d, open, "tenant", parts[l[0]], parts[l[1]])
		}
		reader := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/d2", "document:tenant:/kb/d4", "document:tenant:/kb/d5"}}))
		readable := []string{docs[0], docs[2]}
		sort.Strings(readable)
		hidden := []string{docs[1], docs[3], docs[4]}
		wantEdges := []string{parts[0] + ">" + parts[2], parts[2] + ">" + parts[0]}
		sort.Strings(wantEdges)

		withHidden := map[int][]string{}
		for _, size := range []int{1, 2, 200} {
			pages, texts := snapshotAll(t, d, reader, fmt.Sprintf(`{"op":"graph_snapshot","scope":"tenant","page_documents":%d`, size))
			withHidden[size] = texts
			var seen []string
			edges := map[string]bool{}
			for i, p := range pages {
				seen = append(seen, p.Documents.IDs...)
				for j := range p.Chunks.IDs {
					if doc := p.Documents.IDs[p.Chunks.DocumentIdx[j]]; !slices.Contains(readable, doc) {
						t.Errorf("page_documents=%d: chunk of unreadable document %s", size, doc)
					}
				}
				for j := range p.Edges.IDs {
					edges[p.endID(t, p.Edges.FromIdx[j])+">"+p.endID(t, p.Edges.ToIdx[j])] = true
				}
				for _, id := range append(append([]string{}, hidden...), parts[1], parts[3], parts[4]) {
					if strings.Contains(texts[i], id) {
						t.Errorf("page_documents=%d: page %d names hidden %s: %s", size, i, id, texts[i])
					}
				}
			}
			if strings.Join(seen, ",") != strings.Join(readable, ",") {
				t.Errorf("page_documents=%d: documents %v, want %v", size, seen, readable)
			}
			if want := (len(readable) + size - 1) / size; len(pages) != want {
				t.Errorf("page_documents=%d: %d pages, want %d", size, len(pages), want)
			}
			got := make([]string, 0, len(edges))
			for e := range edges {
				got = append(got, e)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(wantEdges, ",") {
				t.Errorf("page_documents=%d: edges %v, want %v", size, got, wantEdges)
			}
			if p := pages[0].Dirents; p == nil || strings.Join(p.Paths, ",") != "/kb,/kb/d1,/kb/d3" {
				t.Errorf("page_documents=%d: dirents = %+v, want /kb and the two readable names", size, p)
			}
		}

		deleteDocs(t, d, open, hidden)
		for _, size := range []int{1, 2, 200} {
			_, texts := snapshotAll(t, d, reader, fmt.Sprintf(`{"op":"graph_snapshot","scope":"tenant","page_documents":%d`, size))
			if strings.Join(texts, "\n") != strings.Join(withHidden[size], "\n") {
				t.Errorf("page_documents=%d: hidden documents change the answer:\nwith    %v\nwithout %v", size, withHidden[size], texts)
			}
		}
	})
}

// direntCalls counts the Path-tree reads a call makes: a whole-tree listing
// (DirentListUnder) against reverse lookups by document.
type direntCalls struct {
	store.Store
	under, byDocs int
}

func (c *direntCalls) DirentListUnder(ctx context.Context, tenantID, scope, scopeID, prefix string) ([]store.DirentRow, error) {
	c.under++
	return c.Store.DirentListUnder(ctx, tenantID, scope, scopeID, prefix)
}

func (c *direntCalls) DirentListByDocuments(ctx context.Context, tenantID, scope, scopeID string, ids []string) ([]store.DirentRow, error) {
	c.byDocs++
	return c.Store.DirentListByDocuments(ctx, tenantID, scope, scopeID, ids)
}

// A page costs its own documents' names, not the scope's Path tree: under
// path-scoped access rules (which make every readability answer depend on a
// document's names) only the first page lists the tree, for its dirents
// section; every page after reads names through the reverse index alone.
func TestGraphSnapshot_PagesAskOnlyTheirOwnNames(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		var docs []string
		for i := range 6 {
			doc, _, _ := snapDoc(t, d, open, "tenant", fmt.Sprintf("/kb/d%d", i+1), fmt.Sprintf("Doc %d", i+1))
			docs = append(docs, doc)
		}
		reader := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/d2"}}))
		counted := &direntCalls{Store: d.Store}
		d.Store = counted
		defer func() { d.Store = counted.Store }()

		first, _ := graphSnap(t, d, reader, `{"op":"graph_snapshot","scope":"tenant","page_documents":2}`)
		if counted.under != 1 {
			t.Errorf("first page listed the tree %d times, want once (its dirents section)", counted.under)
		}
		cursor, pages := first.Page.NextCursor, 1
		for cursor != "" {
			counted.under = 0
			p, _ := graphSnap(t, d, reader, `{"op":"graph_snapshot","scope":"tenant","page_documents":2,"cursor":"`+cursor+`"}`)
			if counted.under != 0 {
				t.Errorf("page %d listed the whole tree %d times; want names by document only", pages+1, counted.under)
			}
			for i, id := range p.Documents.IDs {
				if len(p.Documents.Paths[i]) != 1 || !strings.HasPrefix(p.Documents.Paths[i][0], "/kb/d") {
					t.Errorf("page %d: document %s paths = %v", pages+1, id, p.Documents.Paths[i])
				}
			}
			cursor, pages = p.Page.NextCursor, pages+1
		}
		if pages != 3 || counted.byDocs == 0 {
			t.Errorf("%d pages, %d reverse lookups; want 3 pages (5 readable of 6, 2 a page) answered by reverse lookups", pages, counted.byDocs)
		}
	})
}

// Fifty thousand chunks in five hundred documents, paged 37 documents at a time:
// every chunk comes back exactly once, every document whole on one page, every
// parent before its children and pointing at the chunk's real parent. Inserted
// by SQL: through the tool the setup alone would take minutes.
func TestGraphSnapshot_PagesByDocumentNeverSplitATree(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		t.Fatal(err)
	}
	const nDocs, perDoc, rowsPerInsert = 500, 100, 500
	rng := rand.New(rand.NewPCG(7, 11))
	parentOf := map[string]string{}
	docOf := map[string]string{}
	var chunkRows, docRows []any
	flush := func(table, cols string, n int, rows *[]any) {
		t.Helper()
		if len(*rows) == 0 {
			return
		}
		one := "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
		count := len(*rows) / n
		stmt := "INSERT INTO " + table + " (" + cols + ") VALUES " + strings.TrimSuffix(strings.Repeat(one+",", count), ",")
		if err := d.exec(ctx, key, stmt, (*rows)...); err != nil {
			t.Fatalf("insert %s: %v", table, err)
		}
		*rows = (*rows)[:0]
	}
	const chunkCols, docCols = "id, document_id, parent_id, position, type, status, title, created_at, updated_at, revision", "id, title, root_chunk_id, type, status, created_at, updated_at"
	now := time.Now().UnixNano()
	for i := range nDocs {
		docID := newDocID()
		ids := make([]string, perDoc)
		kids := map[string]int{}
		for j := range ids {
			ids[j] = newDocID()
			var parent any
			pos := 0
			if j > 0 {
				p := ids[rng.IntN(j)]
				parent, pos = p, kids[p]
				kids[p]++
				parentOf[ids[j]] = p
			}
			docOf[ids[j]] = docID
			chunkRows = append(chunkRows, ids[j], docID, parent, pos, nil, nil, fmt.Sprintf("c%d", j), now, now, 1)
			if len(chunkRows) == rowsPerInsert*10 {
				flush("chunks", chunkCols, 10, &chunkRows)
			}
		}
		docRows = append(docRows, docID, fmt.Sprintf("Doc %d", i), ids[0], nil, nil, now, now)
	}
	flush("chunks", chunkCols, 10, &chunkRows)
	for len(docRows) > 0 {
		n := min(len(docRows), 100*7)
		batch := append([]any{}, docRows[:n]...)
		docRows = docRows[n:]
		flush("documents", docCols, 7, &batch)
	}

	pages, _ := snapshotAll(t, d, ctx, `{"op":"graph_snapshot","scope":"user","include":["documents","chunks"],"page_documents":37`)
	if want := (nDocs + 36) / 37; len(pages) != want {
		t.Errorf("%d pages, want %d", len(pages), want)
	}
	seenChunk := map[string]bool{}
	pageOfDoc := map[string]int{}
	perDocCount := map[string]int{}
	lastDoc := ""
	for pi, p := range pages {
		if p.Page.Documents != len(p.Documents.IDs) {
			t.Fatalf("page %d: page.documents %d, %d ids", pi, p.Page.Documents, len(p.Documents.IDs))
		}
		for _, id := range p.Documents.IDs {
			if id <= lastDoc {
				t.Fatalf("page %d: document %s out of id order after %s", pi, id, lastDoc)
			}
			lastDoc = id
			pageOfDoc[id] = pi
		}
		for i, id := range p.Chunks.IDs {
			if seenChunk[id] {
				t.Fatalf("chunk %s returned twice", id)
			}
			seenChunk[id] = true
			doc := p.Documents.IDs[p.Chunks.DocumentIdx[i]]
			if doc != docOf[id] {
				t.Fatalf("chunk %s placed in document %s, want %s", id, doc, docOf[id])
			}
			perDocCount[doc]++
			par := p.Chunks.ParentIdx[i]
			switch {
			case par >= i:
				t.Fatalf("page %d: chunk %d's parent_idx %d does not point backwards", pi, i, par)
			case par < 0 && parentOf[id] != "":
				t.Fatalf("chunk %s listed as a root; its parent is %s", id, parentOf[id])
			case par >= 0 && p.Chunks.IDs[par] != parentOf[id]:
				t.Fatalf("chunk %s: parent_idx names %s, want %s", id, p.Chunks.IDs[par], parentOf[id])
			}
		}
	}
	if len(seenChunk) != nDocs*perDoc || len(pageOfDoc) != nDocs {
		t.Fatalf("saw %d chunks in %d documents, want %d in %d", len(seenChunk), len(pageOfDoc), nDocs*perDoc, nDocs)
	}
	for doc, n := range perDocCount {
		if n != perDoc {
			t.Errorf("document %s came back with %d chunks, want all %d on one page", doc, n, perDoc)
		}
	}
}

// docFixtureWithRowCap is documentFixture with SQL Memory's row cap set.
func docFixtureWithRowCap(t *testing.T, maxRows int) (*Document, context.Context) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), MaxRows: maxRows})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	return &Document{Store: s, SqlMem: mgr, Bus: channels.NewBus()}, ctx
}

// When the row cap cuts the chunk statement, the page keeps the documents it
// read whole, says so in clamped_to, and the next page starts at the cut one; a
// single document larger than the cap is refused rather than split.
func TestGraphSnapshot_RowCapClampsThePageToWholeDocuments(t *testing.T) {
	d, ctx := docFixtureWithRowCap(t, 10)
	for i := range 3 {
		doc, _, part := snapDoc(t, d, ctx, "user", fmt.Sprintf("/kb/d%d", i), "Doc")
		snapChunk(t, d, ctx, "user", doc, part, "Child")
		snapChunk(t, d, ctx, "user", doc, "", "Sibling")
	}
	pages, _ := snapshotAll(t, d, ctx, `{"op":"graph_snapshot","scope":"user","include":["documents","chunks"],"page_documents":3`)
	if len(pages) != 2 || pages[0].Page.ClampedTo != 2 || len(pages[0].Documents.IDs) != 2 || len(pages[0].Chunks.IDs) != 8 {
		t.Fatalf("first page = %+v, want two whole documents and clamped_to 2", pages[0].Page)
	}
	if pages[1].Page.ClampedTo != 0 || len(pages[1].Documents.IDs) != 1 || len(pages[1].Chunks.IDs) != 4 {
		t.Errorf("second page = %+v with %d chunks, want the third document whole", pages[1].Page, len(pages[1].Chunks.IDs))
	}

	d, ctx = docFixtureWithRowCap(t, 3)
	doc, _, part := snapDoc(t, d, ctx, "user", "/kb/big", "Big")
	snapChunk(t, d, ctx, "user", doc, part, "Child")
	snapChunk(t, d, ctx, "user", doc, "", "Sibling") // four chunks, one over the cap
	_, r := docExec(t, d, ctx, `{"op":"graph_snapshot","scope":"user","include":["documents","chunks"]}`)
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryBusiness {
		t.Errorf("a document over the row cap: want a business refusal, got IsError=%v %s", r.IsError, r.Text)
	}
}

// chunk_ids selects the documents holding those chunks, together with
// document_ids; a chunk in a document the caller may not read selects nothing,
// exactly as an id that does not exist.
func TestGraphSnapshot_ChunkIdsResolveToDocuments(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		docs, roots, parts := make([]string, 3), make([]string, 3), make([]string, 3)
		for i := range docs {
			docs[i], roots[i], parts[i] = snapDoc(t, d, open, "tenant", fmt.Sprintf("/kb/d%d", i+1), "Doc")
		}
		ids := func(s snapResp) string { return strings.Join(s.Documents.IDs, ",") }
		sorted := func(v ...string) string { sort.Strings(v); return strings.Join(v, ",") }

		s, _ := graphSnap(t, d, open, fmt.Sprintf(`{"op":"graph_snapshot","scope":"tenant","chunk_ids":[%q,%q]}`, parts[1], roots[2]))
		if ids(s) != sorted(docs[1], docs[2]) {
			t.Errorf("chunk_ids: documents %s, want %s", ids(s), sorted(docs[1], docs[2]))
		}
		s, _ = graphSnap(t, d, open, fmt.Sprintf(`{"op":"graph_snapshot","scope":"tenant","document_ids":[%q],"chunk_ids":[%q]}`, docs[0], parts[2]))
		if ids(s) != sorted(docs[0], docs[2]) {
			t.Errorf("document_ids with chunk_ids: documents %s, want the union %s", ids(s), sorted(docs[0], docs[2]))
		}

		reader := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/d2"}}))
		hiddenS, hiddenText := graphSnap(t, d, reader, fmt.Sprintf(`{"op":"graph_snapshot","scope":"tenant","chunk_ids":[%q]}`, parts[1]))
		_, missingText := graphSnap(t, d, reader, `{"op":"graph_snapshot","scope":"tenant","chunk_ids":["0123456789abcdef0123456789abcdef"]}`)
		if hiddenS.Page.Documents != 0 || hiddenText != missingText {
			t.Errorf("a chunk of an unreadable document:\n%s\nwant the answer for a missing id:\n%s", hiddenText, missingText)
		}
	})
}

// The edge budget stops the relation list at exactly the budget and says so;
// a budget the page does not reach reports nothing cut.
func TestGraphSnapshot_EdgeBudgetFlagsTruncation(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, root, a := snapDoc(t, d, ctx, "user", "/kb/one", "One")
	b := snapChunk(t, d, ctx, "user", doc, "", "B")
	snapLink(t, d, ctx, "user", a, b)
	snapLink(t, d, ctx, "user", b, root)
	snapLink(t, d, ctx, "user", a, root)
	for _, c := range []struct {
		budget, want int
		truncated    bool
	}{{2, 2, true}, {3, 3, false}, {4, 3, false}} {
		s, _ := graphSnap(t, d, ctx, fmt.Sprintf(`{"op":"graph_snapshot","scope":"user","edge_budget":%d}`, c.budget))
		if len(s.Edges.IDs) != c.want || s.Edges.Truncated != c.truncated {
			t.Errorf("edge_budget=%d: %d edges, truncated=%v; want %d, %v", c.budget, len(s.Edges.IDs), s.Edges.Truncated, c.want, c.truncated)
		}
	}
}

// parent_idx always points at an earlier row, and at the chunk's real parent.
func TestGraphSnapshot_ParentIndexPointsBackwards(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, root, a := snapDoc(t, d, ctx, "user", "/kb/tree", "Tree")
	want := map[string]string{a: root}
	parent := a
	for i := range 6 {
		c := snapChunk(t, d, ctx, "user", doc, parent, fmt.Sprintf("Level %d", i))
		want[c] = parent
		side := snapChunk(t, d, ctx, "user", doc, parent, fmt.Sprintf("Side %d", i))
		want[side] = parent
		parent = c
	}
	s, _ := graphSnap(t, d, ctx, `{"op":"graph_snapshot","scope":"user"}`)
	if len(s.Chunks.IDs) != len(want)+1 || s.Chunks.IDs[0] != root || s.Chunks.ParentIdx[0] != -1 {
		t.Fatalf("chunks %v (parents %v), want the root first and all %d chunks", s.Chunks.IDs, s.Chunks.ParentIdx, len(want)+1)
	}
	for i, id := range s.Chunks.IDs[1:] {
		p := s.Chunks.ParentIdx[i+1]
		if p < 0 || p > i || s.Chunks.IDs[p] != want[id] {
			t.Errorf("chunk %s at %d: parent_idx %d, want an earlier row holding %s", id, i+1, p, want[id])
		}
	}

	t.Run("a broken tree still lists every chunk once", func(t *testing.T) {
		// A and B are each other's parent; D's parent is not in the document.
		rows := []snapshotChunk{{id: "A", parentID: "B"}, {id: "B", parentID: "A"}, {id: "C"}, {id: "D", parentID: "gone"}, {id: "E", parentID: "C"}}
		order, parents := snapshotTreeOrder("C", rows)
		if len(order) != len(rows) {
			t.Fatalf("order %v, want each of %d rows once", order, len(rows))
		}
		seen := map[int]bool{}
		for i, r := range order {
			if seen[r] {
				t.Fatalf("row %d listed twice: %v", r, order)
			}
			seen[r] = true
			if parents[i] >= i {
				t.Errorf("row %d at %d has parent %d, not an earlier row", r, i, parents[i])
			}
		}
		if rows[order[0]].id != "C" {
			t.Errorf("first row %s, want the document's root C", rows[order[0]].id)
		}
	})
}

// Every array of a section has one entry per item, and every code names an
// entry of its codes table.
func TestGraphSnapshot_ColumnsAreParallel(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, _, part := snapDoc(t, d, ctx, "user", "/docs/plan", "Plan")
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+part+`","type":"note","status":"draft","valid_at":1700000000000000000,"revision":1}`); r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"plan|ships|march","title":"It ships in March","body":"It ships in March.","confidence":0.9}`, doc))
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	fact := out["id"].(string)
	snapLink(t, d, ctx, "user", part, fact)
	other, _, otherPart := snapDoc(t, d, ctx, "user", "/docs/other", "Other")
	snapLink(t, d, ctx, "user", fact, otherPart)

	res, err := d.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"graph_snapshot","scope":"user","document_ids":[%q],"include":["documents","chunks","edges","dirents","facts","projection"]}`, doc)))
	if err != nil || res.IsError {
		t.Fatalf("graph_snapshot: %v %s", err, res.Text)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal([]byte(res.Text), &raw); err != nil {
		t.Fatal(err)
	}
	codes := map[string]int{}
	for k, v := range raw["codes"] {
		codes[k] = len(v.([]any))
	}
	codeTable := map[string]string{"type_code": "types", "status_code": "statuses", "kind_code": "kinds", "fact_class_code": "fact_classes", "method_code": "methods"}
	for section, want := range map[string]int{"documents": 1, "chunks": 3, "edges": 2, "dirents": 3} {
		cols := raw[section]
		if len(cols) == 0 {
			t.Fatalf("no %s section: %s", section, res.Text)
		}
		for name, v := range cols {
			arr, ok := v.([]any)
			if !ok || name == "external" {
				continue
			}
			if len(arr) != want {
				t.Errorf("%s.%s has %d entries, want %d", section, name, len(arr), want)
			}
			if table, coded := codeTable[name]; coded {
				for _, c := range arr {
					if n := int(c.(float64)); n < -1 || n >= codes[table] {
						t.Errorf("%s.%s = %d, outside codes.%s (%d entries)", section, name, n, table, codes[table])
					}
				}
			}
		}
	}
	if m := raw["projection"]["missing"].([]any); len(m) != 3 {
		t.Errorf("projection.missing = %v, want every chunk index", m)
	}

	s, _ := graphSnap(t, d, ctx, fmt.Sprintf(`{"op":"graph_snapshot","scope":"user","document_ids":[%q]}`, doc))
	for i, id := range s.Chunks.IDs {
		isFact := id == fact
		if s.Chunks.IsFact[i] != isFact {
			t.Errorf("chunk %s: is_fact %v, want %v", id, s.Chunks.IsFact[i], isFact)
		}
		if isFact && (s.Chunks.FactConfidence[i] == nil || *s.Chunks.FactConfidence[i] != 0.9) {
			t.Errorf("fact confidence = %v, want 0.9", s.Chunks.FactConfidence[i])
		}
		if id == part && (s.Chunks.ValidAt[i] == nil || *s.Chunks.ValidAt[i] != 1700000000000000000) {
			t.Errorf("dated chunk valid_at = %v", s.Chunks.ValidAt[i])
		}
	}
	if got := strings.Join(s.Dirents.Paths, ","); got != "/docs,/docs/other,/docs/plan" {
		t.Errorf("dirents %s, want /docs and both names", got)
	}
	for i, p := range s.Dirents.Paths {
		wantIdx, wantID := -1, ""
		switch p {
		case "/docs/plan":
			wantIdx, wantID = 0, doc
		case "/docs/other":
			wantID = other // named, but on no page of this selection
		}
		if s.Dirents.DocumentIdx[i] != wantIdx || s.Dirents.DocumentIDs[i] != wantID {
			t.Errorf("dirent %s: document_idx %d id %q, want %d %q", p, s.Dirents.DocumentIdx[i], s.Dirents.DocumentIDs[i], wantIdx, wantID)
		}
	}
	if got := s.Documents.Paths; len(got) != 1 || strings.Join(got[0], ",") != "/docs/plan" {
		t.Errorf("documents.paths = %v", got)
	}
}

// An end on another document is an external entry carrying that document's id,
// one entry per chunk however many relations reach it.
func TestGraphSnapshot_ExternalEndpointsCarryDocumentID(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	a, _, a1 := snapDoc(t, d, ctx, "user", "/kb/a", "A")
	a2 := snapChunk(t, d, ctx, "user", a, "", "A2")
	b, _, b1 := snapDoc(t, d, ctx, "user", "/kb/b", "B")
	snapLink(t, d, ctx, "user", a1, b1)
	snapLink(t, d, ctx, "user", b1, a2)
	snapLink(t, d, ctx, "user", a1, a2)

	s, _ := graphSnap(t, d, ctx, fmt.Sprintf(`{"op":"graph_snapshot","scope":"user","document_ids":[%q]}`, a))
	if len(s.Edges.IDs) != 3 {
		t.Fatalf("%d edges, want all 3 with an end in A", len(s.Edges.IDs))
	}
	if len(s.Edges.External) != 1 || s.Edges.External[0].Idx != -1 || s.Edges.External[0].ID != b1 || s.Edges.External[0].DocumentID != b {
		t.Fatalf("external = %+v, want one entry {-1, %s, %s}", s.Edges.External, b1, b)
	}
	got := map[string]bool{}
	for i := range s.Edges.IDs {
		got[s.endID(t, s.Edges.FromIdx[i])+">"+s.endID(t, s.Edges.ToIdx[i])] = true
	}
	for _, e := range []string{a1 + ">" + b1, b1 + ">" + a2, a1 + ">" + a2} {
		if !got[e] {
			t.Errorf("edge %s missing from %v", e, got)
		}
	}
}

// A call the op cannot answer as asked is refused with what to change; a page
// size over the maximum is lowered, not refused.
func TestGraphSnapshot_RefusesWhatItCannotAnswer(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	snapDoc(t, d, ctx, "user", "/kb/one", "One")
	for _, body := range []string{
		`{"op":"graph_snapshot","scope":"user","include":["bodies"]}`,
		`{"op":"graph_snapshot","scope":"user","include":["documents","edges"]}`,
		`{"op":"graph_snapshot","scope":"user","cursor":"not-a-cursor"}`,
		`{"op":"graph_snapshot","scope":"user","as_of":-1}`,
	} {
		_, r := docExec(t, d, ctx, body)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation {
			t.Errorf("%s: want a validation refusal, got IsError=%v %s", body, r.IsError, r.Text)
		}
	}
	if s, _ := graphSnap(t, d, ctx, `{"op":"graph_snapshot","scope":"user","page_documents":100000}`); s.Page.Documents != 1 {
		t.Errorf("page_documents over the maximum: %d documents, want the one", s.Page.Documents)
	}
}
