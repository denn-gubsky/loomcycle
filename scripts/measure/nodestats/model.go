package main

import (
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
)

// Doc is one live document as the measurement sees it.
type Doc struct {
	ID        string
	Title     string
	RootChunk string
	// CreatedAt, RootValidAt: unix nanoseconds; RootValidAt 0 when unset.
	CreatedAt   int64
	RootValidAt int64
	// SourceUpdatedAt is the root chunk's import provenance source_updated_at
	// (RFC 3339), "" when absent.
	SourceUpdatedAt string

	// DocVec is the document-level annotation vector, nil when there is none;
	// DocUnit its normalised form.
	DocVec  nodestats.Vector
	DocUnit []float64
	// Sections are the branch-level annotation vectors of its non-root chunks.
	Sections []nodestats.Vector
	SecUnits [][]float64

	// The document-level annotation, when there is one.
	HasAnnotation bool
	PromptVersion int
	Brief         string
	Abstract      string

	// Ontology marks a document playing the ontology role: never content.
	Ontology bool
}

// Statement is one live statement from a document into a concept of the scheme.
type Statement struct {
	ID         string
	DocID      string
	ConceptID  string
	Kind       string
	Provenance string
}

// Data is everything one run reads.
type Data struct {
	Docs       map[string]*Doc
	Concepts   []Concept
	Statements []Statement
	// Notes are facts about the data found while loading, for the report.
	Notes []string
	// Counts of what was read and skipped, for the data-quality section.
	Counts map[string]int
	// NotLiveDocs are the documents with a statement into the scheme that is
	// not live (proposed, rejected, withheld or retired).
	NotLiveDocs map[string]bool
}

// Placed maps each document with at least one statement to its declared
// concepts (distinct, in statement order).
func (d *Data) Placed() map[string][]string {
	out := map[string][]string{}
	seen := map[[2]string]bool{}
	for _, s := range d.Statements {
		k := [2]string{s.DocID, s.ConceptID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out[s.DocID] = append(out[s.DocID], s.ConceptID)
	}
	return out
}
