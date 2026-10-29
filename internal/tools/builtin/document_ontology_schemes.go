package builtin

// document_ontology_schemes.go — the Schemes section on the runtime side (RFC FO P1c-1):
// the tag mirror that gives every concept its cross-scope identity, and the concept
// proposals a curator may file.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SchemeMirrorReport says what the tag mirror changed (or, dry run, would change).
type SchemeMirrorReport struct {
	// Concepts is how many concept chunks were looked at.
	Concepts int `json:"concepts"`
	// Stale lists the concepts whose tags were (or would be) changed, by mirror tag.
	Stale   []string `json:"stale,omitempty"`
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
}

// MirrorSchemeTags gives every concept chunk of the run's tenant ontology its mirror tag
// (RFC FO decision 10f.2) and removes mirror-shaped tags that no longer match — a
// concept renamed or moved keeps no tag from its old place. With dryRun nothing is
// written and the report says what would change.
//
// It touches CONCEPT CHUNKS ONLY. A tag an operator or agent put on a document is that
// document's classification, and the mirror has no say over it — which is also why a
// concept's rename does not silently reclassify documents: their tags stay until someone
// re-files them.
//
// "Mirror-shaped" means the first segment is a declared scheme's mirror segment: a
// concept chunk's other tags are left alone. Idempotent: a second run changes nothing.
func (d *Document) MirrorSchemeTags(ctx context.Context, dryRun bool) (SchemeMirrorReport, error) {
	var rep SchemeMirrorReport
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return rep, err
	}
	// Read directly, not through the cache: a mirror run is an operator action that
	// must see the document as it is now, and it is rare enough not to need the cache.
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath)
	if err != nil {
		return rep, err
	}
	if read.partial {
		// A concept whose body could not be read still has its title, so its tag is
		// right; but an unreadable SECTION body hides the whole section. Never strip tags
		// on a read that may have lost part of the tree.
		return rep, fmt.Errorf("the ontology could not be read completely; try again")
	}
	roots := map[string]bool{}
	for _, s := range read.Schemes {
		roots[memrank.MirrorTag(s.Name, nil)] = true
	}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			rep.Concepts++
			have, err := d.listChunkTags(ctx, key, c.ChunkID)
			if err != nil {
				return rep, err
			}
			want := []string{c.Tag}
			added, removed := true, 0
			for _, t := range have {
				switch {
				case t == c.Tag:
					added = false
				case roots[strings.SplitN(t, "/", 2)[0]]:
					removed++ // a mirror tag from the concept's old name or place
				default:
					want = append(want, t)
				}
			}
			if !added && removed == 0 {
				continue
			}
			rep.Stale = append(rep.Stale, c.Tag)
			if added {
				rep.Added++
			}
			rep.Removed += removed
			if dryRun {
				continue
			}
			sort.Strings(want)
			if err := d.replaceChunkTags(ctx, key, c.ChunkID, want); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// proposeConcept files an inert concept into a scheme. The parent is a scheme name, a
// concept name, or a concept's mirror tag — the tag disambiguates a concept name that
// two schemes share.
func (d *Document) proposeConcept(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead, name string, in docInput) (tools.Result, error) {
	if read.SectionChunks[memrank.SectionSchemes] == "" {
		return errBusiness("propose_entity: this tenant's ontology declares no Schemes section, so there is no "+
			"concept scheme to propose into", "Describe the topic in prose instead; an operator decides whether to declare schemes."), nil
	}
	want := strings.TrimSpace(in.Parent)
	if want == "" {
		return errValidation("propose_entity: a concept needs a parent — the scheme or the broader concept it belongs under",
			"Pass parent: a scheme name (e.g. topics), a concept name, or a concept tag (topics/risk-management)."), nil
	}
	var parentChunk, parentTag string
	var have []string
	for _, s := range read.Schemes {
		have = append(have, s.Name)
		if parentChunk == "" && (s.Name == want || memrank.MirrorTag(s.Name, nil) == want) {
			parentChunk, parentTag = s.ChunkID, memrank.MirrorTag(s.Name, nil)
		}
		for _, c := range s.Concepts {
			if parentChunk == "" && (c.Tag == want || strings.EqualFold(c.Name, want)) {
				parentChunk, parentTag = c.ChunkID, c.Tag
			}
		}
	}
	if parentChunk == "" {
		return errValidation("propose_entity: no scheme or concept named "+want+" is declared (schemes: "+
			strings.Join(have, ", ")+")", "Pass a declared scheme, concept name or concept tag as parent."), nil
	}
	tag := parentTag + "/" + memrank.MirrorSegment(name)
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if c.Tag == tag {
				return errBusiness("propose_entity: concept "+tag+" is already declared", "Use it as it is."), nil
			}
		}
	}
	for _, p := range read.Proposals {
		if p.Section == memrank.SectionSchemes && strings.EqualFold(p.Name, name) && p.Parent == want {
			return errBusiness("propose_entity: concept "+name+" is already "+p.Status+" — an operator has seen it",
				"Do not propose it again; an operator decides."), nil
		}
	}
	return d.createOntologyProposal(ctx, key, mscope, read.DocumentID, parentChunk, name, in.Body)
}

// EffectiveSchemesFor is the run's tenant's concept schemes in force: those of a
// CONFIRMED ontology, through the same cached read the gates use; nil otherwise.
func (d *Document) EffectiveSchemesFor(ctx context.Context) []memrank.OntologyScheme {
	if d.Store == nil || d.SqlMem == nil {
		return nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil
	}
	return read.Schemes
}
