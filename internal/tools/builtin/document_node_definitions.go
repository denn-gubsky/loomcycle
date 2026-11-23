package builtin

// document_node_definitions.go — concept definition vectors (RFC GM P3.3,
// "candidates"): each concept's definition — its place in the scheme, its name,
// its meaning line and its alternative labels — embedded once and kept, so a
// document can be compared with every concept, placed or not.
//
// WHERE THEY LIVE. In the node_definitions table beside node_stats, not as Memory
// embedding rows beside the annotation index:
//   - a definition's freshness is a hash comparison, which SQL answers in one read
//     (def_sha256), while a Memory row carries no hash a bulk read returns;
//   - the reader is in-process and wants every concept's vector at once, as the
//     node_stats centroid (BLOB/BYTEA, float32 little-endian) is read: no search
//     index is needed for hundreds to low thousands of concepts, and none is
//     added (a definition must never rank in a document search);
//   - the vectors stay where node_stats' centroids stay: returned by no op.
//
// WHEN. node_stats mode compute refreshes them, on a store whose vector stage runs
// (Postgres with pgvector, and an embedder): only a definition whose hash moved —
// its text or the embed model — is embedded again. The text is the concept's own
// definition and nothing else, so an edit elsewhere in the ontology re-embeds
// nothing. On a store without vectors nothing is kept and candidates fall back to
// classify's tree walk.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

const (
	// nodeDefinitionVersion is the version of the definition text's form: a change
	// to nodeDefinitionText bumps it, which re-embeds every definition on purpose.
	nodeDefinitionVersion = "node_definition/1"
	// nodeDefinitionBatch is the definitions per embed call; nodeDefinitionMax the
	// most one pass embeds (the rest are embedded by the next pass, and counted).
	nodeDefinitionBatch = 64
	nodeDefinitionMax   = 1000
)

// nodeDefinitionsDDL is built per tier, like node_stats: vector is float32
// little-endian, BLOB on sqlite and BYTEA on postgres.
func nodeDefinitionsDDL(binType string) string {
	return `CREATE TABLE IF NOT EXISTS node_definitions (
		node_id TEXT PRIMARY KEY, def_sha256 TEXT NOT NULL, embed_model TEXT NOT NULL,
		vector ` + binType + ` NOT NULL, computed_at BIGINT NOT NULL)`
}

// nodeDefinitionText is what is embedded for concept c of scheme: the header names
// the place, as an annotation's index text does, so the definition and a document
// annotation are embedded in the same shape.
func nodeDefinitionText(scheme string, c memrank.OntologyConcept) string {
	path := c.Path
	if len(path) == 0 {
		path = []string{c.Name}
	}
	var b strings.Builder
	b.WriteString("[concept · " + scheme + "] " + strings.Join(path, " › "))
	if m := strings.TrimSpace(c.Meaning); m != "" {
		b.WriteString("\n" + m)
	}
	if len(c.Alt) > 0 {
		b.WriteString("\nAlso: " + strings.Join(c.Alt, "; "))
	}
	return b.String()
}

// nodeDefinitionHash is the stored def_sha256 of text under model.
func nodeDefinitionHash(model, text string) string {
	sum := sha256.Sum256([]byte(nodeDefinitionVersion + "\n" + model + "\n" + text))
	return hex.EncodeToString(sum[:])
}

// nodeDefinition is one concept's definition as a pass sees it.
type nodeDefinition struct {
	id, text, hash string
}

// nodeDefinitionsOf is every concept of read's schemes (scheme narrows it) with
// its definition text and hash under model. A concept with no chunk is left out.
func nodeDefinitionsOf(read OntologyRead, scheme, model string) []nodeDefinition {
	var out []nodeDefinition
	if !read.Confirmed {
		return nil
	}
	seen := map[string]bool{}
	for _, s := range read.Schemes {
		if scheme != "" && s.Name != scheme {
			continue
		}
		for _, c := range s.Concepts {
			if c.ChunkID == "" || seen[c.ChunkID] {
				continue
			}
			seen[c.ChunkID] = true
			text := nodeDefinitionText(s.Name, c)
			out = append(out, nodeDefinition{id: c.ChunkID, text: text, hash: nodeDefinitionHash(model, text)})
		}
	}
	return out
}

// loadNodeDefinitionHashes is every stored row's def_sha256, by node id.
func (d *Document) loadNodeDefinitionHashes(ctx context.Context, key sqlmem.ScopeKey) (map[string]string, error) {
	res, err := d.query(ctx, key, `SELECT node_id, def_sha256 FROM node_definitions`)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("node_definitions: the stored rows were cut short")
	}
	out := make(map[string]string, len(res.Rows))
	for _, r := range res.Rows {
		out[asStr(r[0])] = asStr(r[1])
	}
	return out, nil
}

