package builtin

// document_graph.go — graph-expanded, time-aware recall over the entity tier
// (RFC BL P4c PR 4).
//
// WHAT IS NEW HERE, AND WHAT IS DELIBERATELY NOT. The genuinely new parts are the
// BOUNDED EXPANSION over typed edges and the TEMPORAL filter. Hybrid seed
// retrieval — vector ∥ full-text fused by RRF — already exists on the Memory tool's
// recall path, and is NOT reimplemented: a caller runs whatever retrieval it likes
// and hands the chunk ids in via seed_ids. Composition rather than a second ranker
// that would drift from the first.
//
// `query` is offered as a convenience for the common case, and it is honest about
// being a TITLE match in SQL rather than a semantic search. For an entity graph
// that is more useful than it sounds — an entity's title IS its name — and it needs
// no embedder, so it works identically on both tiers and in deployments with no
// vector stack at all.
//
// NO GRAPH DATABASE. Expansion is a bounded breadth-first walk in SQL over
// chunk_edges, one round trip per hop, bounded on three axes: how DEEP it goes
// (graphMaxHops), how many nodes each hop may expand FROM (graphFrontierCap), and
// how many rows one hop may return (graphHopRowCap). The three are separate
// because bounding only the first two still leaves the row count as the product
// of frontier size and degree — see graphHopRowCap.
//
// What bounds DRIFT is none of those, though: it is the content budget, which caps
// what actually reaches the reader. A hop count caps the wrong thing, since one hop
// into a hub is worse than six along a chain.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// graphMaxHops bounds the walk. A hop is ONE edge in either direction, so a
	// fact -> entity -> fact cycle costs two: at 2 the walk answered a two-hop
	// question and nothing deeper, which is what RFC CV's P5 exists to lift.
	// Three-hop chains need four and four-hop need six.
	//
	// The old cap's reason — "past two hops a result stops describing what you
	// asked about" — is a real worry about drift, and it is now measured rather
	// than assumed: at this depth traversal scored 100% on two-hop and 66% on
	// three-hop chains while the shuffled-relation control LOST to plain search,
	// so the walk was following relations rather than wandering. What actually
	// bounds drift is the CONTENT BUDGET below, which caps what reaches the
	// reader; a hop count caps the wrong thing, since one hop into a hub is
	// worse than six along a chain.
	graphMaxHops      = 6
	graphDefaultHops  = 1
	graphDefaultLimit = 50
	graphMaxLimit     = 200
	// graphFrontierCap bounds how many nodes ONE hop expands FROM. Without it a
	// single heavily-connected entity turns hop 2 into a scan of the scope's edges,
	// and the caller sees a slow recall rather than a truncated one.
	graphFrontierCap = 500
	// graphHopRowCap bounds how many rows ONE hop may RETURN, which the frontier cap
	// does not: it bounds nodes, and the row count is nodes × degree.
	//
	// Degree is not small in practice. On a 304-chunk benchmark store the maximum
	// in-degree was 162 — one hub entity with 162 facts pointing at it — so a single
	// frontier node can return 162 rows and a full frontier can return five figures.
	// Unbounded, those rows are materialised in memory AND accumulated into `order`,
	// which graphIdentityNodes then turns into one placeholder per id; past the
	// driver's parameter ceiling that query fails, and it fails SILENTLY (it returns
	// an empty set by design), so the assertion-first budget ordering reverts to the
	// behaviour it was added to fix — on exactly the dense graphs where it matters.
	//
	// Generous rather than tight: the content budget is what decides what reaches
	// the reader, so this only has to stop the pathological case.
	graphHopRowCap = 2000
	// graphIDBatch bounds one IN(...) list. Keeps every id-set query under the
	// driver ceilings (SQLite 32766 params, Postgres 65535) regardless of how the
	// caller got there, so a future change to the caps above cannot reintroduce the
	// silent failure described on graphHopRowCap.
	graphIDBatch = 500
	// graphSeedTopK is how many ranked facts semantic seeding considers. The walk
	// starts from the best few and the rest are held for the backfill below.
	graphSeedTopK = 50
	// graphDefaultSeeds is how many of those ranked facts actually seed the walk.
	// Seeding from ALL of them spends the whole budget before the walk contributes
	// anything — measured: with 50 seeds and room for 28 facts, traversal and
	// plain search returned an identical set.
	graphDefaultSeeds = 5
)

