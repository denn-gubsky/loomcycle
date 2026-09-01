package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// placementBatchMax bounds one call. The caller is a consolidation pass, whose fact
// count an operator does not control, and each distinct subject costs a query.
const placementBatchMax = 200

// execPlacement answers "where does a fact about this subject belong" for a batch, and
// writes nothing.
//
// WHY AN OP AND NOT A SERVER-SIDE REDIRECT. A fact is stored twice — a key/value row that
// semantic recall searches, and a chunk mirror the graph walks — and only the chunk write
// carries the type and subject the decision needs. Resolving it independently on each path
// would mean two writers agreeing by luck, and a fact whose halves land in different
// partitions is worse than one that never moved: recall finds it in one scope and the
// graph in another.
//
// So the decision is made ONCE, by the writer that owns both halves, before either is
// written. That also keeps the scope gate intact: the caller then writes to the scope it
// was told, through the ordinary grant check, instead of the server silently redirecting a
// write to a plane the caller was never granted. An operator reading `memory_scopes` still
// sees the whole truth about what an agent can reach.
func (m *Memory) execPlacement(ctx context.Context, callerScope store.MemoryScope, in memoryInput) (tools.Result, error) {
	if len(in.Items) == 0 {
		return errResult("placement: items is required — pass [{\"type\":\"service\",\"subject\":\"checkout-api\"}, …] " +
			"for the facts you are about to write"), nil
	}
	if len(in.Items) > placementBatchMax {
		return errResult(fmt.Sprintf("placement: %d items, over the %d limit — split the batch",
			len(in.Items), placementBatchMax)), nil
	}

	policy := tools.MemoryPolicy(ctx)
	ident := tools.RunIdentity(ctx)

	// FAILS CLOSED, unlike the retrieval-side ontology reads. Those fail open because an
	// unreadable ontology should still answer the question narrowly; here "I could not
	// read the declarations" must mean "place nothing", since the alternative is placing
	// on a guess.
	terms, confirmed, exists := m.placementOntology(ctx)
	reasonNoOntology := ""
	switch {
	case m.SqlMem == nil:
		reasonNoOntology = "SQL Memory is not enabled, so no ontology can be read"
	case !exists:
		reasonNoOntology = "this tenant has no ontology document, so nothing declares where facts belong"
	case !confirmed:
		reasonNoOntology = "this tenant's ontology is a draft, so its declarations are not in force"
	}

	effective := memrank.EffectiveOntology(terms, confirmed)
	// Read ONCE per call. The names come from the user's own profile document, which the
	// self guard needs to tell a fact about them from a fact about a colleague; a
	// per-item read would repeat the same export for every fact in the pass.
	selfNames := m.selfNames(ctx)
	out := make([]map[string]any, 0, len(in.Items))
	moved := 0
	subjectTypes := map[string][]string{}

	for _, it := range in.Items {
		row := map[string]any{
			"type":    it.Type,
			"subject": it.Subject,
			"scope":   string(callerScope),
			"moved":   false,
		}
		if reasonNoOntology != "" {
			row["reason"] = reasonNoOntology
			out = append(out, row)
			continue
		}
		// Looked up lazily and memoised per call: a pass usually writes several facts
		// about the same handful of subjects, and the query only matters for a subject a
		// declaration would actually move.
		subj := strings.ToLower(strings.TrimSpace(it.Subject))
		if _, seen := subjectTypes[subj]; !seen && subj != "" {
			subjectTypes[subj] = m.typesForSubject(ctx, it.Subject)
		}
		d := memrank.ResolvePlacement(memrank.PlacementInput{
			DeclaredType:  it.Type,
			Subject:       it.Subject,
			Terms:         effective,
			CallerScope:   string(callerScope),
			UserID:        ident.UserID,
			SubjectTypes:  subjectTypes[subj],
			SelfNames:     selfNames,
			Isolated:      ident.Isolated,
			GrantedScopes: policy.AllowedScopes,
			// sql_scopes too: a tenant placement also writes the chunk mirror, which is a
			// Document write and gated on both planes.
			GrantedSqlScopes: tools.SqlMemPolicy(ctx).AllowedScopes,
		})
		row["scope"], row["moved"], row["reason"] = d.Scope, d.Moved, d.Reason
		if d.Advisory != "" {
			row["advisory"] = d.Advisory
		}
		if d.Moved {
			moved++
		}
		out = append(out, row)
	}

	return jsonResult(map[string]any{
		"placements":         out,
		"moved":              moved,
		"caller_scope":       string(callerScope),
		"granted_scopes":     policy.AllowedScopes,
		"granted_sql_scopes": tools.SqlMemPolicy(ctx).AllowedScopes,
	})
}

