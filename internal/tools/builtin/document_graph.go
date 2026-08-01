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
// chunk_edges, one round trip per hop, capped at two hops. Two is not arbitrary:
// each hop multiplies the frontier by the average degree, and past two the result
// stops being "related to what you asked" and becomes "most of the graph".

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	graphMaxHops      = 2
	graphDefaultHops  = 1
	graphDefaultLimit = 50
	graphMaxLimit     = 200
	// graphFrontierCap bounds ONE hop's frontier. Without it a single
	// heavily-connected entity turns hop 2 into a scan of the scope's edges, and
	// the caller sees a slow recall rather than a truncated one.
	graphFrontierCap = 500
)

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
		return errResult(fmt.Sprintf("graph_recall: hops must be 0..%d (got %d) — past two hops a result stops describing what you asked about", graphMaxHops, hops)), nil
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

	seeds, err := d.graphSeeds(ctx, key, in, limit)
	if err != nil {
		return errResult("graph_recall: seeds: " + err.Error()), nil
	}
	if len(seeds) == 0 {
		return okJSON(map[string]any{"chunks": []graphChunk{}, "seeds": 0, "hops": hops, "truncated": false})
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
		next, nerr := d.graphNeighbours(ctx, key, frontier, hop, in)
		if nerr != nil {
			return errResult("graph_recall: hop " + fmt.Sprint(hop) + ": " + nerr.Error()), nil
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

	out := make([]graphChunk, 0, len(order))
	for _, id := range order {
		if len(out) >= limit {
			truncated = true
			break
		}
		out = append(out, seen[id])
	}
	return okJSON(map[string]any{
		"chunks": out, "seeds": len(seeds), "hops": hops, "truncated": truncated,
	})
}

// graphSeeds resolves the starting set: explicit ids when given, else a title
// match. Both go through the same temporal filter as the expansion, so a
// superseded fact cannot enter as a seed while being excluded as a neighbour.
func (d *Document) graphSeeds(ctx context.Context, key sqlmem.ScopeKey, in docInput, limit int) ([]graphChunk, error) {
	where := []string{}
	args := []any{}
	if len(in.SeedIDs) > 0 {
		where = append(where, "c.id IN ("+placeholders(len(in.SeedIDs))+")")
		for _, id := range in.SeedIDs {
			args = append(args, id)
		}
	} else {
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
	}
	stmt := `SELECT c.id, c.title, c.type, c.status, m.valid_at, m.invalid_at
	           FROM chunks c LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
	          WHERE ` + strings.Join(where, " AND ") + `
	          ORDER BY c.title LIMIT ?`
	args = append(args, fetch)
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return nil, err
	}
	rows := scanGraphRows(res.Rows, 0, "", "")
	if len(in.SeedIDs) > 0 {
		return rows, nil
	}
	return filterWholeWord(rows, in.Query, limit), nil
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
func (d *Document) graphNeighbours(ctx context.Context, key sqlmem.ScopeKey, frontier []string, hop int, in docInput) ([]graphChunk, error) {
	marks := placeholders(len(frontier))
	temporal, targs := graphTemporalClause(in)
	extra := ""
	if temporal != "" {
		extra = " AND " + temporal
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
	          WHERE e.to_id IN (` + marks + `)` + extra
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return nil, err
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
	return out, nil
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
	return `(m.chunk_id IS NULL OR m.invalid_at IS NULL)`, nil
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
			c.Retired = true
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
