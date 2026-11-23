package builtin

// document_concept_candidates.go — concept candidates for classify's concept step
// (RFC GM P3.3). A vocabulary too large for one model request is walked down its
// tree today (classifyConceptTree), one request per level and branch. Where the
// store keeps vectors, the document's own annotation vector (the unit is the whole
// document, D3) is compared instead with every concept's definition vector
// (document_node_definitions.go) and, where node_stats holds one, its centroid; the
// nearest ClassifyMaxConcepts concepts are then asked in ONE request.
//
// Falls back to the tree walk, unchanged, whenever the comparison cannot be made: a
// store without vectors, no embedder, a document with no document annotation vector
// of the embed model, or no definition or centroid vector stored yet. A small
// vocabulary (one request already) is asked whole as before: narrowing it could only
// lose concepts.
//
// The candidates are what the MODEL is asked; what classify files from its answer is
// a proposal exactly as before. Vectors leave this file as similarities only.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/projection"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// conceptCandidate is one concept the document was compared nearest to, with the
// similarity that put it on the list: to its definition, to its centroid, or both.
type conceptCandidate struct {
	Concept    string   `json:"concept"`
	Tag        string   `json:"tag"`
	ChunkID    string   `json:"chunk_id"`
	Definition *float64 `json:"definition,omitempty"`
	Centroid   *float64 `json:"centroid,omitempty"`

	c memrank.OntologyConcept
}

// docAnnotationVector is the document-level annotation vector of root under model,
// nil when there is none.
func (d *Document) docAnnotationVector(ctx context.Context, key sqlmem.ScopeKey, root, model string) (nodestats.Vector, error) {
	k := annotationKey(root, annotationLevelDocument)
	rows, err := d.Store.MemoryEmbedListVectors(ctx, direntTenant(ctx), store.MemoryScopeTenant,
		annotationMemScopeID(store.MemoryScopeTenant, key), k, "", 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0].Key != k || rowModel(rows[0]) != model {
		return nil, nil
	}
	return nodestats.Vector(rows[0].Vector), nil
}

// loadConceptCentroids is each concept's stored centroid of model, from rows of
// unrestrictedFamily only (RFC GM C8: the same figures every reader is served).
func (d *Document) loadConceptCentroids(ctx context.Context, key sqlmem.ScopeKey, model string) (map[string]nodestats.Vector, error) {
	res, err := d.query(ctx, key, `SELECT node_id, centroid, inputs_sha256 FROM node_stats
		WHERE kind = ? AND embed_model = ? AND centroid IS NOT NULL`, nodeKindConcept, model)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("node_stats: the centroids were cut short")
	}
	out := map[string]nodestats.Vector{}
	for _, r := range res.Rows {
		if !strings.HasPrefix(asStr(r[2]), unrestrictedFamily) {
			continue
		}
		b := asBytes(r[1])
		if len(b) == 0 || len(b)%4 != 0 {
			continue
		}
		if v, err := projection.DecodeFloat32s(b, len(b)/4); err == nil {
			out[asStr(r[0])] = nodestats.Vector(v)
		}
	}
	return out, nil
}

// cosine32 is the cosine similarity of a and b; ok is false when it is undefined
// (another dimension, a zero or non-finite vector).
func cosine32(a, b nodestats.Vector) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var ab, aa, bb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		ab, aa, bb = ab+x*y, aa+x*x, bb+y*y
	}
	if aa == 0 || bb == 0 {
		return 0, false
	}
	s := ab / math.Sqrt(aa*bb)
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return 0, false
	}
	return s, true
}

