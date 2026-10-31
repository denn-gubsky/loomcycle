package builtin

// A subject changing kind, or its document being deleted, is announced on the scope's
// memory/subjects channel (publishSubjectEvent), as adoption is. Whatever indexes
// relations by subject re-checks the statements that touch it: a re-typed subject may no
// longer fit a relation kind's domain or range, and a removed one leaves other scopes'
// evidence naming a key that no longer resolves.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// subjectRoot is a subject as its document records it: the document's ROOT chunk,
// carrying a natural key. That is the shape create_document with type+subject+
// natural_key makes, and so the shape adoption and subject homing leave behind. A keyed
// chunk below a root is a fact filed under a subject, not a subject.
type subjectRoot struct {
	NaturalKey, DocumentID, RootChunkID, Type string
}

// subjectRootOf returns the subject whose root chunk (byDocument=false) or whose
// document (byDocument=true) is id, and found=false when it is not a subject.
func (d *Document) subjectRootOf(ctx context.Context, key sqlmem.ScopeKey, id string, byDocument bool) (s subjectRoot, found bool, err error) {
	where := `doc.root_chunk_id = ?`
	if byDocument {
		where = `doc.id = ?`
	}
	res, err := d.query(ctx, key,
		`SELECT m.natural_key, doc.id, doc.root_chunk_id, coalesce(c.type, '') FROM documents doc
		   JOIN chunk_memory_meta m ON m.chunk_id = doc.root_chunk_id
		   JOIN chunks c ON c.id = doc.root_chunk_id
		  WHERE `+where+` AND coalesce(m.natural_key, '') <> '' LIMIT 1`, id)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 4 {
		return subjectRoot{}, false, err
	}
	r := res.Rows[0]
	return subjectRoot{NaturalKey: asStr(r[0]), DocumentID: asStr(r[1]), RootChunkID: asStr(r[2]), Type: asStr(r[3])}, true, nil
}

// subjectRetype is a write that changes a subject root's type, carried from the check
// before the write to the announcement after it. nil: the write re-types no subject.
type subjectRetype struct {
	subject subjectRoot
	newType string
}

// checkSubjectRetype runs before a write that sets chunkID's type to newType, in every
// path that can (update_chunk, and upsert_chunk on a key it already holds), so no route
// changes a subject's kind without announcing it. A chunk that is not a subject root, or
// a type that does not change, yields nil.
//
// The new type must be an entity type in force, judged as an entity write is: a
// subject of an undeclared kind is a node nobody finds by kind. gated says the caller
// already held newType to that rule (upsert_chunk with type+subject), so it is not read
// twice. The natural key is kept: it names the subject, and its prefix records the kind
// at minting, not now.
func (d *Document) checkSubjectRetype(ctx context.Context, key sqlmem.ScopeKey, op, chunkID, newType string, gated bool) (*subjectRetype, *tools.Result, error) {
	s, found, err := d.subjectRootOf(ctx, key, chunkID, false)
	if err != nil || !found {
		return nil, nil, err
	}
	if strings.EqualFold(strings.TrimSpace(s.Type), strings.TrimSpace(newType)) {
		return nil, nil, nil
	}
	if !gated {
		if declared, undeclared := d.undeclaredEntityType(ctx, strings.TrimSpace(newType)); undeclared {
			r := errValidation(op+": "+strconv.Quote(newType)+" is not an entity type this tenant declares, and this "+
				"chunk is the subject "+s.NaturalKey+", so re-typing it would leave a subject of a kind nobody can "+
				"find. Declared: "+strings.Join(declared, ", ")+". Nothing was written.",
				"Use one of the declared types, or propose the type with op=propose_entity and let an operator accept it.")
			return nil, &r, nil
		}
	}
	return &subjectRetype{subject: s, newType: newType}, nil, nil
}

// announceRetype publishes `retyped` for a write checkSubjectRetype let through, once it
// has been written. Returns why it was not announced, or "".
func (d *Document) announceRetype(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rt *subjectRetype) string {
	if rt == nil {
		return ""
	}
	return d.publishSubjectEvent(ctx, key, mscope, subjectEvent{
		Op: "retyped", NaturalKey: rt.subject.NaturalKey, DocumentID: rt.subject.DocumentID,
		RootChunkID: rt.subject.RootChunkID, OldType: rt.subject.Type, NewType: rt.newType,
	})
}

// withNotAnnounced adds the reason an event was withheld to an op's JSON result, so
// the caller learns it rather than assuming subscribers were told.
func withNotAnnounced(res tools.Result, why string) tools.Result {
	if why == "" || res.IsError {
		return res
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(res.Text))
	// UseNumber: results carry unix-nano timestamps above 2^53, which a float64 round
	// trip would corrupt.
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return res
	}
	out["not_announced"] = why
	b, err := json.Marshal(out)
	if err != nil {
		return res
	}
	res.Text = string(b)
	return res
}