// loadNodeDefinitionVectors is the stored vector of each of defs whose row is
// current: of model and of the definition as it reads now. A row of an older
// definition is no vector, so a renamed concept is never matched by its old name.
func (d *Document) loadNodeDefinitionVectors(ctx context.Context, key sqlmem.ScopeKey, defs []nodeDefinition, model string) (map[string]nodestats.Vector, error) {
	want := make(map[string]string, len(defs))
	for _, def := range defs {
		want[def.id] = def.hash
	}
	res, err := d.query(ctx, key, `SELECT node_id, def_sha256, embed_model, vector FROM node_definitions`)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("node_definitions: the stored rows were cut short")
	}
	out := map[string]nodestats.Vector{}
	for _, r := range res.Rows {
		id := asStr(r[0])
		if h, ok := want[id]; !ok || h != asStr(r[1]) || asStr(r[2]) != model {
			continue
		}
		b := asBytes(r[3])
		if len(b) == 0 || len(b)%4 != 0 {
			continue
		}
		v, err := projection.DecodeFloat32s(b, len(b)/4)
		if err != nil {
			continue
		}
		out[id] = nodestats.Vector(v)
	}
	return out, nil
}

// NodeDefinitionsReport is the definitions stage of a compute pass.
type NodeDefinitionsReport struct {
	// Fresh: definitions whose stored vector is current. Embedded: written by this
	// pass. Pending: due but past this pass's bound, left for the next. Removed:
	// rows of concepts no longer in force (only without scheme).
	Fresh    int `json:"fresh"`
	Embedded int `json:"embedded"`
	Pending  int `json:"pending,omitempty"`
	Removed  int `json:"removed,omitempty"`
	// Error: the embedder failed; what was embedded before it is kept.
	Error string `json:"error,omitempty"`
}

// refreshNodeDefinitions embeds the definitions whose hash moved and writes them,
// removing the rows of concepts no longer in force when scheme is empty. The
// caller runs it only where the vector stage runs (model set). Every read is made
// before the write transaction (SQLite has one connection per scope).
func (d *Document) refreshNodeDefinitions(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme, model string) (*NodeDefinitionsReport, error) {
	defs := nodeDefinitionsOf(read, scheme, model)
	stored, err := d.loadNodeDefinitionHashes(ctx, key)
	if err != nil {
		return nil, err
	}
	rep := &NodeDefinitionsReport{}
	inForce := map[string]bool{}
	var due []nodeDefinition
	for _, def := range defs {
		inForce[def.id] = true
		if stored[def.id] == def.hash {
			rep.Fresh++
			continue
		}
		due = append(due, def)
	}
	var orphans []string
	if scheme == "" {
		for id := range stored {
			if !inForce[id] {
				orphans = append(orphans, id)
			}
		}
	}
	if len(due) > nodeDefinitionMax {
		rep.Pending, due = len(due)-nodeDefinitionMax, due[:nodeDefinitionMax]
	}
	type embedded struct {
		def nodeDefinition
		vec []float32
	}
	var rows []embedded
	for start := 0; start < len(due); start += nodeDefinitionBatch {
		batch := due[start:min(start+nodeDefinitionBatch, len(due))]
		texts := make([]string, len(batch))
		for i, def := range batch {
			texts[i] = def.text
		}
		vecs, err := d.Embedder.Embed(ctx, texts)
		if err == nil && len(vecs) != len(batch) {
			err = fmt.Errorf("the embedder returned %d vectors for %d texts", len(vecs), len(batch))
		}
		if err != nil {
			rep.Error = err.Error()
			rep.Pending += len(due) - start
			break
		}
		for i, def := range batch {
			if len(vecs[i]) == 0 {
				rep.Pending++
				continue
			}
			rows = append(rows, embedded{def: def, vec: vecs[i]})
		}
	}
	if len(rows) == 0 && len(orphans) == 0 {
		return rep, nil
	}
	now := time.Now().UnixNano()
	err = d.withSqlTxn(ctx, key, func(txnID string) error {
		for _, r := range rows {
			if err := d.execTxn(ctx, txnID, `INSERT INTO node_definitions (node_id, def_sha256, embed_model, vector, computed_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (node_id) DO UPDATE SET def_sha256 = excluded.def_sha256, embed_model = excluded.embed_model,
				vector = excluded.vector, computed_at = excluded.computed_at`,
				r.def.id, r.def.hash, model, projection.EncodeFloat32s(r.vec), now); err != nil {
				return fmt.Errorf("node_definitions %s: %w", r.def.id, err)
			}
		}
		for _, id := range orphans {
			if err := d.execTxn(ctx, txnID, `DELETE FROM node_definitions WHERE node_id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rep.Embedded, rep.Removed = len(rows), len(orphans)
	return rep, nil
}
