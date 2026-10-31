package builtin

import (
	"context"
	"regexp"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// document_definitional.go — which chunks are definitions passages (RFC FP R8).
//
// A document class may declare @definitions <pattern>, an RE2 pattern over chunk
// titles: the chunk whose title matches, and every chunk under it, is where the
// document says what its terms mean. The reads report it as `definitional: true` so a
// linker running in JavaScript never evaluates the pattern itself — JS regex reads
// (?i), (?P<name>…) and \b over Cyrillic differently, and a passage classified one
// way here and another way there is a linker proposing definitions nobody wrote.

// definitionalChunks returns the ids, among rows, of the chunks in a definitions
// passage of their document; nil when there are none.
//
// What counts, and why:
//   - Only a CONFIRMED ontology governs (a draft governs nothing), and only a document
//     CLASS's effective @definitions — its own or the nearest ancestor's.
//   - The class is the document root chunk's type, resolved case-insensitively against
//     the confirmed classes, exactly as anchorGrammarFor resolves it.
//   - The root chunk is never matched and never flagged: its title is the document's
//     title, and a law named "…про визначення термінів…" would otherwise mark every
//     passage in it as a definition. @title_pattern is what speaks to document titles.
//   - A fact chunk (one with a chunk_memory_meta sidecar) is never flagged: it is a
//     claim about a subject, not a passage of the document's text.
//
// Cost: nothing past the cached ontology read unless some confirmed document class has
// a pattern; then one query for the documents' classes, and — only when one of them
// has a pattern — one query for the lineage of the candidate chunks.
//
// A store fault on either query is returned rather than folded into "not
// definitional": the two are indistinguishable to the reader, and a linker told a
// definitions passage is ordinary text proposes the wrong relations from it. A fault
// reading the ontology itself degrades to no flags, as every ontology gate does.
func (d *Document) definitionalChunks(ctx context.Context, key sqlmem.ScopeKey, rows []chunkRow) (map[string]bool, error) {
	if d.Store == nil || d.SqlMem == nil {
		return nil, nil
	}
	var docIDs []string
	seenDoc := map[string]bool{}
	for _, r := range rows {
		if r.ParentID == "" || r.DocumentID == "" || seenDoc[r.DocumentID] {
			continue
		}
		seenDoc[r.DocumentID] = true
		docIDs = append(docIDs, r.DocumentID)
	}
	if len(docIDs) == 0 {
		return nil, nil
	}
	patterns := d.definitionsPatterns(ctx)
	if len(patterns) == 0 {
		return nil, nil
	}

	ph, args := inPlaceholders(docIDs)
	res, err := d.query(ctx, key, `SELECT doc.id, c.type FROM documents doc JOIN chunks c ON c.id = doc.root_chunk_id
		WHERE doc.id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	// Compiled once per class per call. The parse is shared through the ontology cache
	// and stays plain data, so the compiled form is not kept on it; the parser has
	// already validated and length-capped the pattern, and a compile error here simply
	// means "no pattern".
	compiled := map[string]*regexp.Regexp{}
	docPattern := map[string]*regexp.Regexp{}
	for _, r := range res.Rows {
		class := strings.ToLower(strings.TrimSpace(asStr(r[1])))
		p, ok := patterns[class]
		if !ok {
			continue
		}
		re, done := compiled[class]
		if !done {
			re, _ = regexp.Compile(p)
			compiled[class] = re
		}
		if re != nil {
			docPattern[asStr(r[0])] = re
		}
	}
	if len(docPattern) == 0 {
		return nil, nil
	}

	var startIDs []string
	for _, r := range rows {
		if r.ParentID != "" && docPattern[r.DocumentID] != nil {
			startIDs = append(startIDs, r.ID)
		}
	}
	if len(startIDs) == 0 {
		return nil, nil
	}
	// Every ancestor of every candidate in one statement. UNION, not UNION ALL: chains
	// that share ancestors read them once, and a corrupted parent cycle terminates —
	// a repeated row adds nothing, so the recursion runs out of new rows. The step stays
	// inside the chunk's own document, as a parent always is.
	ph, args = inPlaceholders(startIDs)
	res, err = d.query(ctx, key, `
		WITH RECURSIVE lineage(id, parent_id, title, document_id) AS (
			SELECT id, parent_id, title, document_id FROM chunks WHERE id IN (`+ph+`)
			UNION
			SELECT c.id, c.parent_id, c.title, c.document_id
			  FROM chunks c JOIN lineage l ON c.id = l.parent_id AND c.document_id = l.document_id
		)
		SELECT l.id, l.parent_id, l.title, m.chunk_id
		  FROM lineage l LEFT JOIN chunk_memory_meta m ON m.chunk_id = l.id`, args...)
	if err != nil {
		return nil, err
	}
	type node struct {
		parent, title string
		fact          bool
	}
	nodes := make(map[string]node, len(res.Rows))
	for _, r := range res.Rows {
		nodes[asStr(r[0])] = node{parent: asStr(r[1]), title: asStr(r[2]), fact: asStr(r[3]) != ""}
	}

	var out map[string]bool
	for _, r := range rows {
		re := docPattern[r.DocumentID]
		if re == nil || r.ParentID == "" {
			continue
		}
		if n, ok := nodes[r.ID]; !ok || n.fact {
			continue
		}
		visited := map[string]bool{}
		for cur := r.ID; cur != "" && !visited[cur]; {
			n, ok := nodes[cur]
			// A missing node (the row cap clipped the lineage) or the root ends the walk
			// unflagged: the root is the document, not a passage of it.
			if !ok || n.parent == "" {
				break
			}
			if re.MatchString(n.title) {
				if out == nil {
					out = map[string]bool{}
				}
				out[r.ID] = true
				break
			}
			visited[cur] = true
			cur = n.parent
		}
	}
	return out, nil
}

// definitionsPatterns maps each confirmed document class (lower-cased name) to its
// effective @definitions pattern; nil when the ontology is a draft, unreadable, or no
// document class has one. The read is the shared cached one and is not mutated.
func (d *Document) definitionsPatterns(ctx context.Context) map[string]string {
	return d.classPatterns(ctx, "definitions")
}

// classPatterns maps each confirmed document class (lower-cased name) to its effective
// value of the pattern directive name; nil when the ontology is a draft, unreadable, or
// no document class has one.
func (d *Document) classPatterns(ctx context.Context, name string) map[string]string {
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil
	}
	var out map[string]string
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if !c.IsDocumentClass {
			continue
		}
		if p := memrank.EffectiveDirective(c, name); p != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[strings.ToLower(c.Name)] = p
		}
	}
	return out
}
