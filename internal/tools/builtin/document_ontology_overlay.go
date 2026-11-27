package builtin

// document_ontology_overlay.go — changes applied to the ontology document's chunks in
// memory, so a plan (RFC GQ P1) can read the ontology as it would be after them with
// the one reader every other caller uses (ontologyForKey). Nothing here writes.

// ontologyOverlay is a set of in-memory changes to the ontology document's chunks,
// keyed by chunk id. A nil overlay changes nothing.
type ontologyOverlay struct {
	titles  map[string]string    // rename: the chunk's new title
	parents map[string]string    // move: the chunk's new parent chunk
	bodies  map[string]chunkBody // edit: the chunk's new body (fields kept)
	gone    map[string]bool      // delete or retire: the chunk is read as absent
	// statuses is a chunk's new status (unretire: ""). Only set by an unretire, so an
	// overlay without one reads every status as stored.
	statuses map[string]string
}

func newOntologyOverlay() *ontologyOverlay {
	return &ontologyOverlay{titles: map[string]string{}, parents: map[string]string{},
		bodies: map[string]chunkBody{}, gone: map[string]bool{}, statuses: map[string]string{}}
}

// rewrite applies the overlay to one chunk row and reports whether the row is still
// read. A chunk read as absent takes its whole subtree with it: its children are
// reached only through it.
func (ov *ontologyOverlay) rewrite(c *ontologyChunk) bool {
	if ov == nil {
		return true
	}
	if ov.gone[c.id] {
		return false
	}
	if t, ok := ov.titles[c.id]; ok {
		c.title = t
	}
	if p, ok := ov.parents[c.id]; ok {
		c.parentID = p
	}
	if s, ok := ov.statuses[c.id]; ok {
		c.status = s
	}
	return true
}

// body is the chunk's overlaid body, if the overlay changes it.
func (ov *ontologyOverlay) body(id string) (chunkBody, bool) {
	if ov == nil {
		return chunkBody{}, false
	}
	b, ok := ov.bodies[id]
	return b, ok
}
