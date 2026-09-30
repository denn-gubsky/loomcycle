package builtin

// document_units.go — Document derived search units (RFC DM Design C): the storage
// half. A unit is an index-only memory row about a chunk (see memory/units.go for
// why it is shaped the way it is). This file writes them, keeps their index text
// true as the tree changes, and deletes them with their chunk. Generating them — a
// model call per chunk — is a separate operator pass.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DerivedUnit is one unit to store for a chunk.
type DerivedUnit struct {
	Kind string // memrank.UnitDescription | UnitClaim | UnitQuestion
	Text string
}

// Bounds on what one chunk may carry. The design asks for a description and 3–6
// claims and questions each; the ceilings leave room without letting one chunk
// flood the shared search pool.
const (
	maxUnitsPerChunk = 24
	maxUnitChars     = 1000
)

// ReplaceUnits replaces every derived search unit of one chunk with units, each
// indexed under the chunk's header, and returns how many it wrote. src is recorded
// on each unit — the hash of the body the units were written from is how a later
// pass finds the stale ones. An empty units list removes the chunk's units.
//
// It refuses a chunk that does not exist in the scope: a unit votes for its chunk,
// and a unit with no chunk is an orphan the moment it is written.
// UnitSource records what a chunk's units were written from.
type UnitSource struct {
	Model        string
	BodyRevision int
	BodySHA256   string
}

func (d *Document) ReplaceUnits(ctx context.Context, scope, chunkID string, units []DerivedUnit, src UnitSource) (int, error) {
	if d.Store == nil || d.SqlMem == nil {
		return 0, fmt.Errorf("derived units: SQL Memory is not configured")
	}
	if d.Embedder == nil && len(units) > 0 {
		return 0, fmt.Errorf("derived units: no embedder is configured, so a unit could not be found")
	}
	if len(units) > maxUnitsPerChunk {
		return 0, fmt.Errorf("derived units: %d units for one chunk, at most %d", len(units), maxUnitsPerChunk)
	}
	for _, u := range units {
		switch u.Kind {
		case memrank.UnitDescription, memrank.UnitClaim, memrank.UnitQuestion:
		default:
			return 0, fmt.Errorf("derived units: unknown kind %q (description, claim or question)", u.Kind)
		}
		if strings.TrimSpace(u.Text) == "" {
			return 0, fmt.Errorf("derived units: an empty %s", u.Kind)
		}
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return 0, err
	}
	// The chunk ROW, not its body: a body read of a missing chunk is just empty.
	res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE id = ?`, chunkID)
	if err != nil {
		return 0, fmt.Errorf("derived units: chunk %s: %w", chunkID, err)
	}
	if len(res.Rows) == 0 {
		return 0, fmt.Errorf("derived units: no chunk %s in this scope", chunkID)
	}
	tenant := direntTenant(ctx)
	d.deleteUnitsOf(ctx, tenant, mscope, key.ScopeID, chunkID)

	written := 0
	next := map[string]int{}
	for _, u := range units {
		text := truncateRunes(strings.TrimSpace(u.Text), maxUnitChars)
		n := next[u.Kind]
		next[u.Kind]++
		k := memrank.UnitKey(chunkID, u.Kind, n)
		v, _ := json.Marshal(memrank.UnitValue{ChunkID: chunkID, Kind: u.Kind, Text: text,
			Model: src.Model, BodyRevision: src.BodyRevision, BodySHA256: src.BodySHA256})
		if err := d.Store.MemorySet(ctx, tenant, mscope, key.ScopeID, k, v, 0); err != nil {
			return written, fmt.Errorf("derived units: write %s: %w", k, err)
		}
		if idx := d.unitIndexText(ctx, key, chunkID, text); idx != "" && !d.embedText(ctx, tenant, mscope, key.ScopeID, k, idx) {
			// Best-effort like every document embed: the row stands, unindexed, and
			// the re-index pass picks it up.
			log.Printf("document: unit %s written but not embedded", k)
		}
		written++
	}
	return written, nil
}

// unitIndexText is a unit's index text: its chunk's header, then the unit. Built
// from the same header chunkIndexText uses, so a unit and its chunk are found
// under the same document and section.
func (d *Document) unitIndexText(ctx context.Context, key sqlmem.ScopeKey, chunkID, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	titles, _, ok := d.chunkLineage(ctx, key, chunkID)
	if !ok {
		return text
	}
	if h := indexHeader(titles); h != "" {
		return h + "\n" + text
	}
	return text
}

// unitsOf lists every unit row of one chunk.
func (d *Document) unitsOf(ctx context.Context, tenant string, mscope store.MemoryScope, scopeID, chunkID string) []store.MemoryEntry {
	var out []store.MemoryEntry
	cursor := ""
	for {
		page, err := d.Store.MemoryListAfter(ctx, tenant, mscope, scopeID, memrank.UnitKeyPrefixFor(chunkID), cursor, reindexPage)
		if err != nil {
			log.Printf("document: list units of %s: %v", chunkID, err)
			return out
		}
		out = append(out, page...)
		if len(page) < reindexPage {
			return out
		}
		cursor = page[len(page)-1].Key
	}
}

// deleteUnitsOf deletes every unit of one chunk and returns how many went.
// Best-effort, like a chunk's body delete: a unit left behind is an orphan the
// dead-link sweeper reaps, and it never surfaces in a search meanwhile.
func (d *Document) deleteUnitsOf(ctx context.Context, tenant string, mscope store.MemoryScope, scopeID, chunkID string) int {
	n := 0
	for _, u := range d.unitsOf(ctx, tenant, mscope, scopeID, chunkID) {
		if removed, _ := d.Store.MemoryDelete(ctx, tenant, mscope, scopeID, u.Key); removed {
			n++
		}
	}
	return n
}

// reindexUnitsOf re-embeds one chunk's units under its current header — part of the
// subtree re-index a rename or a move triggers, since a unit's header is its chunk's.
func (d *Document) reindexUnitsOf(ctx context.Context, tenant string, mscope store.MemoryScope, key sqlmem.ScopeKey, chunkID string) {
	for _, u := range d.unitsOf(ctx, tenant, mscope, key.ScopeID, chunkID) {
		var v memrank.UnitValue
		_ = json.Unmarshal(u.Value, &v)
		if idx := d.unitIndexText(ctx, key, chunkID, v.Text); idx != "" &&
			!d.indexCurrent(ctx, tenant, mscope, key.ScopeID, u.Key, idx) {
			d.embedText(ctx, tenant, mscope, key.ScopeID, u.Key, idx)
		}
	}
}

// UnitsForChunk is what a chunk's units say, for a caller that shows them (the
// generation pass reports what it wrote). Ordered by key.
func (d *Document) UnitsForChunk(ctx context.Context, scope, chunkID string) ([]memrank.UnitValue, error) {
	if d.Store == nil || d.SqlMem == nil {
		return nil, fmt.Errorf("derived units: SQL Memory is not configured")
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	var out []memrank.UnitValue
	for _, u := range d.unitsOf(ctx, direntTenant(ctx), mscope, key.ScopeID, chunkID) {
		var v memrank.UnitValue
		if json.Unmarshal(u.Value, &v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

func truncateRunes(s string, max int) string {
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}
