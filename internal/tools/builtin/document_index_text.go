package builtin

// document_index_text.go — THE text a document chunk is indexed under.
//
// Every path that writes or judges a chunk's embed text goes through chunkIndexText:
// the write path (embedBody), the admin backfill, the admin re-embed, and the stale-
// embedding purge. They used to compose the text separately, and drifted: re-embed
// indexed the raw JSON envelope, and both admin paths dropped an image's generated
// description. A rule that decides what search can FIND has to be one rule — a purge
// judging "indexable" differently from the writer deletes rows the writer re-creates,
// and a re-embed deriving a different text silently changes what a scope returns.
//
// WHAT CHANGED WITH IT: the text now carries a HEADER naming where the chunk sits —
//
//	<document title> — <heading path of the chunk>
//	<content>
//
// A section's text often never names its document or even its own topic ("we use the
// same setting as above"), so a question about one document retrieved another's chunk.
// Measured before adopting it (pre-registered, 911 Natural Questions over 911
// Wikipedia pages): R@5 0.912 → 0.935, p=0.0046. The header is DERIVED from the chunk
// tree at index time and never stored, so it cannot go stale in storage — only in the
// index, which the rename/move re-index keeps current.
//
// Readers never see it: bodies, get_chunk, export_md and search results are unchanged.

import (
	"context"
	"log"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// indexHeaderSep separates the document title from the heading path, and
// indexPathSep the headings within it.
const (
	indexHeaderSep = " — "
	indexPathSep   = " > "
	// chunkLineageMaxDepth bounds the walk to the root. A document is a tree, so the walk
	// ends at the root long before this; the bound only stops a corrupted parent cycle
	// from looping the query.
	chunkLineageMaxDepth = 64
)

// chunkLineage returns the titles from the document root down to the chunk (root first,
// the chunk itself last) and the chunk's stored type. ok is false when the chunk does
// not exist or the read failed — the caller indexes nothing rather than guessing.
func (d *Document) chunkLineage(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (titles []string, chunkType string, ok bool) {
	if d.SqlMem == nil {
		return nil, "", false
	}
	res, err := d.query(ctx, key, `
		WITH RECURSIVE lineage(id, parent_id, title, type, depth) AS (
			SELECT id, parent_id, title, type, 0 FROM chunks WHERE id = ?
			UNION ALL
			SELECT c.id, c.parent_id, c.title, c.type, l.depth + 1
			  FROM chunks c JOIN lineage l ON c.id = l.parent_id
			 WHERE l.depth < ?
		)
		SELECT title, type, depth FROM lineage ORDER BY depth DESC`, chunkID, chunkLineageMaxDepth)
	if err != nil || len(res.Rows) == 0 {
		return nil, "", false
	}
	titles = make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		titles = append(titles, strings.TrimSpace(asStr(r[0])))
	}
	// The chunk itself is the LAST row (depth 0), so its type is there.
	return titles, asStr(res.Rows[len(res.Rows)-1][1]), true
}

// chunkContentText is the type-specific part of the index text — what embedBody
// derived before the header existed, unchanged:
//
//   - prose   → the body, minus Markdown scaffolding (indexableText)
//   - mermaid → extracted labels + diagram kind
//   - image   → title + caption + the generated description
//
// skip is true for a body that is entirely a NON-mermaid media form (a data-URL image on
// an untyped chunk). Such a chunk was never indexed — its searchable text is a generated
// description, not the payload — and a header must not change that.
func (d *Document) chunkContentText(ctx context.Context, key sqlmem.ScopeKey, chunkID, chunkType, title, body string) (content string, skip bool) {
	switch chunkType {
	case "image":
		return imageEmbedText(title, body, d.assetDescription(ctx, key, chunkID)), false
	case "mermaid":
		return mermaidEmbedText(body), false
	}
	if typ, _, _, src := classifyMediaBody(body); typ != "" {
		if typ != "mermaid" {
			return "", true
		}
		return mermaidEmbedText(src), false
	}
	return indexableText(body), false
}

// chunkIndexText is the ONE definition of the text a chunk is indexed under. It returns
// "" when the chunk must not be indexed at all.
//
// The rules, in order:
//
//   - A body that is a non-mermaid media payload is never indexed (see chunkContentText).
//   - A chunk with CONTENT is indexed as header + "\n" + content.
//   - A non-root chunk with no content is indexed under its header alone — but only when
//     its OWN heading is real language. This is the old title fallback, generalised: a
//     bodyless chunk is usually a heading, and a heading is a real answer to a search.
//     A chunk whose own title carries no letter is still not indexed; the header must not
//     turn it into a row that matches the document's name and nothing else.
//   - The document ROOT with no content is NOT indexed. It used to be indexed under its
//     title, and then it outranked every section for any query naming the document
//     (measured: rank 1 on every question), while carrying nothing to read. With headers
//     every section carries the title, so the document stays findable through them.
//
// When the chunk tree cannot be read (no SQL Memory, a body whose chunk row is gone, a
// failed read), the text falls back to the CONTENT alone — what the old rule produced
// without a title. That direction is deliberate: the stale-embedding purge deletes a
// vector whose text derives to "", so failing to "" on a read error would un-index real
// content.
//
// chunkType may be "" (the admin paths do not know it); it is then read from the store.
func (d *Document) chunkIndexText(ctx context.Context, key sqlmem.ScopeKey, chunkID, chunkType, body string) string {
	titles, storedType, ok := d.chunkLineage(ctx, key, chunkID)
	if !ok {
		content, skip := d.chunkContentText(ctx, key, chunkID, chunkType, "", body)
		if skip {
			return ""
		}
		return content
	}
	if chunkType == "" {
		chunkType = storedType
	}
	own := titles[len(titles)-1]
	content, skip := d.chunkContentText(ctx, key, chunkID, chunkType, own, body)
	if skip {
		return ""
	}
	header := indexHeader(titles)
	switch {
	case content != "" && header != "":
		return header + "\n" + content
	case content != "":
		return content
	case len(titles) == 1: // the bodyless root
		return ""
	case indexableText(own) == "": // a heading with no language in it
		return ""
	default:
		return header
	}
}

// indexHeader renders "<document title> — <heading path>" from the root-first titles.
// The root alone renders as just the document title. A part with no letter in it
// (an empty title, "---") is dropped rather than indexed, the same judgement
// indexableText applies to bodies; a header left with nothing is "".
func indexHeader(titles []string) string {
	if len(titles) == 0 {
		return ""
	}
	doc := indexableText(titles[0])
	var path []string
	for _, t := range titles[1:] {
		if t = indexableText(t); t != "" {
			path = append(path, t)
		}
	}
	switch {
	case doc != "" && len(path) > 0:
		return doc + indexHeaderSep + strings.Join(path, indexPathSep)
	case doc != "":
		return doc
	default:
		return strings.Join(path, indexPathSep)
	}
}

// ChunkIndexTextForRow is chunkIndexText for the admin paths, which see memory ROWS
// rather than chunks: the backfill, the re-embed and the stale-embedding purge.
//
// isChunk is false for any row that is not a document chunk body — the caller keeps its
// own rule for those. For a chunk body, text is exactly what the write path would index
// ("" = index nothing). It lives here so the ""→"default" SQL-tenant rule stays in this
// package: chunk bodies key on the RAW tenant while SQL Memory canonicalises it, and
// restating that at a call site is how the tenant axis drifts.
func ChunkIndexTextForRow(ctx context.Context, mgr *sqlmem.Manager, tenant string,
	mscope store.MemoryScope, scopeID string, row store.MemoryEntry) (text string, isChunk bool) {

	// No SQL Memory means no tree to read a header or a type from; the builders then
	// fall back to the content alone (see chunkIndexText's fail-safe note), so a nil
	// manager is handled there rather than by indexing nothing here. A derived search
	// unit is a Document row too: an admin re-embed that treated it as a plain row
	// would embed its JSON envelope.
	d := &Document{SqlMem: mgr}
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenantValue(tenant), Scope: string(mscope), ScopeID: scopeID}
	return d.rowIndexText(ctx, key, row)
}

