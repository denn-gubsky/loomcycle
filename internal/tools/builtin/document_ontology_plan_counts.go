package builtin

// document_ontology_plan_counts.go — what rests on the entries a plan touches
// (RFC GQ P1, 4.2 + 9b), counted across the tenant.
//
// TWO KINDS OF ROW. Rows keyed by an ontology chunk id (statements to a concept or a
// class chunk, node annotations, node statistics of a concept, series, memberships)
// live in the tenant scope, beside the ontology. Rows keyed by a NAME (a class on
// documents and chunks, identity and subject keys, a relation kind on statements, a
// concept's tag on documents) live in every scope: most of a class's dependents are in
// user and agent scopes, where consolidation writes typed facts. So name-keyed counts
// walk the tenant scope and every durable user and agent scope of the tenant
// (sqlmem.Manager.ListScopes), and the rest are read in the tenant scope only.
//
// WHOLE-SCOPE, no access filter: a plan is the operator's, and it counts what a change
// would touch whoever may read it. Sample ids come from the tenant scope only, so a
// plan never lists another user's documents (RFC GQ 9c).
//
// READS ONLY, AND NEVER PROVISIONS. PlanOntology runs under noWrites, so every read
// goes through sqlmem.Manager.QueryExisting (no provisioning DDL, no last-use stamp
// that would keep a dormant scope from GC) and d.exec refuses. planQuery reads a table
// or column a scope never made as "nothing here"; a scope is read only when a plane
// holding its documents exists. No transaction is opened: on SQLite a scope has one
// connection.
//
// CLASS NAMES MATCH UNDER UNICODE CASE FOLDING, as everywhere else (strings.EqualFold):
// the stored spellings are read, narrowed by length, and folded in Go (planSpellings),
// then counted exactly — SQL lower() folds ASCII only on SQLite and Unicode on Postgres.
// Prefixes are compared with substr, portable to both tiers (instr and split_part are
// not).

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// ontologyPlanScopeBudget bounds the time a plan spends on user and agent scopes. A
// scope not reached or not finished in it is reported skipped, and the plan
// incomplete, rather than the request timing out with nothing.
const ontologyPlanScopeBudget = 20 * time.Second