// conceptCandidates is the nearest concepts of read (already narrowed to what a new
// placement may use) to the document root's annotation vector, at most
// memrank.ClassifyMaxConcepts: the nearest by definition and the nearest by centroid,
// taken in turn, each concept once. Nil — the caller walks the tree — when no
// comparison can be made. A store fault is returned; the caller falls back too.
func (d *Document) conceptCandidates(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, root string) ([]conceptCandidate, error) {
	if d.nodeStatsVectorsNote() != "" || root == "" {
		return nil, nil
	}
	model := d.projectionModel()
	doc, err := d.docAnnotationVector(ctx, key, root, model)
	if err != nil || doc == nil {
		return nil, err
	}
	byID := map[string]memrank.OntologyConcept{}
	for _, c := range inForceConcepts(read) {
		if c.ChunkID != "" {
			byID[c.ChunkID] = c
		}
	}
	defVecs, err := d.loadNodeDefinitionVectors(ctx, key, nodeDefinitionsOf(read, "", model), model)
	if err != nil {
		return nil, err
	}
	cenVecs, err := d.loadConceptCentroids(ctx, key, model)
	if err != nil {
		return nil, err
	}
	rank := func(vecs map[string]nodestats.Vector) []nodestats.Scored {
		var out []nodestats.Scored
		for id, v := range vecs {
			if _, ok := byID[id]; !ok {
				continue
			}
			if s, ok := cosine32(doc, v); ok {
				out = append(out, nodestats.Scored{ID: id, Score: s})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Score != out[j].Score {
				return out[i].Score > out[j].Score
			}
			return out[i].ID < out[j].ID
		})
		return out
	}
	byDef, byCen := rank(defVecs), rank(cenVecs)
	if len(byDef) == 0 && len(byCen) == 0 {
		return nil, nil
	}
	defScore, cenScore := map[string]float64{}, map[string]float64{}
	for _, s := range byDef {
		defScore[s.ID] = s.Score
	}
	for _, s := range byCen {
		cenScore[s.ID] = s.Score
	}
	var out []conceptCandidate
	seen := map[string]bool{}
	take := func(id string) {
		if seen[id] || len(out) >= memrank.ClassifyMaxConcepts {
			return
		}
		seen[id] = true
		c := byID[id]
		cand := conceptCandidate{Concept: c.Name, Tag: c.Tag, ChunkID: id, c: c}
		if s, ok := defScore[id]; ok {
			cand.Definition = &s
		}
		if s, ok := cenScore[id]; ok {
			cand.Centroid = &s
		}
		out = append(out, cand)
	}
	for i := 0; i < max(len(byDef), len(byCen)) && len(out) < memrank.ClassifyMaxConcepts; i++ {
		if i < len(byDef) {
			take(byDef[i].ID)
		}
		if i < len(byCen) {
			take(byCen[i].ID)
		}
	}
	return out, nil
}

// classifyConceptCandidates asks memory.classifier about the candidates in one
// request and keeps the most specific concepts said yes: a concept said yes with a
// narrower one also said yes is left out, as the tree walk keeps the child.
func (d *Document) classifyConceptCandidates(ctx context.Context, cd classifyDoc) ([]classifyConcept, classifyStep) {
	step := classifyStep{Step: "model", Candidates: []classifyCandidate{}, Nearest: cd.nearest}
	concepts := make([]memrank.OntologyConcept, len(cd.nearest))
	for i, c := range cd.nearest {
		concepts[i] = c.c
	}
	fallback := map[string]bool{}
	yes, calls, reason := d.askConceptBatches(ctx, cd, concepts, 1, &step.Method, fallback)
	step.Reason = fmt.Sprintf("asked the %d concepts nearest the document's annotation by definition and centroid: %d call", len(concepts), calls)
	if reason != "" {
		step.Reason += "; " + reason
	}
	var out []classifyConcept
	for _, c := range yes {
		narrower := false
		for _, o := range yes {
			if o.Scheme == c.Scheme && len(o.Path) > len(c.Path) && strings.Join(o.Path[:len(c.Path)], "\x00") == strings.Join(c.Path, "\x00") {
				narrower = true
				break
			}
		}
		if !narrower {
			out = append(out, classifyConcept{Concept: c.Name, Tag: c.Tag, Kind: "subject", ChunkID: c.ChunkID, Fallback: fallback[conceptKey(c)]})
		}
	}
	return out, step
}
