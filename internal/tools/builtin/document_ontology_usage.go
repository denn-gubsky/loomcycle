package builtin

// document_ontology_usage.go — what the stored data says about the ontology (RFC FO
// §3.6, §6): which types are in use, and which vocabulary version facts were written
// under. Read by the Settings → Ontology panel so an operator can declare or map the
// types nobody declared, and see how much was written under an older vocabulary.

import (
	"context"
	"fmt"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TypeCount is one chunks.type value and how many chunks carry it.
type TypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// VersionCount is one ontology_version value and how many facts were written under it.
// Version 0 stands for facts with no recorded version (written before versions were
// stamped, or with no ontology document).
type VersionCount struct {
	Version int `json:"version"`
	Count   int `json:"count"`
}

// OntologyUsage is the type and version census of one scope.
type OntologyUsage struct {
	Types    []TypeCount
	Versions []VersionCount
	// StatementVersions counts relation statements by the ontology version they were
	// written under (RFC FO §3.6 counts both tables).
	StatementVersions []VersionCount
}

// maxUsageTypes bounds the type census: the panel shows a list an operator acts on, and a
// scope with thousands of free-text types has a different problem than this list solves.
const maxUsageTypes = 200

// OntologyUsageIn counts the chunk types and fact versions in one scope. Read-only, and
// deliberately no ensureSchema: a panel read must not provision a scope.
func (d *Document) OntologyUsageIn(ctx context.Context, scope string) (OntologyUsage, error) {
	if d.Store == nil || d.SqlMem == nil {
		return OntologyUsage{}, fmt.Errorf("ontology usage: requires SQL Memory")
	}
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		return OntologyUsage{}, err
	}
	var u OntologyUsage
	res, err := d.query(ctx, key,
		`SELECT type, count(*) FROM chunks WHERE type IS NOT NULL AND type <> '' GROUP BY type ORDER BY count(*) DESC, type LIMIT ?`,
		maxUsageTypes)
	if err != nil {
		return OntologyUsage{}, nil // no schema yet: nothing in use
	}
	for _, row := range res.Rows {
		if len(row) >= 2 {
			u.Types = append(u.Types, TypeCount{Type: asStr(row[0]), Count: asInt(row[1])})
		}
	}
	if vres, verr := d.query(ctx, key,
		`SELECT coalesce(ontology_version, 0), count(*) FROM chunk_memory_meta GROUP BY coalesce(ontology_version, 0) ORDER BY 1`); verr == nil {
		for _, row := range vres.Rows {
			if len(row) >= 2 {
				u.Versions = append(u.Versions, VersionCount{Version: asInt(row[0]), Count: asInt(row[1])})
			}
		}
	}
	if sres, serr := d.query(ctx, key,
		`SELECT coalesce(ontology_version, 0), count(*) FROM chunk_edges GROUP BY coalesce(ontology_version, 0) ORDER BY 1`); serr == nil {
		for _, row := range sres.Rows {
			if len(row) >= 2 {
				u.StatementVersions = append(u.StatementVersions, VersionCount{Version: asInt(row[0]), Count: asInt(row[1])})
			}
		}
	}
	return u, nil
}

// UndatedStatementsIn counts the asserted, live statements of the given kinds that carry
// no valid_at — for a kind declared @dated, statements the panel flags as missing the
// date the kind's meaning depends on. 0 for no kinds.
func (d *Document) UndatedStatementsIn(ctx context.Context, scope string, kinds []string) (int, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	if d.Store == nil || d.SqlMem == nil {
		return 0, fmt.Errorf("ontology usage: requires SQL Memory")
	}
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		return 0, err
	}
	ph, args := inPlaceholders(kinds)
	res, err := d.query(ctx, key, `SELECT count(*) FROM chunk_edges e
		WHERE e.kind IN (`+ph+`) AND e.valid_at IS NULL AND e.status = '`+edgeStatusAsserted+`' AND e.expired_at IS NULL`, args...)
	if err != nil || len(res.Rows) == 0 {
		return 0, nil // no schema yet: nothing written
	}
	return asInt(res.Rows[0][0]), nil
}

// EffectiveRelationsFor is the relation vocabulary in force for the run's tenant, or nil
// without a confirmed Relations section — the same view the relation gate uses.
func (d *Document) EffectiveRelationsFor(ctx context.Context) []memrank.OntologyRelation {
	v := d.relationView(ctx)
	if !v.active {
		return nil
	}
	return sortedRelations(v)
}
