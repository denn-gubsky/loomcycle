package builtin

// document_import_scheme.go — Document op=import_scheme (RFC GM P1.4): a published
// classification, as a Markdown outline or as SKOS in RDF/XML, filed as ONE proposed
// scheme with @origin adopted. It only proposes: the shape and the accept are
// document_ontology_scheme_proposal.go's, and an operator accepts it like any ontology
// proposal. So it asks what propose_entity asks — no tenant grant — and writes nothing
// in force.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// importSchemeInputMax bounds the outline or the SKOS file one call reads. Far above
// what SchemeProposalMaxConcepts concepts take; it bounds the parse, not the scheme.
const importSchemeInputMax = 2 << 20

// importSchemeDefaultLanguage is the label language when neither the call nor the
// ontology names one.
const importSchemeDefaultLanguage = "en"

func (d *Document) importScheme(ctx context.Context, in docInput) (tools.Result, error) {
	outline, skos := strings.TrimSpace(in.Markdown) != "", strings.TrimSpace(in.SKOSXML) != ""
	if outline == skos {
		return errValidation("import_scheme: pass exactly one of markdown (an outline) and skos_xml (SKOS in RDF/XML)",
			"See the help article Document/import_scheme for the outline form."), nil
	}
	if n := len(in.Markdown) + len(in.SKOSXML); n > importSchemeInputMax {
		return errValidation(fmt.Sprintf("import_scheme: the input is %d bytes, over the limit of %d", n, importSchemeInputMax),
			"Import a smaller scheme, or its upper levels first."), nil
	}
	if len(in.Body) > proposalBodyMax {
		return errValidation(fmt.Sprintf("import_scheme: body is %d bytes, over the %d-byte limit", len(in.Body), proposalBodyMax), ""), nil
	}
	h := memrank.SchemeHeader{}
	if in.Header != nil {
		h = *in.Header
	}
	switch strings.ToLower(strings.TrimSpace(h.Origin)) {
	case "", memrank.SchemeOriginAdopted:
		h.Origin = memrank.SchemeOriginAdopted
	default:
		return errValidation("import_scheme: an imported scheme is @origin adopted, not "+h.Origin,
			"Omit header.origin; use propose_entity for a declared or generated scheme."), nil
	}

	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return errBusiness("import_scheme: "+err.Error(), "This deployment has no SQL Memory; ask an operator to enable it."), nil
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return errFrom("import_scheme: "+err.Error(), err), nil
	}
	if read.DocumentID == "" || read.RootChunkID == "" {
		return errBusiness("import_scheme: this tenant has no ontology document yet — an "+
			"operator opens the Ontology page once to create it", ""), nil
	}

	var p memrank.SchemeProposal
	var warnings []string
	if outline {
		p, err = memrank.ParseSchemeOutline(in.Markdown)
	} else {
		lang := strings.TrimSpace(in.Language)
		if lang == "" {
			lang = d.ontologyLanguage(ctx, key, mscope, read)
		}
		p, warnings, err = memrank.ParseSKOS([]byte(in.SKOSXML), lang, strings.TrimSpace(in.Scheme))
	}
	if err != nil {
		next := "Fix the input and import it again."
		if errors.Is(err, memrank.ErrSchemeTooLarge) {
			next = "Import a smaller scheme, or its upper levels first. A larger scheme needs a document of its own, " +
				"which is not supported yet (RFC GM P1.5)."
		}
		return errValidation("import_scheme: "+err.Error(), next), nil
	}
	if n := memrank.TrimOntologyName(in.Name); n != "" {
		p.Name = n
	}
	if p.Name == "" {
		return errValidation("import_scheme: the scheme has no name — the outline has no # title and the SKOS file no scheme label",
			"Pass name."), nil
	}
	if strings.TrimSpace(in.Body) != "" {
		p.Meaning = in.Body
	}
	p.Header = h
	return d.fileSchemeProposal(ctx, key, mscope, read, "import_scheme", p, warnings)
}

// ontologyLanguage is the tenant's language as its ontology root names it
// (annotation_language, which the annotator writes in), else the default.
func (d *Document) ontologyLanguage(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead) string {
	b, err := d.readBody(ctx, mscope, key, read.RootChunkID)
	if err != nil || len(b.Fields) == 0 {
		return importSchemeDefaultLanguage
	}
	var f struct {
		Language string `json:"annotation_language"`
	}
	if json.Unmarshal(b.Fields, &f) != nil || !annotationLanguageRE.MatchString(f.Language) {
		return importSchemeDefaultLanguage
	}
	return f.Language
}