// graphSeedInfo records how a walk started. A caller that cannot tell a semantic
// seeding from a title match cannot tell why a recall came back thin.
type graphSeedInfo struct {
	How    string   // "ids" | "semantic" | "title"
	Ranked []string // semantic only: the full ranked list, for the backfill
}

// graphChunk is one row of the answer, carrying HOW it was reached. A caller that
// cannot tell a seed from a two-hop neighbour cannot tell a direct answer from an
// association, which is the difference between recall and free association.
type graphChunk struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status,omitempty"`
	Hop      int    `json:"hop"`
	ViaKind  string `json:"via_kind,omitempty"`
	ViaID    string `json:"via_id,omitempty"`
	ValidAt  *int64 `json:"valid_at,omitempty"`
	Invalid  *int64 `json:"invalid_at,omitempty"`
	Retired  bool   `json:"retired"`
	Superset bool   `json:"-"`
}

func (d *Document) graphRecall(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	hops := graphDefaultHops
	if in.Hops != nil {
		hops = *in.Hops
	}
	if hops < 0 || hops > graphMaxHops {
		return errResult(fmt.Sprintf("graph_recall: hops must be 0..%d (got %d) — a hop is one edge, so a fact→entity→fact step costs two; bound a long walk with budget_chars rather than by stopping it short", graphMaxHops, hops)), nil
	}
	// docInput.Limit is shared with the other list-shaped ops (a plain int, 0 =
	// unset), so it is reused rather than shadowed with a second limit field.
	limit := graphDefaultLimit
	if in.Limit > 0 {
		limit = in.Limit
	}
	if limit > graphMaxLimit {
		limit = graphMaxLimit
	}
	if len(in.SeedIDs) == 0 && strings.TrimSpace(in.Query) == "" {
		return errResult("graph_recall: give either seed_ids (chunks to start from) or query (match starting chunks by title)"), nil
	}
	// REFUSED, not truncated. Seeds go into one IN(...), so an unbounded list also
	// walks into the driver's placeholder ceiling; and silently starting from a
	// subset of the chunks a caller named is the failure this op just fixed one
	// layer down. Naming the number back is what lets a caller split the walk.
	if len(in.SeedIDs) > graphFrontierCap {
		return errResult(fmt.Sprintf("graph_recall: %d seed_ids is more than the %d a single walk starts from — "+
			"split them across calls rather than have some silently dropped", len(in.SeedIDs), graphFrontierCap)), nil
	}

	seeds, seedInfo, err := d.graphSeedIDs(ctx, key, in, limit)
	if err != nil {
		return errResult("graph_recall: seeds: " + err.Error()), nil
	}
	if len(seeds) == 0 {
		return okJSONCount(map[string]any{"chunks": []graphChunk{}, "seeds": 0, "hops": hops, "truncated": false}, 0)
	}

	// Breadth-first, one round trip per hop. `seen` is keyed on chunk id so a
	// diamond in the graph is visited once and the SHALLOWEST path wins — reporting
	// a chunk as two hops away when a one-hop path exists would overstate its
	// distance from the question.
	seen := make(map[string]graphChunk, len(seeds))
	order := make([]string, 0, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, c := range seeds {
		if _, dup := seen[c.ID]; dup {
			continue
		}
		seen[c.ID] = c
		order = append(order, c.ID)
		frontier = append(frontier, c.ID)
	}

	truncated := false
	for hop := 1; hop <= hops && len(frontier) > 0; hop++ {
		if len(frontier) > graphFrontierCap {
			frontier = frontier[:graphFrontierCap]
			truncated = true
		}
		next, hopFull, nerr := d.graphNeighbours(ctx, key, frontier, hop, in, graphHopRowCap)
		if nerr != nil {
			return errResult("graph_recall: hop " + fmt.Sprint(hop) + ": " + nerr.Error()), nil
		}
		if hopFull {
			truncated = true
		}
		frontier = frontier[:0]
		for _, c := range next {
			if _, dup := seen[c.ID]; dup {
				continue
			}
			seen[c.ID] = c
			order = append(order, c.ID)
			frontier = append(frontier, c.ID)
		}
	}

	// ⚠️ THE BUDGET IS ON CONTENT, NOT ROWS, AND THE BACKFILL IS WHY.
	//
	// A walk fetches more rows than a search by construction, so a row limit lets
	// traversal win on volume rather than on the relations — the trap the
	// shuffled-relation control exists to catch. Worse, on a SPARSE graph the walk
	// runs dry and under-fills: measured at natural link density it returned 587 of
	// 1200 characters, and pure traversal then scored BELOW plain search, because
	// the unspent budget was free accuracy it declined to take.
	//
	// So spend the walk first, then give whatever is left to the ranked facts the
	// semantic seeding already produced. Measured across link densities, that never
	// loses to either the walk or the search alone, and at the sparse end it is 8%
	// where the bare walk is 2%.
	out := make([]graphChunk, 0, len(order))
	used, backfilled := 0, 0
	budget := in.BudgetChars
	// ⚠️ SPEND THE BUDGET ON ASSERTIONS FIRST. An entity node's title is a NAME
	// ("Yelby", "Imke Zoltan"); it carries no claim, and a reader handed one
	// learns nothing it can answer from. Charging names at the same priority as
	// facts cost a live walk the middle link of a three-relation chain: 17 of 42
	// rows were entity nodes, the budget filled at 1194 of 1200, and
	// "Lumfield Textiles is based in Istcombe" — the hop that joins the two ends
	// the walk DID find — was pushed out by names.
	//
	// Entity nodes still come back, because they are how a caller sees the path
	// the walk took; they simply queue behind the facts rather than ahead of them.
	// BFS order is preserved inside each pass, so a nearer fact still beats a
	// further one.
	passes := [][]string{order}
	if budget > 0 {
		// WHY THE EDGE AND NOT THE CHUNK TYPE. A distilled fact's type is the
		// constant "fact", which makes `type = 'fact'` the obvious test — but
		// `remember` stamps the CALLER's type, so that filter demotes an
		// operator-remembered fact to a name. Being the TARGET of an `about` edge
		// is what an identity node structurally is, which is the same test the
		// verification-coverage query settled on for the same reason.
		identity := d.graphIdentityNodes(ctx, key, order)
		facts, entities := make([]string, 0, len(order)), make([]string, 0, len(order))
		for _, id := range order {
			if identity[id] {
				entities = append(entities, id)
			} else {
				facts = append(facts, id)
			}
		}
		passes = [][]string{facts, entities}
	}
	// ⚠️ `limit` BOUNDS BOTH MODES. It used to sit in the `else` of the budget test,
	// so a caller that passed both got the row cap silently dropped — accepted,
	// ignored, and no signal that it had not applied. That is the same shape as the
	// `sources` selector that decoded and was discarded, and as `limit` vs `top_k`
	// on the search path before it; a parameter a caller can set and cannot observe
	// is worse than one that is refused.
	//
	// The two bounds compose rather than replace: budget caps what reaches the
	// reader by CONTENT, limit caps it by ROWS, and whichever binds first wins.
	for _, pass := range passes {
		for _, id := range pass {
			if len(out) >= limit {
				truncated = true
				break
			}
			c := seen[id]
			if budget > 0 {
				if used+len(c.Title) > budget {
					truncated = true
					continue
				}
				used += len(c.Title)
			}
			out = append(out, c)
		}
	}
	if budget > 0 && seedInfo.How == "semantic" {
		pending := make([]string, 0, len(seedInfo.Ranked))
		for _, id := range seedInfo.Ranked {
			if _, dup := seen[id]; !dup {
				pending = append(pending, id)
			}
		}
		rows := d.graphChunksByID(ctx, key, pending)
		// Walked in RANK order, not the order the batch read returned them.
		for _, id := range pending {
			if len(out) >= limit {
				truncated = true
				break
			}
			row, ok := rows[id]
			if !ok {
				continue
			}
			if used+len(row.Title) > budget {
				continue
			}
			used += len(row.Title)
			row.Hop = -1 // reached by rank, not by an edge: never claim a path
			out = append(out, row)
			seen[id] = row
			backfilled++
		}
	}
	payload := map[string]any{
		"chunks": out, "seeds": len(seeds), "hops": hops, "truncated": truncated,
		"seeded_by": seedInfo.How,
	}
	if budget > 0 {
		// An arm that cannot fill its budget says so, rather than leaving the
		// reader to infer a thin result from a short list.
		payload["budget_chars"] = budget
		payload["chars_used"] = used
		payload["backfilled"] = backfilled
	}
	return okJSONCount(payload, len(out))
}

// graphIdentityNodes reports which of these chunks are identity nodes — the
// targets of an `about` edge. One round trip for the whole set.
//
// A miss is safe in the direction that matters: an unreadable store returns an
// empty set, every chunk is then treated as an assertion, and the budget spends
// exactly as it did before this existed.
func (d *Document) graphIdentityNodes(ctx context.Context, key sqlmem.ScopeKey, ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	// BATCHED. A walk's `order` is unbounded by node count, and one placeholder per
	// id runs into the driver ceiling (SQLite 32766, Postgres 65535) on a dense
	// graph. The failure is silent — the error path below returns an empty set on
	// purpose — so an over-long list would quietly disable assertion-first ordering
	// instead of reporting anything.
	for _, batch := range batchIDs(ids, graphIDBatch) {
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		res, err := d.query(ctx, key,
			`SELECT DISTINCT to_id FROM chunk_edges WHERE kind = 'about' AND to_id IN (`+
				placeholders(len(batch))+`)`, args...)
		if err != nil {
			return out
		}
		for _, row := range res.Rows {
			if len(row) > 0 {
				if id, ok := row[0].(string); ok && id != "" {
					out[id] = true
				}
			}
		}
	}
	return out
}

// batchIDs splits ids into chunks of at most size.
func batchIDs(ids []string, size int) [][]string {
	if size <= 0 || len(ids) <= size {
		return [][]string{ids}
	}
	out := make([][]string, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[start:end])
	}
	return out
}

