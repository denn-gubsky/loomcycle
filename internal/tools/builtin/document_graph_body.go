package builtin

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// graphBodies hands graph_recall rows their body excerpts.
//
// Bodies live in the Memory plane (doc.chunk:<id>), not in the SQL rows the walk
// reads, and the store has no multi-get. So they are read concurrently, a window
// at a time, and only for rows about to be considered for the answer: the walk can
// reach thousands of chunks, and reading every one to return fifty would make a
// bounded walk unbounded in store calls.
type graphBodies struct {
	d      *Document
	key    sqlmem.ScopeKey
	mscope store.MemoryScope
	max    int  // excerpt length in characters; 0 = no bodies
	all    bool // facts too, not only ordinary chunks
	got    map[string]string
}

// newGraphBodies reads body_chars: nil is the default (an excerpt on non-fact
// rows), 0 turns bodies off, N puts an N-character excerpt on every row.
func newGraphBodies(d *Document, key sqlmem.ScopeKey, bodyChars *int) *graphBodies {
	g := &graphBodies{d: d, key: key, mscope: store.MemoryScope(key.Scope), max: graphDefaultBodyChars, got: map[string]string{}}
	if bodyChars != nil {
		g.max, g.all = *bodyChars, true
	}
	return g
}

// wants reports whether a row gets a body. A fact stays title-only by default:
// its title IS the claim, and the measured traversal behaviour was measured on
// titles, so the default must not change what a fact walk spends its budget on.
func (g *graphBodies) wants(c graphChunk) bool {
	return g.max > 0 && (g.all || !c.fact)
}

// prefetch makes sure the body of ids[from] is read, reading it together with up
// to n-1 more unread bodies further down ids — the rows the caller considers
// next. n is how many rows the answer still has room for, so a window never reads
// further ahead than the answer could reach.
func (g *graphBodies) prefetch(ctx context.Context, ids []string, from, n int, rows map[string]graphChunk) {
	if g.max == 0 || from >= len(ids) {
		return
	}
	if _, done := g.got[ids[from]]; done || !g.wants(rows[ids[from]]) {
		return
	}
	if n < 1 {
		n = 1
	}
	window := make([]string, 0, n)
	for _, id := range ids[from:] {
		if len(window) == n {
			break
		}
		if r, ok := rows[id]; ok && g.wants(r) {
			if _, done := g.got[id]; done {
				continue
			}
			window = append(window, id)
		}
	}
	read := make([]string, len(window))
	sem := make(chan struct{}, graphBodyReadParallel)
	var wg sync.WaitGroup
	for i, id := range window {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			cb, err := g.d.readBody(ctx, g.mscope, g.key.ScopeID, id)
			if err != nil {
				// Omitted, never an error: a recall that loses one excerpt is still the
				// recall the caller asked for, and the title is still there.
				slog.DebugContext(ctx, "graph_recall: chunk body unreadable", "chunk_id", id, "err", err)
				return
			}
			read[i] = cb.Body
		}(i, id)
	}
	wg.Wait()
	for i, id := range window {
		g.got[id] = read[i] // "" marks read-and-empty, so it is not read again
	}
}

// attach puts the row's excerpt on it, when the row wants one and it was read.
func (g *graphBodies) attach(c *graphChunk) {
	if !g.wants(*c) {
		return
	}
	c.Body, c.BodyTruncated = bodyExcerpt(g.got[c.ID], g.max)
}

// bodyExcerpt cuts s to at most max characters (runes), backing off to the last
// space when the cut lands inside a word, and reports whether it cut. It does not
// back off past half the excerpt: one long token (a URL, a table row) is better
// cut mid-word than reduced to a few characters.
func bodyExcerpt(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	cut := truncateRunes(s, max)
	if next, _ := utf8.DecodeRuneInString(s[len(cut):]); !unicode.IsSpace(next) {
		if i := strings.LastIndexFunc(cut, unicode.IsSpace); i > len(cut)/2 {
			cut = cut[:i]
		}
	}
	return strings.TrimRightFunc(cut, unicode.IsSpace), true
}