// countPlanDependents fills each change's counts. counted and skipped are the user and
// agent scopes counted and left out; complete is false when any was left out.
func (d *Document) countPlanDependents(ctx context.Context, key sqlmem.ScopeKey, before, after OntologyRead,
	planned []*plannedChange) (counted, skipped int, complete bool, err error) {
	var members conceptMembership
	for _, p := range planned {
		if len(p.concepts) > 0 {
			if members, err = d.conceptMembership(ctx, key, before.Schemes, nil); err != nil {
				return 0, 0, false, err
			}
			break
		}
	}
	for _, p := range planned {
		if err := d.countPlanTenant(ctx, key, before, after, members, p); err != nil {
			return 0, 0, false, err
		}
		if err := d.countPlanNames(ctx, key, before.DocumentID, true, p); err != nil {
			return 0, 0, false, err
		}
	}

	scopes, err := d.SqlMem.ListTenantScopes(ctx, key.Tenant)
	if err != nil {
		return 0, 0, false, err
	}
	// The budget is a deadline on every read of the pass, not only a check between
	// scopes: a slow scope is cut mid-count (and reported skipped), not finished.
	budget := d.planScopeBudget
	if budget <= 0 {
		budget = ontologyPlanScopeBudget
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, sk := range scopes {
		// The listing is the tenant's; the comparison is belt-and-braces.
		if sk.Tenant != key.Tenant || (sk.Scope != "user" && sk.Scope != "agent") {
			continue
		}
		if sctx.Err() != nil {
			skipped++
			continue
		}
		at, ok, perr := d.planScopePlane(sctx, sk)
		if perr != nil {
			skipped++
			slog.WarnContext(ctx, "ontology plan: could not check a scope", "tenant", sk.Tenant, "scope", sk.Scope, "err", perr)
			continue
		}
		if !ok {
			continue // neither plane holds documents: nothing to count
		}
		// Counted into a copy first, so a scope that fails halfway adds nothing.
		scratch := make([]map[string]*connector.OntologyPlanCount, len(planned))
		collisions := make([]int, len(planned))
		var serr error
		for i, p := range planned {
			q := &plannedChange{entry: p.entry, classes: p.classes, kinds: p.kinds, tags: p.tags,
				newlyFunctional: p.newlyFunctional, newlyAsymmetric: p.newlyAsymmetric,
				counts: map[string]*connector.OntologyPlanCount{}}
			if serr = d.countPlanNames(sctx, at, "", false, q); serr != nil {
				break
			}
			scratch[i] = q.counts
			if p.in.Op == connector.OntologyPlanRename && p.entry.Kind == planKindClass {
				// The rename job rewrites this scope too (RFC GQ P5.4): a key it would move
				// onto one another row holds is found here, before anything is written.
				if _, collisions[i], serr = d.renameCollisions(sctx, at, renameOwnNames(p), strings.TrimSpace(p.in.Name)); serr != nil {
					break
				}
			}
		}
		if serr != nil {
			skipped++
			slog.WarnContext(ctx, "ontology plan: could not count a scope", "tenant", sk.Tenant, "scope", sk.Scope, "err", serr)
			continue
		}
		for i, p := range planned {
			for name, c := range scratch[i] {
				p.add(name, false, c.N, c.ByStatus, nil)
			}
			p.otherCollisions += collisions[i]
		}
		counted++
	}
	return counted, skipped, skipped == 0, nil
}

// planScopePlane is the plane holding a user or agent scope's documents: its document
// plane once that exists, else its agent plane when the scope is from before the planes
// split and still holds them there. Postgres never moves such a scope until the
// Document tool next uses it, and sqlite leaves one it could not move at boot (or one a
// snapshot from before the split restored) for its first use, so "no document plane"
// is not "no documents". ok is false when neither plane holds them. An agent could plant
// a table named documents in a scope that never held any; what that adds is a count,
// never a write or a sample.
func (d *Document) planScopePlane(ctx context.Context, sk sqlmem.ScopeKey) (sqlmem.ScopeKey, bool, error) {
	ok, err := d.docSQL().PlaneExists(ctx, sk)
	if err != nil || ok {
		return sk, ok, err
	}
	held, err := d.docSQL().AgentPlaneTables(ctx, sk, []string{"documents"})
	if err != nil || len(held) == 0 {
		return sk, false, err
	}
	return sk.AgentPlane(), true, nil
}

// add folds n rows of one dependent into the change's counts. An absent count is
// created even for n == 0, so the response shows what was looked for.
func (p *plannedChange) add(name string, tenant bool, n int, byStatus map[string]int, sample []string) {
	c := p.counts[name]
	if c == nil {
		c = &connector.OntologyPlanCount{}
		p.counts[name] = c
	}
	c.N += n
	if tenant {
		c.TenantScope += n
		c.Sample = append(c.Sample, sample...)
		if len(c.Sample) > ontologyPlanSample {
			c.Sample = c.Sample[:ontologyPlanSample]
		}
	} else {
		c.OtherScopes += n
	}
	for s, k := range byStatus {
		if c.ByStatus == nil {
			c.ByStatus = map[string]int{}
		}
		c.ByStatus[s] += k
	}
}

// planQuery is a read that never provisions: a table or column the scope never made is
// an empty answer (nothing rests there), not a reason to create it.
func (d *Document) planQuery(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) (*sqlmem.QueryResult, error) {
	res, err := d.sqlRead(ctx, key, stmt, args)
	if schemaMissing(err) {
		return &sqlmem.QueryResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// planCount is a count(*) read through planQuery.
func (d *Document) planCount(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) (int, error) {
	res, err := d.planQuery(ctx, key, stmt, args...)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return 0, err
	}
	return asInt(res.Rows[0][0]), nil
}

// planIDs is the first column of up to ontologyPlanSample rows.
func (d *Document) planIDs(ctx context.Context, key sqlmem.ScopeKey, stmt string, args ...any) ([]string, error) {
	res, err := d.planQuery(ctx, key, stmt+` LIMIT `+fmt.Sprint(ontologyPlanSample), args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range res.Rows {
		if len(r) > 0 {
			out = append(out, asStr(r[0]))
		}
	}
	return out, nil
}

// prefixAny is `(substr(col, 1, n) = p OR …)` for each prefix's "<prefix>:".
func prefixAny(col string, prefixes []string) (string, []any) {
	parts := make([]string, 0, len(prefixes))
	var args []any
	for _, n := range prefixes {
		p := n + ":"
		parts = append(parts, "substr("+col+", 1, ?) = ?")
		args = append(args, utf8.RuneCountInString(p), p)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// planSpellings is the stored values of col (with prefix, the "<value>:" prefixes of
// it) equal to one of names under Unicode case folding (strings.EqualFold, as class
// names are matched everywhere else), for the counts to match exactly. SQL lower()
// cannot do it portably: SQLite's folds ASCII only and Postgres's folds Unicode, so
// "Ärzte" against "ÄRZTE" matched on one tier and not the other. Values equal under
// folding have the same length in characters, which length() and substr() count on
// both tiers, so the read is narrowed by length and the folding done here. where, when
// not empty, narrows the rows further.
func (d *Document) planSpellings(ctx context.Context, key sqlmem.ScopeKey, table, col, where string, names []string, prefix bool) ([]string, error) {
	byLen := map[int][]string{}
	var lengths []int
	for _, n := range names {
		l := utf8.RuneCountInString(n)
		if byLen[l] == nil {
			lengths = append(lengths, l)
		}
		byLen[l] = append(byLen[l], n)
	}
	sort.Ints(lengths)
	var out []string
	for _, l := range lengths {
		stmt, args := `SELECT DISTINCT `+col+` FROM `+table+` WHERE `+col+` IS NOT NULL AND length(`+col+`) = ?`, []any{l}
		if prefix {
			stmt = `SELECT DISTINCT substr(` + col + `, 1, ?) FROM ` + table + ` WHERE ` + col + ` IS NOT NULL AND substr(` + col + `, ?, 1) = ':'`
			args = []any{l, l + 1}
		}
		if where != "" {
			stmt += ` AND ` + where
		}
		res, err := d.planQuery(ctx, key, stmt, args...)
		if err != nil {
			return nil, err
		}
		if res.Truncated {
			return nil, fmt.Errorf("too many distinct values of %s.%s to match", table, col)
		}
		for _, r := range res.Rows {
			if len(r) == 0 {
				continue
			}
			v := asStr(r[0])
			for _, n := range byLen[l] {
				if strings.EqualFold(v, n) {
					out = appendUnique(out, v)
					break
				}
			}
		}
	}
	return out, nil
}

// planClassMatch is the condition matching col's stored spellings of the change's
// classes; ok is false when the scope stores none (nothing to count).
func (d *Document) planClassMatch(ctx context.Context, key sqlmem.ScopeKey, table, col, where string, classes []string, prefix bool) (string, []any, bool, error) {
	vals, err := d.planSpellings(ctx, key, table, col, where, classes, prefix)
	if err != nil || len(vals) == 0 {
		return "", nil, false, err
	}
	if prefix {
		cond, args := prefixAny(col, vals)
		return cond, args, true, nil
	}
	ph, args := inPlaceholders(vals)
	return col + ` IN (` + ph + `)`, args, true, nil
}

// countPlanNames counts the name-keyed dependents of one change in one scope. Samples
// are taken only when tenant is set; excludeDoc (the ontology document, in the tenant
// scope) is left out of the concept-tag count, since its concept chunks carry their
// own mirror tags.
func (d *Document) countPlanNames(ctx context.Context, key sqlmem.ScopeKey, excludeDoc string, tenant bool, p *plannedChange) error {
	type count struct {
		name, stmt, sample string
		args               []any
	}
	var counts []count
	if len(p.classes) > 0 {
		for _, c := range []struct {
			name, table, col, id string
			prefix               bool
		}{
			{"typed_documents", "documents", "type", "id", false},
			{"typed_chunks", "chunks", "type", "id", false},
			{"identity_keys", "chunks", "identity_key", "id", true},
			{"subject_keys", "chunk_memory_meta", "natural_key", "chunk_id", true},
		} {
			cond, a, ok, err := d.planClassMatch(ctx, key, c.table, c.col, "", p.classes, c.prefix)
			if err != nil {
				return fmt.Errorf("%s: %w", c.name, err)
			}
			if !ok {
				p.add(c.name, tenant, 0, nil, nil)
				continue
			}
			from := ` FROM ` + c.table + ` WHERE ` + cond
			counts = append(counts, count{c.name, `SELECT count(*)` + from, `SELECT ` + c.id + from + ` ORDER BY ` + c.id, a})
		}
	}
	if len(p.kinds) > 0 {
		ph, ka := inPlaceholders(p.kinds)
		n, by, err := d.planStatusCounts(ctx, key, `e.kind IN (`+ph+`)`, ka)
		if err != nil {
			return err
		}
		var sample []string
		if tenant && n > 0 {
			if sample, err = d.planIDs(ctx, key, `SELECT e.id FROM chunk_edges e WHERE e.kind IN (`+ph+`) ORDER BY e.id`, ka...); err != nil {
				return err
			}
		}
		p.add("statements_of_kind", tenant, n, by, sample)
		if p.newlyFunctional || p.newlyAsymmetric {
			if err := d.countCharacteristicBreaks(ctx, key, tenant, p); err != nil {
				return err
			}
		}
	}
	for _, tag := range p.tags {
		cond := `(t.tag = ? OR substr(t.tag, 1, ?) = ?)`
		ta := []any{tag, utf8.RuneCountInString(tag + "/"), tag + "/"}
		inner := `SELECT c.document_id AS doc FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id WHERE ` + cond +
			` UNION SELECT t.document_id AS doc FROM document_tags t WHERE ` + cond
		args := append(append(append([]any{}, ta...), ta...), excludeDoc)
		counts = append(counts, count{"concept_tags", `SELECT count(*) FROM (` + inner + `) x WHERE x.doc <> ?`,
			`SELECT x.doc FROM (` + inner + `) x WHERE x.doc <> ? ORDER BY x.doc`, args})
	}
	for _, c := range counts {
		n, err := d.planCount(ctx, key, c.stmt, c.args...)
		if err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		var sample []string
		if tenant && n > 0 {
			if sample, err = d.planIDs(ctx, key, c.sample, c.args...); err != nil {
				return fmt.Errorf("%s: %w", c.name, err)
			}
		}
		p.add(c.name, tenant, n, nil, sample)
	}
	return nil
}

// countCharacteristicBreaks counts the live asserted statements a characteristic the
// edit adds would break: for @functional, the from-ends holding more than one target
// of the kind; for @asymmetric, the statements whose reverse is also asserted. Only the
// edited relation's own kind: a characteristic is not inherited by its sub-kinds, and
// the statement gate checks it per kind (checkCharacteristics).
func (d *Document) countCharacteristicBreaks(ctx context.Context, key sqlmem.ScopeKey, tenant bool, p *plannedChange) error {
	ph, ka := "?", []any{p.entry.Name}
	live := func(a string) string {
		return a + `.status = '` + edgeStatusAsserted + `' AND ` + a + `.expired_at IS NULL`
	}
	var stmts [][2]string
	if p.newlyFunctional {
		inner := `SELECT e.from_id FROM chunk_edges e WHERE e.kind IN (` + ph + `) AND ` + live("e") +
			` GROUP BY e.from_id HAVING count(DISTINCT e.to_id) > 1`
		stmts = append(stmts, [2]string{`SELECT count(*) FROM (` + inner + `) x`, inner + ` ORDER BY e.from_id`})
	}
	if p.newlyAsymmetric {
		from := ` FROM chunk_edges a JOIN chunk_edges b ON b.from_id = a.to_id AND b.to_id = a.from_id AND b.kind = a.kind
		         WHERE a.kind IN (` + ph + `) AND ` + live("a") + ` AND ` + live("b")
		stmts = append(stmts, [2]string{`SELECT count(*)` + from, `SELECT a.id` + from + ` ORDER BY a.id`})
	}
	for _, s := range stmts {
		n, err := d.planCount(ctx, key, s[0], ka...)
		if err != nil {
			return fmt.Errorf("characteristic_breaks: %w", err)
		}
		var sample []string
		if tenant && n > 0 {
			if sample, err = d.planIDs(ctx, key, s[1], ka...); err != nil {
				return fmt.Errorf("characteristic_breaks: %w", err)
			}
		}
		p.add("characteristic_breaks", tenant, n, nil, sample)
	}
	return nil
}

// planStatusCounts counts the statements matching where (on alias e) by status; a
// retired (expired) statement counts as "expired" whatever its status.
func (d *Document) planStatusCounts(ctx context.Context, key sqlmem.ScopeKey, where string, args []any) (int, map[string]int, error) {
	expired := `CASE WHEN e.expired_at IS NULL THEN 0 ELSE 1 END`
	res, err := d.planQuery(ctx, key, `SELECT e.status, `+expired+`, count(*) FROM chunk_edges e WHERE `+where+
		` GROUP BY e.status, `+expired, args...)
	if err != nil {
		return 0, nil, err
	}
	total, by := 0, map[string]int{}
	for _, r := range res.Rows {
		if len(r) < 3 {
			continue
		}
		status := asStr(r[0])
		if asInt(r[1]) == 1 {
			status = "expired"
		}
		by[status] += asInt(r[2])
		total += asInt(r[2])
	}
	if total == 0 {
		by = nil
	}
	return total, by, nil
}

// countPlanTenant counts the dependents that live in the tenant scope only: rows keyed
// by a chunk id of the subtree, and what the ontology itself says about the entry.
func (d *Document) countPlanTenant(ctx context.Context, key sqlmem.ScopeKey, before, after OntologyRead,
	members conceptMembership, p *plannedChange) error {
	if err := d.countSubtreeStatements(ctx, key, before.DocumentID, p); err != nil {
		return fmt.Errorf("statements: %w", err)
	}
	if err := d.countSubtreeRows(ctx, key, p); err != nil {
		return err
	}
	if len(p.concepts) > 0 {
		docs := map[string]bool{}
		for _, id := range p.concepts {
			for doc := range members.members[id] {
				docs[doc] = true
			}
		}
		p.add("members", true, len(docs), nil, sortedSample(docs))
		if p.in.Op == connector.OntologyPlanMove || p.in.Op == connector.OntologyPlanEdit {
			p.countPolicyChanges(before, after)
		}
	}
	if len(p.classes) > 0 {
		in, a, matched, err := d.planClassMatch(ctx, key, "documents", "type", "", p.classes, false)
		if err != nil {
			return fmt.Errorf("document_annotations: %w", err)
		}
		n := 0
		stmt := ` FROM chunk_annotations an JOIN chunks c ON c.id = an.chunk_id JOIN documents doc ON doc.id = c.document_id WHERE doc.` + in
		if matched {
			if n, err = d.planCount(ctx, key, `SELECT count(*)`+stmt, a...); err != nil {
				return fmt.Errorf("document_annotations: %w", err)
			}
		}
		var sample []string
		if n > 0 {
			if sample, err = d.planIDs(ctx, key, `SELECT an.chunk_id`+stmt+` ORDER BY an.chunk_id`, a...); err != nil {
				return fmt.Errorf("document_annotations: %w", err)
			}
		}
		p.add("document_annotations", true, n, nil, sample)
		p.countClassReferences(before)
	}
	if len(p.kinds) > 0 {
		var hits []string
		if before.Review != nil {
			for _, r := range before.Review.Rules {
				for _, k := range r.Kinds {
					if containsString(p.kinds, k) {
						hits = append(hits, fmt.Sprint(r.Index))
						break
					}
				}
			}
		}
		p.add("review_rules", true, len(hits), nil, hits)
	}
	if len(p.instances) > 0 {
		return d.countInstanceSubjects(ctx, key, p)
	}
	return nil
}

// countClassReferences counts what the ontology itself says about the change's
// classes: the instances of them and the schemes whose @projects names one.
func (p *plannedChange) countClassReferences(r OntologyRead) {
	var inst, proj []string
	for _, in := range r.Instances {
		if containsFold(p.classes, in.Class) {
			inst = append(inst, in.ChunkID)
		}
	}
	for _, s := range r.Schemes {
		for _, c := range s.Projects {
			if containsFold(p.classes, c) {
				proj = append(proj, s.ChunkID)
				break
			}
		}
	}
	p.add("instances", true, len(inst), nil, inst)
	p.add("projects", true, len(proj), nil, proj)
}

// countPolicyChanges counts the concepts of the subtree whose effective retention or
// groups the change would alter (RFC FO 10: reported, not enforced).
func (p *plannedChange) countPolicyChanges(before, after OntologyRead) {
	// Every group counted (nil): whether a group exists does not change with the move.
	was, _ := memrank.EffectiveConceptPolicies(before.Schemes, nil)
	now, _ := memrank.EffectiveConceptPolicies(after.Schemes, nil)
	var changed []string
	for _, id := range p.concepts {
		if !reflect.DeepEqual(was[id], now[id]) {
			changed = append(changed, id)
		}
	}
	p.add("policy_changes", true, len(changed), nil, changed)
}

// countSubtreeStatements counts the statements touching any chunk of the subtree, each
// once — what a delete removes with it. An edge between two chunks of the subtree, or
// from one ontology chunk to another, is counted by its from-end only.
func (d *Document) countSubtreeStatements(ctx context.Context, key sqlmem.ScopeKey, ontologyDoc string, p *plannedChange) error {
	inSubtree := map[string]bool{}
	for _, id := range p.subtree {
		inSubtree[id] = true
	}
	total, by := 0, map[string]int{}
	fold := func(n int, b map[string]int) {
		total += n
		for s, k := range b {
			by[s] += k
		}
	}
	var sample []string
	for start := 0; start < len(p.subtree); start += ontologyCountConceptBatch {
		ph, ids := inPlaceholders(p.subtree[start:min(start+ontologyCountConceptBatch, len(p.subtree))])
		// From the subtree: each edge has one from-end, so batches never overlap.
		n, b, err := d.planStatusCounts(ctx, key, `e.from_id IN (`+ph+`)`, ids)
		if err != nil {
			return err
		}
		fold(n, b)
		// Into the subtree from outside the ontology document.
		n, b, err = d.planStatusCounts(ctx, key, `e.to_id IN (`+ph+`) AND e.from_id NOT IN (SELECT id FROM chunks WHERE document_id = ?)`,
			append(append([]any{}, ids...), ontologyDoc))
		if err != nil {
			return err
		}
		fold(n, b)
		// Into the subtree from another ontology chunk outside it (a focus statement):
		// few, so read as rows and checked against the whole subtree.
		res, err := d.planQuery(ctx, key, `SELECT e.from_id, e.status, CASE WHEN e.expired_at IS NULL THEN 0 ELSE 1 END
			   FROM chunk_edges e WHERE e.to_id IN (`+ph+`) AND e.from_id IN (SELECT id FROM chunks WHERE document_id = ?)`,
			append(append([]any{}, ids...), ontologyDoc)...)
		if err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("too many statements between ontology entries to count")
		}
		for _, r := range res.Rows {
			if len(r) < 3 || inSubtree[asStr(r[0])] {
				continue
			}
			status := asStr(r[1])
			if asInt(r[2]) == 1 {
				status = "expired"
			}
			fold(1, map[string]int{status: 1})
		}
		if len(sample) < ontologyPlanSample {
			got, err := d.planIDs(ctx, key, `SELECT e.id FROM chunk_edges e WHERE e.from_id IN (`+ph+`) OR e.to_id IN (`+ph+`) ORDER BY e.id`,
				append(append([]any{}, ids...), ids...)...)
			if err != nil {
				return err
			}
			sample = append(sample, got...)
		}
	}
	if total == 0 {
		by = nil
	}
	p.add("statements", true, total, by, sample)
	return nil
}

// countSubtreeRows counts the rows keyed by the subtree's chunk ids (node annotations)
// or by the entry's node id (node statistics and series: a concept's chunk id, a
// class's name).
func (d *Document) countSubtreeRows(ctx context.Context, key sqlmem.ScopeKey, p *plannedChange) error {
	ann := 0
	var annSample []string
	for start := 0; start < len(p.subtree); start += ontologyCountConceptBatch {
		ph, ids := inPlaceholders(p.subtree[start:min(start+ontologyCountConceptBatch, len(p.subtree))])
		n, err := d.planCount(ctx, key, `SELECT count(*) FROM chunk_annotations WHERE chunk_id IN (`+ph+`)`, ids...)
		if err != nil {
			return fmt.Errorf("node_annotations: %w", err)
		}
		ann += n
		if n > 0 && len(annSample) < ontologyPlanSample {
			got, err := d.planIDs(ctx, key, `SELECT chunk_id FROM chunk_annotations WHERE chunk_id IN (`+ph+`) ORDER BY chunk_id`, ids...)
			if err != nil {
				return fmt.Errorf("node_annotations: %w", err)
			}
			annSample = append(annSample, got...)
		}
	}
	p.add("node_annotations", true, ann, nil, annSample)
	if len(p.concepts) == 0 && len(p.classes) == 0 {
		return nil
	}
	// A class's node id is its name; the rows a class match reads are narrowed to class
	// nodes (node_stats records the kind; a measure's concept node is an ontology chunk).
	classNodes := map[string]string{"node_stats": `kind = 'class'`, "measure_points": `node_id NOT IN (SELECT id FROM chunks)`}
	for _, table := range []string{"node_stats", "measure_points"} {
		var conds []string
		var args []any
		if len(p.concepts) > 0 {
			ph, a := inPlaceholders(p.concepts)
			conds, args = append(conds, `node_id IN (`+ph+`)`), append(args, a...)
		}
		if len(p.classes) > 0 {
			in, a, ok, err := d.planClassMatch(ctx, key, table, "node_id", classNodes[table], p.classes, false)
			if err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
			if ok {
				conds, args = append(conds, in), append(args, a...)
			}
		}
		if len(conds) == 0 {
			p.add(table, true, 0, nil, nil)
			continue
		}
		where := `(` + strings.Join(conds, " OR ") + `)`
		n, err := d.planCount(ctx, key, `SELECT count(*) FROM `+table+` WHERE `+where, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		var sample []string
		if n > 0 {
			if sample, err = d.planIDs(ctx, key, `SELECT DISTINCT node_id FROM `+table+` WHERE `+where+` ORDER BY node_id`, args...); err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
		}
		p.add(table, true, n, nil, sample)
	}
	return nil
}

// countInstanceSubjects counts the subjects the subtree's instances were adopted as
// (by the key recorded on the instance) and the statements about them.
func (d *Document) countInstanceSubjects(ctx context.Context, key sqlmem.ScopeKey, p *plannedChange) error {
	var keys []string
	for _, in := range p.instances {
		if in.registryKey != "" {
			keys = append(keys, in.registryKey)
		}
	}
	var subjects []string
	if len(keys) > 0 {
		ph, a := inPlaceholders(keys)
		res, err := d.planQuery(ctx, key, `SELECT chunk_id FROM chunk_memory_meta WHERE natural_key IN (`+ph+`) ORDER BY chunk_id`, a...)
		if err != nil {
			return fmt.Errorf("subject: %w", err)
		}
		for _, r := range res.Rows {
			subjects = append(subjects, asStr(r[0]))
		}
	}
	p.add("subject", true, len(subjects), nil, subjects)
	n, by := 0, map[string]int(nil)
	var sample []string
	if len(subjects) > 0 {
		ph, a := inPlaceholders(subjects)
		var err error
		if n, by, err = d.planStatusCounts(ctx, key, `e.to_id IN (`+ph+`)`, a); err != nil {
			return fmt.Errorf("facts_about: %w", err)
		}
		if n > 0 {
			if sample, err = d.planIDs(ctx, key, `SELECT e.id FROM chunk_edges e WHERE e.to_id IN (`+ph+`) ORDER BY e.id`, a...); err != nil {
				return fmt.Errorf("facts_about: %w", err)
			}
		}
	}
	p.add("facts_about", true, n, by, sample)
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sortedSample is up to ontologyPlanSample of set's keys, sorted.
func sortedSample(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > ontologyPlanSample {
		out = out[:ontologyPlanSample]
	}
	return out
}