// graphChunksByID reads the backfill candidates in ONE round trip per batch, keyed
// by id so the caller can walk them in RANK order rather than the order SQL
// happened to return.
//
// Separate from the seed query because a backfilled row is NOT a seed: it was
// reached by rank, never by an edge, and conflating the two would let a caller read
// an association as a path.
//
// It reads the whole candidate set up front rather than one row at a time. The
// per-id version issued one query per ranked id — up to graphSeedTopK of them — and
// issued them BEFORE testing the budget, so a walk whose budget was already full
// still paid for every remaining round trip and threw each result away.
func (d *Document) graphChunksByID(ctx context.Context, key sqlmem.ScopeKey, ids []string) map[string]graphChunk {
	out := make(map[string]graphChunk, len(ids))
	if len(ids) == 0 {
		return out
	}
	for _, batch := range batchIDs(ids, graphIDBatch) {
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		res, err := d.query(ctx, key,
			`SELECT c.id, c.title, c.type, c.status, m.valid_at, m.invalid_at
			   FROM chunks c LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
			  WHERE c.id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return out
		}
		for _, row := range scanGraphRows(res.Rows, 0, "", "") {
			if row.ID != "" {
				out[row.ID] = row
			}
		}
	}
	return out
}

// graphSemanticSeeds ranks fact bodies against the query by VECTOR similarity and
// returns their chunk ids, best first.
//
// The title match graphSeeds falls back to requires the caller to already know the
// entity's NAME, which is the thing a question usually does not supply: "which
// region does Toma Zoltan work in" names a person and wants a region. Seeding from
// the fact bodies instead is what lets a walk start from the question — RFC CV's
// P5 named this as its candidate, and the benchmark that cleared P5's gate did
// exactly this.
//
// Returns nil (not an error) when semantic seeding is unavailable — no embedder,
// no vector support — so the caller falls back to the title match rather than
// failing a recall that used to work.
func (d *Document) graphSemanticSeeds(ctx context.Context, key sqlmem.ScopeKey, query string, topK int) []string {
	if d.Embedder == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	vec, err := d.Embedder.Embed(ctx, []string{query})
	if err != nil || len(vec) == 0 {
		return nil
	}
	mscope := store.MemoryScope(key.Scope)
	entries, err := d.Store.MemoryEmbedSearch(ctx, direntTenant(ctx), mscope, key.ScopeID,
		store.MemorySearchFilter{KeyPrefix: chunkBodyKeyPrefix}, vec[0], topK)
	if err != nil {
		// A store with no vector index answers the title way rather than not at all.
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if cid := ChunkIDFromBodyKey(e.Key); cid != "" {
			out = append(out, cid)
		}
	}
	return out
}

// graphSeedIDs is graphSeeds plus the two things the caller needs to report and
// to backfill with: HOW the seeds were found, and the full ranked list semantic
// seeding produced (of which only the first few actually seed the walk).
func (d *Document) graphSeedIDs(ctx context.Context, key sqlmem.ScopeKey, in docInput, limit int) ([]graphChunk, graphSeedInfo, error) {
	info := graphSeedInfo{How: "ids"}
	where := []string{}
	args := []any{}
	if len(in.SeedIDs) > 0 {
		where = append(where, "c.id IN ("+placeholders(len(in.SeedIDs))+")")
		for _, id := range in.SeedIDs {
			args = append(args, id)
		}
	} else if ranked := d.graphSemanticSeeds(ctx, key, in.Query, graphSeedTopK); len(ranked) > 0 {
		// SEMANTIC FIRST when a query was given and the store can answer it. The
		// title match below stays as the fallback, and as the only path when there
		// is no embedder — a recall that worked before must not start failing.
		info.How = "semantic"
		info.Ranked = ranked
		n := graphDefaultSeeds
		if in.Seeds > 0 {
			n = in.Seeds
		}
		if n > len(ranked) {
			n = len(ranked)
		}
		where = append(where, "c.id IN ("+placeholders(n)+")")
		for _, id := range ranked[:n] {
			args = append(args, id)
		}
	} else {
		info.How = "title"
		// Case-insensitive contains, as a COARSE PREFILTER only — the word-boundary
		// check happens in Go below, because portable SQL has no word boundary and
		// faking one with padded LIKE breaks on punctuation. LOWER on both sides
		// rather than ILIKE, which is postgres-only.
		where = append(where, "LOWER(c.title) LIKE ?")
		args = append(args, "%"+strings.ToLower(strings.TrimSpace(in.Query))+"%")
		// DISCOVERY IS RESTRICTED TO ENTITY CHUNKS: a row with no sidecar entry is
		// an ordinary document chunk, not part of the graph.
		//
		// Without this the search covers every chunk in the scope, and the scope is
		// where the document store lives. Measured on the reference deployment: 2
		// entity chunks among 3,071 chunks across 150 documents — so a
		// "what do we know about X" query returned documentation prose, and a query
		// for "statin" matched a configuration paragraph containing the word
		// "restating". Correct results drowned rather than absent, which is the one
		// failure mode a fixture corpus can never show: every test corpus is a clean
		// room, and signal-to-noise is a property of the corpus.
		//
		// Restricted for DISCOVERY only, never for explicit seed_ids — the same
		// division the temporal filter draws below, and for the same reason: naming
		// a chunk is an assertion about where to start, and the documented use of
		// seed_ids is handing in results found some other way.
		where = append(where, "m.chunk_id IS NOT NULL")
	}
	if in.DocumentID != "" {
		where = append(where, "c.document_id = ?")
		args = append(args, in.DocumentID)
	}
	// The temporal filter governs what is DISCOVERED, not what the caller NAMED.
	//
	// Explicit seed_ids are an assertion about where to start, so they are returned
	// as asked. Filtering them made as_of unusable: an entity is typically timeless
	// while the FACTS about it have validity windows, so an entity created today has
	// a valid_at of today and "as of June, what did we know about Ada?" excluded Ada
	// herself and returned nothing at all. Filtering the wrong noun.
	//
	// A `query` seed IS discovery, so it is filtered like the expansion.
	if len(in.SeedIDs) == 0 {
		temporal, targs := graphTemporalClause(in)
		if temporal != "" {
			where = append(where, temporal)
			args = append(args, targs...)
		}
	}
	// Applied whether the seeds were discovered or supplied: handing in an id must not
	// smuggle a refuted fact into a walk. No placeholders, so it does not disturb the
	// argument ordering the union branches depend on.
	if w := graphWithholdClause(in); w != "" {
		where = append(where, w)
	}

	// OVER-FETCH when discovering, because the word-boundary filter below rejects
	// some of what the LIKE returned. Fetching exactly `limit` and then filtering
	// would silently return fewer seeds than asked for, which reads as "the graph
	// holds nothing else" rather than "the prefilter was noisy". Bounded by
	// graphFrontierCap so a pathological query cannot pull the whole scope.
	fetch := limit
	if len(in.SeedIDs) == 0 {
		if fetch = limit * seedPrefilterFactor; fetch > graphFrontierCap {
			fetch = graphFrontierCap
		}
	} else {
		// EXPLICIT IDS ARE FETCHED IN FULL. `limit` used to bound this query too, so
		// handing in 60 ids with the default limit of 50 silently walked from 50 of
		// them — accepted, dropped, and indistinguishable from "the graph holds
		// nothing else". Naming a chunk is an assertion about where to START; what
		// `limit` bounds is what comes BACK, and the budget/limit pass already applies
		// it to the result.
		fetch = len(in.SeedIDs)
	}
	stmt := `SELECT c.id, c.title, c.type, c.status, m.valid_at, m.invalid_at
	           FROM chunks c LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
	          WHERE ` + strings.Join(where, " AND ") + `
	          ORDER BY c.title LIMIT ?`
	args = append(args, fetch)
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return nil, info, err
	}
	rows := scanGraphRows(res.Rows, 0, "", "")
	if info.How != "title" {
		// Explicit ids and semantic seeds are both already the chosen set; the
		// whole-word filter exists to survive the LIKE prefilter and would drop a
		// semantically-ranked fact whose body does not contain the query's words,
		// which is the entire point of ranking it semantically.
		return rows, info, nil
	}
	return filterWholeWord(rows, in.Query, limit), info, nil
}

// seedPrefilterFactor is how many LIKE candidates to consider per requested seed.
// Small on purpose: the point is to survive an ordinary noisy prefilter, not to
// scan the scope hoping for a match further down.
const seedPrefilterFactor = 8

// filterWholeWord keeps the rows whose title contains the query as a WHOLE WORD,
// capped at limit.
//
// A substring LIKE is what let a query for "statin" match a configuration
// paragraph containing "re-statin-g" on the reference deployment. The check lives
// in Go rather than SQL because neither tier has a portable word boundary and the
// padded-LIKE trick misfires on punctuation — a title ending in ":" or a
// hyphenated term would stop matching.
//
// A query with no word character at either edge (say "(" or "--") falls back to
// the substring behaviour instead of matching nothing:  would be undefined there,
// and silently returning zero seeds for a legitimate-if-odd query is worse than
// being imprecise about it.
func filterWholeWord(rows []graphChunk, query string, limit int) []graphChunk {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(q) + `\b`)
	if err != nil || !hasWordEdges(q) {
		if len(rows) > limit {
			return rows[:limit]
		}
		return rows
	}
	out := make([]graphChunk, 0, len(rows))
	for _, r := range rows {
		if re.MatchString(r.Title) {
			out = append(out, r)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

// hasWordEdges reports whether \b is meaningful at both ends of q.
func hasWordEdges(q string) bool {
	isWord := func(b byte) bool {
		return b == '_' || (b >= '0' && b <= '9') ||
			(b|0x20 >= 'a' && b|0x20 <= 'z')
	}
	return len(q) > 0 && isWord(q[0]) && isWord(q[len(q)-1])
}

// graphNeighbours returns the chunks one edge away from the frontier, in EITHER
// direction. Both directions matter: an entity is as related to the fact that
// points at it as to the ones it points at, and a forward-only walk would answer
// half the question. The reverse leg is what PR 1's chunk_edges(to_id, kind) index
// exists for.
// Returns the rows, and whether the hop hit rowCap — a truncated hop is reported
// rather than silently returning a smaller graph than the one that exists.
//
// rowCap is a PARAMETER rather than a reach for graphHopRowCap, so the bound is
// visible at the call site and a test can exercise it without building a fixture
// the size of the production cap. That is not a hypothetical: the first version of
// this test needed 2001 chunks and 2001 edges and cost 95s under -race, on a
// package that already runs close to its CI timeout.
func (d *Document) graphNeighbours(ctx context.Context, key sqlmem.ScopeKey, frontier []string, hop int, in docInput, rowCap int) ([]graphChunk, bool, error) {
	marks := placeholders(len(frontier))
	temporal, targs := graphTemporalClause(in)
	extra := ""
	if temporal != "" {
		extra = " AND " + temporal
	}
	// Argument-free by construction, so it can be concatenated into `extra` without
	// touching the placeholder ordering documented below.
	if w := graphWithholdClause(in); w != "" {
		extra += " AND " + w
	}

	// Argument order must follow the statement's PLACEHOLDER order, and the
	// temporal clause appears in BOTH union branches — so the sequence is
	// frontier, temporal, frontier, temporal, not both frontiers followed by one
	// temporal. Getting that wrong surfaced as "missing argument with index 5"
	// rather than as wrong rows, which is the good failure of the two.
	args := make([]any, 0, (len(frontier)+len(targs))*2)
	for _, id := range frontier {
		args = append(args, id)
	}
	args = append(args, targs...)
	for _, id := range frontier {
		args = append(args, id)
	}
	args = append(args, targs...)

	// One statement per hop, both directions unioned. `via` carries the edge kind
	// and the neighbour it was reached from, so the caller can see the shape of the
	// association rather than just its endpoint.
	stmt := `SELECT c.id, c.title, c.type, c.status, m.valid_at, m.invalid_at, e.kind, e.from_id
	           FROM chunk_edges e
	           JOIN chunks c ON c.id = e.to_id
	           LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
	          WHERE e.from_id IN (` + marks + `)` + extra + `
	         UNION
	         SELECT c.id, c.title, c.type, c.status, m.valid_at, m.invalid_at, e.kind, e.to_id
	           FROM chunk_edges e
	           JOIN chunks c ON c.id = e.from_id
	           LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
	          WHERE e.to_id IN (` + marks + `)` + extra + `
	         LIMIT ?`
	// One over the cap, so the caller can tell "exactly full" from "there was more"
	// without a second query. Trimmed back to the cap below.
	args = append(args, rowCap+1)
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return nil, false, err
	}
	over := len(res.Rows) > rowCap
	if over {
		res.Rows = res.Rows[:rowCap]
	}
	out := make([]graphChunk, 0, len(res.Rows))
	for _, r := range res.Rows {
		c := scanGraphRow(r)
		c.Hop = hop
		if len(r) > 6 {
			c.ViaKind = asStr(r[6])
		}
		if len(r) > 7 {
			c.ViaID = asStr(r[7])
		}
		out = append(out, c)
	}
	return out, over, nil
}

// graphTemporalClause builds the time filter.
//
// The DEFAULT is "what is true now": a superseded fact is excluded, because a
// recall that returned both a fact and its correction with no way to tell them
// apart is worse than one that returned neither.
//
// as_of asks the other question — what was true THEN — and needs both halves of
// the interval: the fact must have become true at or before that moment, and must
// not have stopped being true before it. Checking only invalid_at would return
// facts the system had not yet learned.
//
// A chunk with NO sidecar row (an ordinary chunk, or one predating the entity
// tier) is always current: it has no temporal claim to violate, and excluding it
// would make graph_recall blind to the rest of the document.
func graphTemporalClause(in docInput) (string, []any) {
	if in.IncludeRetired {
		return "", nil
	}
	if in.AsOf != nil {
		return `(m.chunk_id IS NULL OR ((m.valid_at IS NULL OR m.valid_at <= ?) AND (m.invalid_at IS NULL OR m.invalid_at > ?)))`,
			[]any{*in.AsOf, *in.AsOf}
	}
	// invalid_at > now, not merely IS NULL. A fact may carry a KNOWN FUTURE end —
	// "the contract runs until 2027" — and such a fact is true right now. Treating
	// any end-date as "not current" made recording one an act of immediate
	// deletion: the fact vanished from every default recall the moment it was
	// written.
	//
	// The decisive argument is internal consistency rather than taste. The default
	// IS "as_of now", so it must reduce to the as_of predicate above with the
	// current time substituted. It did not: a caller passing as_of=<now> got a
	// different answer than the same caller passing nothing.
	return `(m.chunk_id IS NULL OR m.invalid_at IS NULL OR m.invalid_at > ?)`,
		[]any{time.Now().UnixNano()}
}

// graphWithholdClause keeps a refuted fact out of a graph walk.
//
// Beside the temporal clause and for the same reason it treats a sidecar-less chunk as
// current: a chunk with no entity row has no verdict to fail, and excluding it would make
// graph_recall blind to the rest of the document.
//
// Keyed on include_refuted ALONE, deliberately, though include_retired sits right beside
// it and would have been easy to fold in. They are different axes: retired means a fact
// was corrected by a later one, refuted means it was checked and failed. A caller reading
// history wants the first without the second, and coupling them would leave no way to ask
// for it — as well as making two flags that can disagree about which facts exist.
func graphWithholdClause(in docInput) string {
	return withholdClause("m.confidence", in.IncludeRefuted)
}

func scanGraphRows(rows [][]any, hop int, viaKind, viaID string) []graphChunk {
	out := make([]graphChunk, 0, len(rows))
	for _, r := range rows {
		c := scanGraphRow(r)
		c.Hop, c.ViaKind, c.ViaID = hop, viaKind, viaID
		out = append(out, c)
	}
	return out
}

func scanGraphRow(r []any) graphChunk {
	c := graphChunk{ID: asStr(r[0]), Title: asStr(r[1])}
	if len(r) > 2 {
		c.Type = asStr(r[2])
	}
	if len(r) > 3 {
		c.Status = asStr(r[3])
	}
	if len(r) > 4 {
		if v, ok := asInt64(r[4]); ok {
			c.ValidAt = &v
		}
	}
	if len(r) > 5 {
		if v, ok := asInt64(r[5]); ok {
			c.Invalid = &v
			// HAS an end date != HAS ended. A fact valid until 2027 is current, and
			// labelling it retired told the model the opposite of the truth about
			// something it was simultaneously being shown as a result.
			c.Retired = v <= time.Now().UnixNano()
		}
	}
	return c
}

// asInt64 reads a nullable timestamp cell. It reports presence SEPARATELY from
// value, unlike asInt, because a NULL and a real 0 mean different things here: NULL
// is "no temporal claim", 0 is the unix epoch. Collapsing them would make a chunk
// with no sidecar row look like one valid since 1970.
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	}
	return 0, false
}

// placeholders renders n `?` marks. Rebind converts them per dialect at the
// d.query boundary.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