// reindexSyncMax is the subtree size up to which a re-index runs inline, before the op
// returns. Renaming a leaf heading is the common case and is then consistent the moment
// the call returns; a large subtree (a renamed document root re-indexes EVERY chunk)
// runs in the background instead of holding the caller for the length of its embedding
// calls.
const reindexSyncMax = 32

// subtreeChunks returns the chunk and every descendant, with their stored types. A
// SQL Memory row cap can clip a very large subtree; the clipped chunks keep their old
// index text until the operator re-index pass, and the truncation is logged.
func (d *Document) subtreeChunks(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (ids, types []string, err error) {
	res, err := d.query(ctx, key, `
		WITH RECURSIVE sub(id, type, depth) AS (
			SELECT id, type, 0 FROM chunks WHERE id = ?
			UNION ALL
			SELECT c.id, c.type, s.depth + 1
			  FROM chunks c JOIN sub s ON c.parent_id = s.id
			 WHERE s.depth < ?
		)
		SELECT id, type FROM sub`, chunkID, chunkLineageMaxDepth)
	if err != nil {
		return nil, nil, err
	}
	if res.Truncated {
		log.Printf("document: re-index of the subtree under %s was clipped at %d chunks by the SQL Memory row cap; "+
			"the rest keep their old index text until POST /v1/_document/reindex runs", chunkID, len(res.Rows))
	}
	for _, r := range res.Rows {
		ids = append(ids, asStr(r[0]))
		types = append(types, asStr(r[1]))
	}
	return ids, types, nil
}

// reindexSubtree brings the index text of a chunk and everything under it back in line
// with the tree. It runs after anything that changes a title ABOVE other chunks — a
// rename (a chunk's title is in its descendants' headers, and a root's title is in
// every chunk's) or a move (the heading path changes for the whole moved subtree).
// Best-effort like every embed: a failure leaves a chunk under its old text and never
// fails the op that triggered it.
func (d *Document) reindexSubtree(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkID string) {
	if d.Embedder == nil || d.Store == nil || d.SqlMem == nil {
		return
	}
	ids, types, err := d.subtreeChunks(ctx, key, chunkID)
	if err != nil {
		log.Printf("document: re-index of the subtree under %s: %v", chunkID, err)
		return
	}
	run := func(ctx context.Context) {
		tenant := direntTenant(ctx)
		for i, id := range ids {
			cb, err := d.readBody(ctx, mscope, key.ScopeID, id)
			if err != nil {
				log.Printf("document: re-index %s: read body: %v", id, err)
				continue
			}
			d.embedBody(ctx, tenant, mscope, key, chunkBodyKey(id), id, types[i], cb.Body)
			// A chunk's units carry its header too.
			d.reindexUnitsOf(ctx, tenant, mscope, key, id)
		}
	}
	if len(ids) <= reindexSyncMax {
		run(ctx)
		return
	}
	// Detached from the caller's cancellation (the op has returned) but keeping its
	// values — the run identity and tenant the store keys on.
	bg := context.WithoutCancel(ctx)
	d.reindexJobs.Add(1)
	go func() {
		defer d.reindexJobs.Done()
		run(bg)
	}()
}

// waitReindex blocks until every background subtree re-index has finished. Tests use it
// to observe the end state of a large re-index.
func (d *Document) waitReindex() { d.reindexJobs.Wait() }
