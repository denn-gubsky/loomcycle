package builtin

import (
	"context"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// classifyTreeMaxDepth bounds the walk down a scheme. A concept tree deeper than this
// is unusual; the bound only keeps a malformed one from asking without end.
const classifyTreeMaxDepth = 8

// classifyTreeMaxCalls bounds the model calls one classify makes walking the tree. Each
// concept is asked at most once, so the walk already costs at most the vocabulary's
// size over ClassifyMaxConcepts (plus one per level); this caps it outright, so a very
// large vocabulary costs a known number of calls and says what it left unasked.
const classifyTreeMaxCalls = 12

// conceptKey identifies a concept across schemes: its scheme and its path. A name alone
// is not enough, since two schemes (or two branches) may each have a "security".
func conceptKey(c memrank.OntologyConcept) string {
	return c.Scheme + "\x00" + strings.Join(c.Path, "\x00")
}

func parentKey(c memrank.OntologyConcept) string {
	if len(c.Path) < 2 {
		return ""
	}
	return c.Scheme + "\x00" + strings.Join(c.Path[:len(c.Path)-1], "\x00")
}

// inForceConcepts is every concept of the read's schemes, in document order.
func inForceConcepts(read OntologyRead) []memrank.OntologyConcept {
	var out []memrank.OntologyConcept
	for _, s := range read.Schemes {
		out = append(out, s.Concepts...)
	}
	return out
}

// classifyConceptTree asks memory.classifier which concepts a document is about when
// the vocabulary is too large for one request (more than memrank.ClassifyMaxConcepts).
//
// It walks each scheme from the top: it asks about the top-level concepts, then about
// the children of each one the model said the document is about, and so on down. Each
// call names at most ClassifyMaxConcepts concepts, so a tree of any size can be asked,
// and a branch the document is not about costs no questions below it. The answer is
// the most specific concepts said yes on each branch: a child said yes stands for its
// parent (a concept filter on the parent finds it anyway), and a parent said yes whose
// children were all said no is kept itself.
func (d *Document) classifyConceptTree(ctx context.Context, read OntologyRead, cd classifyDoc) ([]classifyConcept, classifyStep) {
	step := classifyStep{Step: "model", Candidates: []classifyCandidate{}}
	all := inForceConcepts(read)
	children := map[string][]memrank.OntologyConcept{}
	var frontier []memrank.OntologyConcept
	for _, c := range all {
		if pk := parentKey(c); pk == "" {
			frontier = append(frontier, c)
		} else {
			children[pk] = append(children[pk], c)
		}
	}

	var (
		chosen  []memrank.OntologyConcept
		calls   int
		reasons []string
		// waiting holds the concepts said yes whose children are being asked: each is
		// kept itself only if none of its children is said yes.
		waiting []memrank.OntologyConcept
		// fallback holds the concepts (by conceptKey) a ranking fallback took. Such a
		// parent is still descended into: its children face the normal threshold.
		fallback = map[string]bool{}
	)
	for depth := 0; len(frontier) > 0 && depth < classifyTreeMaxDepth; depth++ {
		yes, n, reason := d.askConceptBatches(ctx, cd, frontier, classifyTreeMaxCalls-calls, &step.Method, fallback)
		calls += n
		if reason != "" {
			reasons = append(reasons, reason)
		}
		hasYesChild := map[string]bool{}
		var next, nextWaiting []memrank.OntologyConcept
		for _, c := range yes {
			hasYesChild[parentKey(c)] = true
			if kids := children[conceptKey(c)]; len(kids) > 0 {
				next = append(next, kids...)
				nextWaiting = append(nextWaiting, c)
			} else {
				chosen = append(chosen, c)
			}
		}
		for _, p := range waiting {
			if !hasYesChild[conceptKey(p)] {
				chosen = append(chosen, p)
			}
		}
		frontier, waiting = next, nextWaiting
	}
	// Stopped by the depth bound with parents still waiting: they were said yes.
	chosen = append(chosen, waiting...)

	step.Reason = fmt.Sprintf("asked down the concept tree, at most %d concepts a call: %d calls", memrank.ClassifyMaxConcepts, calls)
	if len(reasons) > 0 {
		step.Reason += "; " + strings.Join(dedupe(reasons), "; ")
	}
	var out []classifyConcept
	seen := map[string]bool{}
	for _, c := range chosen {
		if seen[conceptKey(c)] {
			continue
		}
		seen[conceptKey(c)] = true
		out = append(out, classifyConcept{Concept: c.Name, Tag: c.Tag, Kind: "subject", ChunkID: c.ChunkID, Fallback: fallback[conceptKey(c)]})
	}
	return out, step
}

// askConceptBatches asks about concepts in requests of at most ClassifyMaxConcepts and
// returns those said yes, the number of calls, and the first reason a call gave; method
// is set to the first answering model's method, and a concept a call's ranking
// fallback took is added to fallback (by conceptKey). It
// makes at most budget calls, and none once ctx is done.
func (d *Document) askConceptBatches(ctx context.Context, cd classifyDoc, concepts []memrank.OntologyConcept, budget int, method *string, fallback map[string]bool) ([]memrank.OntologyConcept, int, string) {
	var yes []memrank.OntologyConcept
	calls, reason := 0, ""
	for start := 0; start < len(concepts); start += memrank.ClassifyMaxConcepts {
		if calls >= budget {
			return yes, calls, fmt.Sprintf("stopped after %d calls; %d concepts were not asked", classifyTreeMaxCalls, len(concepts)-start)
		}
		if err := ctx.Err(); err != nil {
			return yes, calls, "stopped: " + err.Error()
		}
		end := min(start+memrank.ClassifyMaxConcepts, len(concepts))
		batch := concepts[start:end]
		// An answer names a concept by name, so a name twice in one request could not be
		// told apart: ask the first and leave the other out of this call.
		byName := map[string]memrank.OntologyConcept{}
		req := memrank.ClassifyRequest{Title: cd.title, Text: cd.text, ConceptsOnly: true}
		for _, c := range batch {
			if _, dup := byName[strings.ToLower(c.Name)]; dup {
				continue
			}
			byName[strings.ToLower(c.Name)] = c
			req.Concepts = append(req.Concepts, memrank.ClassifyOption{Name: c.Name, Meaning: c.Meaning})
		}
		v := d.Classifier.Classify(ctx, req)
		calls++
		if *method == "" {
			*method = v.Method
		}
		if reason == "" && v.Reason != "" {
			reason = v.Reason
		}
		for _, name := range v.Concepts {
			if c, ok := byName[strings.ToLower(name)]; ok {
				yes = append(yes, c)
				if v.ConceptFallback {
					fallback[conceptKey(c)] = true
				}
			}
		}
	}
	return yes, calls, reason
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