// placementOntology reads the tenant's effective ontology terms.
//
// Goes through the Document reader rather than duplicating it: the ontology lives in
// chunks, and a second reader is a second thing to keep in step with the hierarchy, the
// pinned roots and the inert-proposal rule.
func (m *Memory) placementOntology(ctx context.Context) (terms []memrank.OntologyTerm, confirmed, exists bool) {
	if m.Store == nil || m.SqlMem == nil {
		return nil, false, false
	}
	d := &Document{Store: m.Store, SqlMem: m.SqlMem}
	return d.tenantOntologyState(ctx)
}

// typesForSubject returns the entity types this subject is already recorded under, in the
// run's own user scope.
//
// The subject is the identity node's TITLE — a live store holds nodes titled "user",
// "loomboard", "git configuration" — so this is a title match, not a natural-key parse.
// Natural keys embed a slug of the subject under each type ("person:user",
// "location:user"), which is exactly the shape that would make a prefix scan miss.
//
// One query per distinct subject rather than one batched IN clause. A pass has few
// distinct subjects, and a hand-built placeholder list is where this kind of code grows a
// dialect bug — the count has already been wrong once in this repo on Postgres.
//
// Best-effort: a fault returns nothing, which means the conflict guard cannot fire and the
// declaration is honoured on the type in front of it. That is the one place this subsystem
// fails toward action rather than inaction, and it is bounded — a subject with no
// recorded conflict is the normal case, and the caller's own grant still gates the write.
func (m *Memory) typesForSubject(ctx context.Context, subject string) []string {
	if m.SqlMem == nil || strings.TrimSpace(subject) == "" {
		return nil
	}
	d := &Document{Store: m.Store, SqlMem: m.SqlMem}
	ident := tools.RunIdentity(ctx)
	if ident.UserID == "" {
		return nil
	}
	key := sqlmem.ScopeKey{
		Tenant:  sqlScopeTenant(ctx),
		Scope:   string(store.MemoryScopeUser),
		ScopeID: ident.UserID,
	}
	res, err := d.query(ctx, key,
		`SELECT DISTINCT coalesce(c.type, '') FROM chunks c
		   JOIN chunk_memory_meta mm ON mm.chunk_id = c.id
		  WHERE lower(coalesce(c.title, '')) = lower(?)`, subject)
	if err != nil || res == nil {
		return nil
	}
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) == 0 {
			continue
		}
		if s := strings.TrimSpace(asStr(r[0])); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// selfNames reads the names the user declared for themselves in their user-root Document.
//
// READ-ONLY, and deliberately does NOT provision the document. Prompt rendering
// provisions it on first reference, which is the right place for a side effect a person
// then sees and edits; a consolidation pass creating an empty profile behind their back
// would put a document nobody asked for in their Path tree and still declare no names.
//
// Best-effort in the direction of caution: no document, an unreadable one, or an unedited
// one all yield no names, and no names means the guard falls back to catching literal
// self-reference. It never yields a WRONG name, which is the only failure that would
// matter — a wrong name refuses to place facts about a real person of that name.
func (m *Memory) selfNames(ctx context.Context) []string {
	if m.Store == nil || m.SqlMem == nil {
		return nil
	}
	ident := tools.RunIdentity(ctx)
	if ident.UserID == "" {
		return nil
	}
	d := &Document{Store: m.Store, SqlMem: m.SqlMem}
	req, err := json.Marshal(map[string]any{
		"op": "export_md", "scope": "user", "path": memrank.UserRootPath,
		"include_metadata": false,
	})
	if err != nil {
		return nil
	}
	res, execErr := d.Execute(ctx, req)
	if execErr != nil || res.IsError {
		return nil
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	if json.Unmarshal([]byte(res.Text), &out) != nil {
		return nil
	}
	return memrank.ParseSelfNames(out.Markdown)
}
