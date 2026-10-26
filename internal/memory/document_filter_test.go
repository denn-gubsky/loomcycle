package memory

import (
	"reflect"
	"testing"
)

// TestSearchQuery_DocumentIDsConfineToDocumentsOutsideTheChunkNamespace — an id
// list on a search that can reach more than chunk bodies (no selector, a
// documents selector over an empty prefix, a narrowed chunk prefix) is confined
// to the document class of the chunk and unit namespaces, on the main legs and
// the unit leg alike. A search on exactly the chunk namespace keeps the
// single-namespace rule Document op=search relies on.
func TestSearchQuery_DocumentIDsConfineToDocumentsOutsideTheChunkNamespace(t *testing.T) {
	docNS := []string{DocumentChunkKeyPrefix, DocumentUnitKeyPrefix}
	for _, c := range []struct {
		name string
		q    SearchQuery
		want []string
	}{
		{"no selector", SearchQuery{}, docNS},
		{"every source", SearchQuery{Sources: []Source{SourceFacts, SourceNotes, SourceDocuments}}, docNS},
		{"documents selector", SearchQuery{Sources: []Source{SourceDocuments}}, docNS},
		{"narrowed chunk prefix", SearchQuery{Prefix: DocumentChunkKeyPrefix + "ab"}, docNS},
		{"the chunk namespace", SearchQuery{Prefix: DocumentChunkKeyPrefix}, nil},
		{"the unit namespace", SearchQuery{Prefix: DocumentUnitKeyPrefix}, nil},
	} {
		q := c.q
		q.IDs = []string{"c1"}
		main, err := q.Filter()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(main.IDsPrefixes, c.want) {
			t.Errorf("%s: main IDsPrefixes = %v, want %v", c.name, main.IDsPrefixes, c.want)
		}
		if unit, run := q.UnitLegFilter(main); run && !reflect.DeepEqual(unit.IDsPrefixes, c.want) {
			t.Errorf("%s: unit leg IDsPrefixes = %v, want %v", c.name, unit.IDsPrefixes, c.want)
		}
	}
	// No list, no confinement: a filter-free query's filter is unchanged.
	if f, _ := (SearchQuery{}).Filter(); f.IDsPrefixes != nil {
		t.Errorf("a query with no id list carries IDsPrefixes: %v", f.IDsPrefixes)
	}
}

func TestDocumentFilter_IsZeroIgnoresBlankEntries(t *testing.T) {
	if !(DocumentFilter{}).IsZero() || !(DocumentFilter{UnderPaths: []string{" ", ""}}).IsZero() {
		t.Error("a filter of blank entries must read as no filter")
	}
	for _, f := range []DocumentFilter{
		{UnderPaths: []string{"/a"}}, {ExcludeUnderPaths: []string{"/a"}},
		{DocumentIDs: []string{"d"}}, {ExcludeDocumentIDs: []string{"d"}},
	} {
		if f.IsZero() {
			t.Errorf("%+v reports IsZero", f)
		}
	}
}
