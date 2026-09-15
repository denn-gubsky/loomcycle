package builtin

// document_cross_scope.go — RFC CV P4, decision 11: one subject, read across the
// scopes the caller can reach.
//
// A subject lives once per scope. The same person is `person:dave` in a user scope
// and `person:dave` in the tenant registry, and nothing joined them — so a tenant
// dossier could sit empty beside a user scope that knew plenty, and "what do we know
// about X" answered with whichever half the caller happened to be reading.
//
// ⚠️ THERE IS NO CROSS-SCOPE REFERENCE TO BUILD, which is the finding this phase
// turned on. RFC CV specifies references "by NATURAL KEY" and never says where one is
// stored — and it does not need to be stored anywhere. A fact already points at its
// subject node in its own scope, that node already carries `natural_key`, and two
// scopes holding the same key hold the same entity. The join exists; only the reader
// was missing.
//
// So this stores nothing and changes no schema. Subject nodes still duplicate, one
// per scope — accepted, because the duplication was never the user-visible problem.
// The split dossier was.

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// crossScopeOrder is the fan-out, bounded at three and ordered narrow-to-wide so a
// caller reading the result top-down sees their own knowledge before the shared kind.
var crossScopeOrder = []string{"agent", "user", "tenant"}

// subjectElsewhere is one other scope that knows the same subject.
type subjectElsewhere struct {
	Scope       string           `json:"scope"`
	DocumentID  string           `json:"document_id"`
	EntityChunk string           `json:"entity_chunk_id"`
	FactCount   int              `json:"facts"`
	Facts       []map[string]any `json:"fact_rows,omitempty"`
	Truncated   bool             `json:"truncated,omitempty"`
}

// naturalKeyOf reads the identity a subject chunk is known by.
func (d *Document) naturalKeyOf(ctx context.Context, key sqlmem.ScopeKey, chunkID string) string {
	res, err := d.query(ctx, key,
		`SELECT coalesce(natural_key, '') FROM chunk_memory_meta WHERE chunk_id = ?`, chunkID)
	if err != nil || len(res.Rows) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}

// readableScopesOtherThan lists the scopes this caller may read, excluding the one
// they are already in.
//
// AUTHORIZATION IS THE EXISTING GATE, not a check written here. resolveScope applies
// the agent's memory_scopes and sql_scopes exactly as every other Document op does, so
// a scope the caller was never granted simply fails to resolve and is skipped. A
// second implementation of "which scopes may this caller read" is a second chance to
// widen one by accident — and this is a read that crosses an isolation boundary, which
// is the worst place to keep a duplicate.
//
// It never reaches ANOTHER USER'S scope: `user` resolves to the caller's own user id,
// server-side from the run identity. So a tenant dossier read by A gains A's own facts
// and the tenant's, never B's — the same boundary decision 10 drew when it refused to
// backfill one user's history into the shared plane.
func (d *Document) readableScopesOtherThan(ctx context.Context, current string) []struct {
	Name string
	Key  sqlmem.ScopeKey
} {
	var out []struct {
		Name string
		Key  sqlmem.ScopeKey
	}
	for _, name := range crossScopeOrder {
		if strings.EqualFold(name, current) {
			continue
		}
		key, _, err := d.resolveScope(ctx, name)
		if err != nil {
			continue // not granted, or unresolvable for this run — not an error here
		}
		out = append(out, struct {
			Name string
			Key  sqlmem.ScopeKey
		}{Name: name, Key: key})
	}
	return out
}

// subjectAcrossScopes finds the same subject in every other readable scope and reports
// what each knows about it.
//
// withFacts controls whether the rows come back or only the count: `get_document`
// wants a signpost ("the tenant knows 9 more things about Dave"), `list_facts` wants
// the facts. One walk either way, because the expensive part is resolving the subject
// in each scope and that is shared.
func (d *Document) subjectAcrossScopes(ctx context.Context, current string, naturalKey string, limit int, withFacts bool) []subjectElsewhere {
	if strings.TrimSpace(naturalKey) == "" {
		// A subject with no key has no identity to match on. Not an error: an ordinary
		// document's root has no sidecar, and asking to read it across scopes is a
		// question with an empty answer rather than a wrong one.
		return nil
	}
	var out []subjectElsewhere
	for _, sc := range d.readableScopesOtherThan(ctx, current) {
		res, err := d.query(ctx, sc.Key,
			`SELECT m.chunk_id, c.document_id FROM chunk_memory_meta m
			   JOIN chunks c ON c.id = m.chunk_id
			  WHERE m.natural_key = ? LIMIT 1`, naturalKey)
		if err != nil || len(res.Rows) == 0 {
			// A scope that has never heard of this subject, or has no entity tier at
			// all. Skipped silently: "this scope knows nothing about Dave" and "this
			// scope has no sidecar table" are the same answer to the caller.
			continue
		}
		entityID, docID := asStr(res.Rows[0][0]), asStr(res.Rows[0][1])
		frag, fargs := factsAboutSubjectSQL(entityID, docID)
		found := subjectElsewhere{Scope: sc.Name, DocumentID: docID, EntityChunk: entityID}

		// The subject's OWN node is excluded (`c.id <> ?` below), unlike the local
		// `about:` read which includes it. Listing the other scope's duplicate subject
		// node as something it "knows about Dave" would report the split itself as a
		// finding — the one row a reader already assumes is there.
		stmt := `SELECT c.id, c.document_id, coalesce(c.title, ''), coalesce(c.type, ''),
		                coalesce(m.natural_key, ''), coalesce(m.subject, '')
		           FROM chunks c JOIN chunk_memory_meta m ON m.chunk_id = c.id
		          WHERE ` + frag + ` AND c.id <> ? ORDER BY m.created_at DESC LIMIT ?`
		args := append(append([]any{}, fargs...), entityID, limit+1)
		rows, qerr := d.query(ctx, sc.Key, stmt, args...)
		if qerr != nil {
			continue
		}
		list := rows.Rows
		if len(list) > limit {
			list, found.Truncated = list[:limit], true
		}
		found.FactCount = len(list)
		if withFacts {
			for _, r := range list {
				found.Facts = append(found.Facts, map[string]any{
					"id":          asStr(r[0]),
					"document_id": asStr(r[1]),
					"title":       asStr(r[2]),
					"type":        asStr(r[3]),
					"natural_key": asStr(r[4]),
					"subject":     asStr(r[5]),
					// The scope rides on EVERY row, not just on the group, because a
					// caller that flattens the response loses the grouping and would
					// otherwise be unable to tell a shared fact from their own.
					"scope": sc.Name,
				})
			}
		}
		out = append(out, found)
	}
	return out
}

// crossScopeAvailable reports whether a fan-out can run at all, so a caller asking for
// one against a runtime that cannot serve it is told rather than quietly given the
// local answer — the silent-downgrade shape a dropped selector already taught us.
func (d *Document) crossScopeAvailable(ctx context.Context) bool {
	return d.SqlMem != nil && len(d.readableScopesOtherThan(ctx, "")) > 0
}
