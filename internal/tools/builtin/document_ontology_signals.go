package builtin

// document_ontology_signals.go — Document op=ontology_signals (RFC GQ P6.1, 5.1): what
// the tenant's corpus uses and the ontology does not declare.
//
// THREE SIGNALS. Types on documents, chunks and facts that no class declares; relation
// kinds on statements that no relation declares; and statements of a declared kind the
// vocabulary gate marked ontology_valid:false, grouped by kind and the two ends' types.
// They are what an ontology is built from; this op only reads them.
//
// WHOLE TENANT, COUNTS ONLY. Types and kinds are stored by name in every scope, so the
// counts cover the tenant scope and every user and agent scope of the tenant, like the
// plan's name-keyed counts (document_ontology_plan_counts.go). Sample ids come from the
// tenant scope only, and no title or body is ever returned: the counts cover scopes the
// caller may not read.
//
// READS ONLY, AND NEVER PROVISIONS. The whole op runs under noWrites and is answered
// before the call's own scope is resolved and its schema ensured (executeGated), so it
// creates no plane, runs no migration and leaves no last-use stamp. No transaction is
// opened, and each read is materialised before the next: on SQLite a scope has one
// connection.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	ontologySignalsOp = "ontology_signals"
	// ontologySignalsGroups is the groups listed per signal when the call names no limit,
	// and the ends listed per undeclared kind; ontologySignalsMaxGroups is the most a
	// call may ask for.
	ontologySignalsGroups    = 20
	ontologySignalsMaxGroups = 100

	// The signals, as truncated[] names them.
	signalUndeclaredTypes = "undeclared_types"
	signalUndeclaredKinds = "undeclared_kinds"
	signalKindEnds        = "undeclared_kinds.ends"
	signalInvalidEnds     = "invalid_ends"

	// The populations an undeclared type is counted in. They do not overlap: a
	// document's root chunk carries the document's type and is counted as the document.
	signalPopDocument = "document"
	signalPopChunk    = "chunk"
	signalPopFact     = "fact"
)

// OntologySignals is op=ontology_signals' answer.
type OntologySignals struct {
	Ontology OntologySignalsOntology `json:"ontology"`
	// Complete is false when a user or agent scope was left out (ScopesSkipped): the
	// counts are then a lower bound.
	Complete        bool                   `json:"complete"`
	ScopesCounted   int                    `json:"scopes_counted"`
	ScopesSkipped   int                    `json:"scopes_skipped"`
	UndeclaredTypes []UndeclaredTypeSignal `json:"undeclared_types"`
	UndeclaredKinds []UndeclaredKindSignal `json:"undeclared_kinds"`
	InvalidEnds     []InvalidEndsSignal    `json:"invalid_ends"`
	// Truncated names each list that is not whole: cut at the group limit, or read from
	// a scope whose distinct values passed the store's row cap.
	Truncated []string `json:"truncated"`
}

// OntologySignalsOntology says which ontology the signals were read against.
type OntologySignalsOntology struct {
	DocumentID string `json:"document_id"`
	Confirmed  bool   `json:"confirmed"`
	// Fingerprint is the plan fingerprint (ontologyPlanFingerprint), "" with no document.
	Fingerprint string `json:"fingerprint"`
}

// SignalSample is one row a type was found on. ChunkID is empty for a document.
type SignalSample struct {
	ChunkID    string `json:"chunk_id,omitempty"`
	DocumentID string `json:"document_id"`
}

// UndeclaredTypeSignal is one type no class in force declares, in one population.
type UndeclaredTypeSignal struct {
	Type        string `json:"type"`
	Population  string `json:"population"`
	N           int    `json:"n"`
	TenantScope int    `json:"tenant_scope"`
	OtherScopes int    `json:"other_scopes"`
	// Documents is the documents holding the rows (for a document type, N itself).
	Documents int            `json:"documents"`
	Sample    []SignalSample `json:"sample"`
	// Proposal is "", "proposed" or "rejected": whether the ontology document already
	// holds a proposal of this name, and what became of it.
	Proposal string `json:"proposal"`
}

// SignalEnds is how many statements of a kind join two chunk types ("" for untyped).
type SignalEnds struct {
	From string `json:"from"`
	To   string `json:"to"`
	N    int    `json:"n"`
}

// UndeclaredKindSignal is one relation kind no relation in force declares.
type UndeclaredKindSignal struct {
	Kind        string         `json:"kind"`
	N           int            `json:"n"`
	ByStatus    map[string]int `json:"by_status"`
	TenantScope int            `json:"tenant_scope"`
	OtherScopes int            `json:"other_scopes"`
	Ends        []SignalEnds   `json:"ends"`
	Sample      []string       `json:"sample"`
	Proposal    string         `json:"proposal"`
}

// InvalidEndsSignal is the live statements of one declared kind, between two chunk
// types, that the vocabulary gate marked ontology_valid:false, with what the kind asks
// of its ends. The flag does not say which rule was broken, so a statement that broke a
// characteristic or a qualifier is counted here too.
type InvalidEndsSignal struct {
	Kind   string   `json:"kind"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	N      int      `json:"n"`
	Domain []string `json:"domain"`
	Range  []string `json:"range"`
	Sample []string `json:"sample"`
}

// ontologyGrant is the statement_methods value (code-js only) that lets a run read the
// signals and hold the ontologistLeaseName lease. It stamps nothing and grants no write.
const (
	ontologyGrant       = "ontology"
	ontologistLeaseName = "ontologist"
)

func hasOntologyGrant(ctx context.Context) bool {
	return contains(tools.PolicyOf(ctx).StatementMethods, ontologyGrant)
}

// ontologySignalsRefusal says who may read the signals: the operator plane, or a run
// holding the ontology grant (RFC GQ P6.2). The counts cover every user's scope, so a
// member and an ordinary run are refused. The one place the question is asked.
//
// A granted run is asked two more things, as a granted node_stats pass is: it must hold
// the tenant scope (the samples are tenant chunk ids), and its access-control rules must
// hide no document of the tenant scope (the counts and samples would otherwise describe
// documents it may not read). Neither question provisions: resolveScope only checks the
// grant, and the access-control list is read from the Path tree in the main store.
func (d *Document) ontologySignalsRefusal(ctx context.Context, key sqlmem.ScopeKey) (*tools.Result, error) {
	if tools.IsOperatorPlane(ctx) {
		return nil, nil
	}
	if !hasOntologyGrant(ctx) || tools.RunID(ctx) == "" {
		r := errPermission(ontologySignalsOp+": permission: the signals count every scope of the tenant, so only an operator, or a code agent granted ontology in statement_methods, reads them",
			"Ask an operator to run it. describe_ontology reads the ontology in force.")
		return &r, nil
	}
	if _, _, err := d.resolveScope(ctx, "tenant"); err != nil {
		r := errPermission(ontologySignalsOp+": permission: the signals sample the tenant scope, which this agent is not granted",
			"Ask an operator to grant the tenant scope in memory_scopes and sql_scopes.")
		return &r, nil
	}
	list, err := d.readableDocList(ctx, d.newDocACL(ctx, key))
	if err != nil {
		return nil, err
	}
	if !readsEverything(list) {
		r := errPermission(ontologySignalsOp+": permission: the signals count every document of the tenant scope, and this agent's access-control rules hide some",
			"Run it as an operator, or from an agent whose rules let it read the whole tenant scope.")
		return &r, nil
	}
	return nil, nil
}

// ontologySignals is Document op=ontology_signals. Always the tenant's: a scope other
// than tenant is refused rather than read as something else.
func (d *Document) ontologySignals(ctx context.Context, in docInput) (tools.Result, error) {
	if in.Scope != "" && in.Scope != "tenant" {
		return errValidation(ontologySignalsOp+": the signals are the tenant's; scope "+in.Scope+" has none",
			"Omit scope, or pass scope: tenant."), nil
	}
	if in.Limit < 0 || in.Limit > ontologySignalsMaxGroups {
		return errValidation(fmt.Sprintf("%s: limit must be between 1 and %d", ontologySignalsOp, ontologySignalsMaxGroups),
			fmt.Sprintf("Omit limit for %d groups per signal.", ontologySignalsGroups)), nil
	}
	limit := in.Limit
	if limit == 0 {
		limit = ontologySignalsGroups
	}
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return docFail(ontologySignalsOp, err), nil
	}
	refused, err := d.ontologySignalsRefusal(noWrites(ctx), key)
	if err != nil {
		return errFrom(ontologySignalsOp+": access control: "+err.Error(), err), nil
	}
	if refused != nil {
		return *refused, nil
	}
	out, err := d.ontologySignalsAt(noWrites(ctx), key, mscope, limit)
	if err != nil {
		return errFrom(ontologySignalsOp+": "+err.Error(), err), nil
	}
	return jsonResult(out)
}

// signalsVocabulary is what counts as declared: the classes and relation kinds in
// force, seed, reserved and retired ones included (a retired entry stays declared, RFC
// FO 3.7 B3), and the proposals the ontology document holds.
type signalsVocabulary struct {
	classes   map[string]bool // lower-cased: class names match under case folding
	kinds     map[string]bool
	relations map[string]memrank.OntologyRelation
	// classProposal and kindProposal map a proposed name to "proposed" or "rejected".
	classProposal, kindProposal map[string]string
}

func newSignalsVocabulary(read OntologyRead) signalsVocabulary {
	v := signalsVocabulary{classes: map[string]bool{}, kinds: map[string]bool{}, relations: map[string]memrank.OntologyRelation{},
		classProposal: map[string]string{}, kindProposal: map[string]string{}}
	for _, c := range memrank.EffectiveClasses(read.Terms, read.Confirmed) {
		v.classes[signalTypeKeyOf(c.Name)] = true
	}
	// Reserved and seed kinds are declared whatever the tenant's document says: the
	// runtime writes them (about, instance_of, focus, subject) in every tenant.
	for _, n := range memrank.ReservedRelationNames() {
		v.kinds[n] = true
	}
	for _, r := range memrank.SeedRelations() {
		v.kinds[r.Name] = true
	}
	for _, r := range memrank.EffectiveRelations(read.Relations, hasSection(read.Sections, memrank.SectionRelations), read.Confirmed) {
		v.kinds[r.Name], v.relations[r.Name] = true, r
		// A kind's former name is the linker's to re-point, not a kind to propose.
		if r.RenamedFrom != "" {
			v.kinds[r.RenamedFrom] = true
		}
	}
	for _, p := range read.Proposals {
		if p.Change != nil || p.Scheme != nil {
			continue // a change to an entry in force, or a whole scheme: not a new name
		}
		status := memrank.OntologyStatusProposed
		if strings.EqualFold(strings.TrimSpace(p.Status), memrank.OntologyStatusRejected) {
			status = memrank.OntologyStatusRejected
		}
		var into map[string]string
		name := p.Name
		switch p.Section {
		case "", memrank.SectionClasses:
			into, name = v.classProposal, signalTypeKeyOf(p.Name)
		case memrank.SectionRelations:
			into = v.kindProposal
		default:
			continue
		}
		// A rejection outlasts a later proposal of the same name: a person said no.
		if into[name] != memrank.OntologyStatusRejected {
			into[name] = status
		}
	}
	return v
}

// signalTypeKeyOf is the key a type is grouped and matched under. Folded here and not
// in SQL: lower() folds ASCII only on SQLite and Unicode on Postgres.
func signalTypeKeyOf(t string) string { return strings.ToLower(strings.TrimSpace(t)) }

// undeclaredType reports whether a stored type is one no class declares. The types the
// primitive and the runtime write themselves are nobody's to declare.
func (v signalsVocabulary) undeclaredType(t string) bool {
	return signalTypeKeyOf(t) != "" && !v.classes[signalTypeKeyOf(t)] &&
		!memrank.IsReservedStructuralType(t) && !memrank.IsOperationalDocumentType(t)
}

func (v signalsVocabulary) declaredKinds() []string {
	out := make([]string, 0, len(v.kinds))
	for k := range v.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// signalsScopeRead is one scope's grouped rows, read whole before any is counted, so a
// scope that fails halfway adds nothing.
type signalsScopeRead struct {
	types []signalTypeRow
	edges []signalEdgeRow
	cut   []string // the signals whose read hit the store's row cap
}

type signalTypeRow struct {
	population, typ string
	n, documents    int
}

type signalEdgeRow struct {
	kind, status, from, to string
	expired                bool
	n                      int
}

// ontologyInvalidExpr is true on alias e for a statement marked ontology_valid:false.
// attributes is JSON text on both tiers; SQLite's json_extract fails on text that is
// not JSON, so it is asked only of text that is (as judgeVerdictExpr does).
func (d *Document) ontologyInvalidExpr() string {
	if d.docSQL().Tier() == "postgres" {
		return `(CASE WHEN e.attributes IS NULL OR e.attributes = '' THEN NULL ELSE (e.attributes::jsonb ->> 'ontology_valid') END) = 'false'`
	}
	return `(CASE WHEN json_valid(e.attributes) THEN json_extract(e.attributes, '$.ontology_valid') END) = 0`
}

// signalChunkFrom keeps a document's root chunk out of the chunk and fact populations
// (it is joined as d): the root carries the document's own type.
const signalChunkFrom = ` FROM chunks c LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
	LEFT JOIN documents d ON d.root_chunk_id = c.id WHERE d.id IS NULL`

const signalEdgeFrom = ` FROM chunk_edges e LEFT JOIN chunks cf ON cf.id = e.from_id LEFT JOIN chunks ct ON ct.id = e.to_id`

// readSignalsScope reads one scope's three grouped counts: document types, chunk and
// fact types, and the statements that are of an undeclared kind or marked invalid (so
// a row of a declared kind is a marked one).
// Largest groups first, so a read the row cap cuts keeps the groups that matter.
func (d *Document) readSignalsScope(ctx context.Context, key sqlmem.ScopeKey, declaredKinds []string) (signalsScopeRead, error) {
	var out signalsScopeRead
	res, err := d.planQuery(ctx, key, `SELECT type, count(*) FROM documents WHERE type IS NOT NULL AND type <> ''
		GROUP BY 1 ORDER BY 2 DESC, 1`)
	if err != nil {
		return out, fmt.Errorf("document types: %w", err)
	}
	for _, r := range res.Rows {
		if len(r) >= 2 {
			out.types = append(out.types, signalTypeRow{signalPopDocument, asStr(r[0]), asInt(r[1]), asInt(r[1])})
		}
	}
	cut := res.Truncated

	res, err = d.planQuery(ctx, key, `SELECT c.type, CASE WHEN m.chunk_id IS NULL THEN 0 ELSE 1 END, count(*), count(DISTINCT c.document_id)`+
		signalChunkFrom+` AND c.type IS NOT NULL AND c.type <> '' GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2`)
	if err != nil {
		return out, fmt.Errorf("chunk types: %w", err)
	}
	for _, r := range res.Rows {
		if len(r) < 4 {
			continue
		}
		pop := signalPopChunk
		if asInt(r[1]) != 0 {
			pop = signalPopFact
		}
		out.types = append(out.types, signalTypeRow{pop, asStr(r[0]), asInt(r[2]), asInt(r[3])})
	}
	if cut || res.Truncated {
		out.cut = append(out.cut, signalUndeclaredTypes)
	}

	invalid := d.ontologyInvalidExpr()
	ph, args := inPlaceholders(declaredKinds)
	res, err = d.planQuery(ctx, key, `SELECT e.kind, e.status, CASE WHEN e.expired_at IS NULL THEN 0 ELSE 1 END,
		coalesce(cf.type, ''), coalesce(ct.type, ''), count(*)`+
		signalEdgeFrom+` WHERE e.kind NOT IN (`+ph+`) OR `+invalid+`
		GROUP BY 1, 2, 3, 4, 5 ORDER BY 6 DESC, 1, 2, 3, 4, 5`, args...)
	if err != nil {
		return out, fmt.Errorf("statements: %w", err)
	}
	for _, r := range res.Rows {
		if len(r) >= 6 {
			out.edges = append(out.edges, signalEdgeRow{kind: asStr(r[0]), status: asStr(r[1]), expired: asInt(r[2]) != 0,
				from: asStr(r[3]), to: asStr(r[4]), n: asInt(r[5])})
		}
	}
	if res.Truncated {
		out.cut = append(out.cut, signalUndeclaredKinds, signalInvalidEnds)
	}
	return out, nil
}

type signalTypeKey struct{ population, key string }

type signalTypeGroup struct {
	n, tenant, other, documents int
	spellings                   map[string]int // every stored spelling and its rows
	tenantSpellings             []string       // the tenant scope's, for the sample
}

type signalKindGroup struct {
	n, tenant, other int
	byStatus         map[string]int
	ends             map[[2]string]int
}

type signalEndsKey struct{ kind, from, to string }

type signalEndsGroup struct{ n, tenant int }

// signalsTally is the three signals folded over the scopes read so far.
type signalsTally struct {
	v       signalsVocabulary
	types   map[signalTypeKey]*signalTypeGroup
	kinds   map[string]*signalKindGroup
	invalid map[signalEndsKey]*signalEndsGroup
	cut     map[string]bool
}

// add folds one scope's rows in. tenant says the rows are the tenant scope's.
func (t *signalsTally) add(read signalsScopeRead, tenant bool) {
	side := func(n int, tenantN, otherN *int) {
		if tenant {
			*tenantN += n
		} else {
			*otherN += n
		}
	}
	for _, r := range read.types {
		if !t.v.undeclaredType(r.typ) {
			continue
		}
		k := signalTypeKey{r.population, signalTypeKeyOf(r.typ)}
		g := t.types[k]
		if g == nil {
			g = &signalTypeGroup{spellings: map[string]int{}}
			t.types[k] = g
		}
		g.n, g.documents = g.n+r.n, g.documents+r.documents
		g.spellings[r.typ] += r.n
		side(r.n, &g.tenant, &g.other)
		if tenant {
			g.tenantSpellings = appendUnique(g.tenantSpellings, r.typ)
		}
	}
	for _, r := range read.edges {
		if !t.v.kinds[r.kind] {
			g := t.kinds[r.kind]
			if g == nil {
				g = &signalKindGroup{byStatus: map[string]int{}, ends: map[[2]string]int{}}
				t.kinds[r.kind] = g
			}
			status := r.status
			if r.expired {
				status = "expired" // as the plan counts a retired statement
			}
			g.n += r.n
			g.byStatus[status] += r.n
			g.ends[[2]string{r.from, r.to}] += r.n
			side(r.n, &g.tenant, &g.other)
			continue
		}
		// A declared kind, so a statement the gate marked (the read returns no other).
		// Only the live ones: a rejected or retired one says nothing about what the
		// kind's ends should admit.
		if r.expired || r.status == edgeStatusRejected {
			continue
		}
		k := signalEndsKey{r.kind, r.from, r.to}
		g := t.invalid[k]
		if g == nil {
			g = &signalEndsGroup{}
			t.invalid[k] = g
		}
		g.n += r.n
		if tenant {
			g.tenant += r.n
		}
	}
	for _, s := range read.cut {
		t.cut[s] = true
	}
}

// ontologySignalsAt reads the signals of the tenant key names. ctx is under noWrites.
func (d *Document) ontologySignalsAt(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, limit int) (OntologySignals, error) {
	out := OntologySignals{UndeclaredTypes: []UndeclaredTypeSignal{}, UndeclaredKinds: []UndeclaredKindSignal{},
		InvalidEnds: []InvalidEndsSignal{}, Truncated: []string{}}
	// A tenant with no ontology document, or no tenant scope at all, is read against
	// the seed: that is the state an ontology is first built from.
	var read OntologyRead
	docID, err := d.resolveOntologyID(ctx, key)
	if err != nil && !signalsNothingThere(err) {
		return out, err
	}
	if docID != "" {
		if read, err = d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil); err != nil {
			return out, err
		}
		if out.Ontology.Fingerprint, err = d.ontologyPlanFingerprint(ctx, key, docID); err != nil {
			return out, err
		}
		out.Ontology.DocumentID, out.Ontology.Confirmed = docID, read.Confirmed
	}
	t := &signalsTally{v: newSignalsVocabulary(read), types: map[signalTypeKey]*signalTypeGroup{},
		kinds: map[string]*signalKindGroup{}, invalid: map[signalEndsKey]*signalEndsGroup{}, cut: map[string]bool{}}
	declared := t.v.declaredKinds()

	// The tenant scope is always read, outside the budget; its samples come from the
	// plane that holds its documents.
	tenantAt, tenantHeld, err := d.planScopePlane(ctx, key)
	if err != nil {
		return out, err
	}
	if tenantHeld {
		tr, err := d.readSignalsScope(ctx, tenantAt, declared)
		if err != nil {
			return out, err
		}
		t.add(tr, true)
	}

	scopes, err := d.SqlMem.ListTenantScopes(ctx, key.Tenant)
	if err != nil {
		return out, err
	}
	// The budget is a deadline on every read of the pass, as the plan's is: a slow
	// scope is cut mid-count and reported skipped.
	budget := d.planScopeBudget
	if budget <= 0 {
		budget = ontologyPlanScopeBudget
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, sk := range scopes {
		if sk.Tenant != key.Tenant || (sk.Scope != "user" && sk.Scope != "agent") {
			continue
		}
		if sctx.Err() != nil {
			out.ScopesSkipped++
			continue
		}
		at, ok, perr := d.planScopePlane(sctx, sk)
		if perr != nil {
			out.ScopesSkipped++
			slog.WarnContext(ctx, "ontology signals: could not check a scope", "tenant", sk.Tenant, "scope", sk.Scope, "err", perr)
			continue
		}
		if !ok {
			continue // neither plane holds documents: nothing to count
		}
		sr, serr := d.readSignalsScope(sctx, at, declared)
		if serr != nil {
			out.ScopesSkipped++
			slog.WarnContext(ctx, "ontology signals: could not count a scope", "tenant", sk.Tenant, "scope", sk.Scope, "err", serr)
			continue
		}
		t.add(sr, false)
		out.ScopesCounted++
	}
	out.Complete = out.ScopesSkipped == 0

	if err := d.fillSignalTypes(ctx, tenantAt, t, limit, &out); err != nil {
		return out, err
	}
	if err := d.fillSignalKinds(ctx, tenantAt, t, limit, &out); err != nil {
		return out, err
	}
	if err := d.fillSignalInvalidEnds(ctx, tenantAt, t, limit, &out); err != nil {
		return out, err
	}
	for _, s := range []string{signalUndeclaredTypes, signalUndeclaredKinds, signalKindEnds, signalInvalidEnds} {
		if t.cut[s] {
			out.Truncated = append(out.Truncated, s)
		}
	}
	return out, nil
}

// signalsNothingThere reports a tenant scope with no document plane or no document
// tables: there is no ontology to read, which is an answer and not a failure.
func signalsNothingThere(err error) bool {
	return schemaMissing(err) || errors.Is(err, sqlmem.ErrPlaneAbsent)
}

// fillSignalTypes lists the undeclared types, largest first, with the tenant scope's
// sample rows.
func (d *Document) fillSignalTypes(ctx context.Context, key sqlmem.ScopeKey, t *signalsTally, limit int, out *OntologySignals) error {
	for k, g := range t.types {
		// Shown under its most used spelling; the count covers every spelling.
		name, best := "", -1
		for s, n := range g.spellings {
			if n > best || (n == best && s < name) {
				name, best = s, n
			}
		}
		out.UndeclaredTypes = append(out.UndeclaredTypes, UndeclaredTypeSignal{Type: name, Population: k.population, N: g.n,
			TenantScope: g.tenant, OtherScopes: g.other, Documents: g.documents, Sample: []SignalSample{},
			Proposal: t.v.classProposal[k.key]})
	}
	sort.Slice(out.UndeclaredTypes, func(i, j int) bool {
		a, b := out.UndeclaredTypes[i], out.UndeclaredTypes[j]
		if a.N != b.N {
			return a.N > b.N
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Population < b.Population
	})
	if len(out.UndeclaredTypes) > limit {
		out.UndeclaredTypes, t.cut[signalUndeclaredTypes] = out.UndeclaredTypes[:limit], true
	}
	for i := range out.UndeclaredTypes {
		s := &out.UndeclaredTypes[i]
		g := t.types[signalTypeKey{s.Population, signalTypeKeyOf(s.Type)}]
		if g.tenant == 0 {
			continue
		}
		ph, args := inPlaceholders(g.tenantSpellings)
		stmt := `SELECT '', id FROM documents WHERE type IN (` + ph + `) ORDER BY id`
		switch s.Population {
		case signalPopChunk:
			stmt = `SELECT c.id, c.document_id` + signalChunkFrom + ` AND m.chunk_id IS NULL AND c.type IN (` + ph + `) ORDER BY c.id`
		case signalPopFact:
			stmt = `SELECT c.id, c.document_id` + signalChunkFrom + ` AND m.chunk_id IS NOT NULL AND c.type IN (` + ph + `) ORDER BY c.id`
		}
		res, err := d.planQuery(ctx, key, stmt+` LIMIT `+fmt.Sprint(ontologyPlanSample), args...)
		if err != nil {
			return fmt.Errorf("%s sample: %w", signalUndeclaredTypes, err)
		}
		for _, r := range res.Rows {
			if len(r) >= 2 {
				s.Sample = append(s.Sample, SignalSample{ChunkID: asStr(r[0]), DocumentID: asStr(r[1])})
			}
		}
	}
	return nil
}

// fillSignalKinds lists the undeclared kinds, largest first, each with the types its
// statements join and the tenant scope's sample statements.
func (d *Document) fillSignalKinds(ctx context.Context, key sqlmem.ScopeKey, t *signalsTally, limit int, out *OntologySignals) error {
	for kind, g := range t.kinds {
		s := UndeclaredKindSignal{Kind: kind, N: g.n, ByStatus: g.byStatus, TenantScope: g.tenant, OtherScopes: g.other,
			Ends: make([]SignalEnds, 0, len(g.ends)), Sample: []string{}, Proposal: t.v.kindProposal[kind]}
		for e, n := range g.ends {
			s.Ends = append(s.Ends, SignalEnds{From: e[0], To: e[1], N: n})
		}
		sort.Slice(s.Ends, func(i, j int) bool {
			a, b := s.Ends[i], s.Ends[j]
			if a.N != b.N {
				return a.N > b.N
			}
			if a.From != b.From {
				return a.From < b.From
			}
			return a.To < b.To
		})
		if len(s.Ends) > ontologySignalsGroups {
			s.Ends, t.cut[signalKindEnds] = s.Ends[:ontologySignalsGroups], true
		}
		out.UndeclaredKinds = append(out.UndeclaredKinds, s)
	}
	sort.Slice(out.UndeclaredKinds, func(i, j int) bool {
		a, b := out.UndeclaredKinds[i], out.UndeclaredKinds[j]
		if a.N != b.N {
			return a.N > b.N
		}
		return a.Kind < b.Kind
	})
	if len(out.UndeclaredKinds) > limit {
		out.UndeclaredKinds, t.cut[signalUndeclaredKinds] = out.UndeclaredKinds[:limit], true
	}
	for i := range out.UndeclaredKinds {
		s := &out.UndeclaredKinds[i]
		if s.TenantScope == 0 {
			continue
		}
		ids, err := d.planIDs(ctx, key, `SELECT e.id FROM chunk_edges e WHERE e.kind = ? ORDER BY e.id`, s.Kind)
		if err != nil {
			return fmt.Errorf("%s sample: %w", signalUndeclaredKinds, err)
		}
		s.Sample = append(s.Sample, ids...)
	}
	return nil
}

// fillSignalInvalidEnds lists the marked statements of declared kinds by kind and ends,
// largest first, with the kind's domain and range and the tenant scope's samples.
func (d *Document) fillSignalInvalidEnds(ctx context.Context, key sqlmem.ScopeKey, t *signalsTally, limit int, out *OntologySignals) error {
	for k, g := range t.invalid {
		rel := t.v.relations[k.kind]
		out.InvalidEnds = append(out.InvalidEnds, InvalidEndsSignal{Kind: k.kind, From: k.from, To: k.to, N: g.n,
			Domain: append([]string{}, rel.Domain...), Range: append([]string{}, rel.Range...), Sample: []string{}})
	}
	sort.Slice(out.InvalidEnds, func(i, j int) bool {
		a, b := out.InvalidEnds[i], out.InvalidEnds[j]
		if a.N != b.N {
			return a.N > b.N
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	if len(out.InvalidEnds) > limit {
		out.InvalidEnds, t.cut[signalInvalidEnds] = out.InvalidEnds[:limit], true
	}
	for i := range out.InvalidEnds {
		s := &out.InvalidEnds[i]
		if t.invalid[signalEndsKey{s.Kind, s.From, s.To}].tenant == 0 {
			continue
		}
		ids, err := d.planIDs(ctx, key, `SELECT e.id`+signalEdgeFrom+` WHERE e.kind = ? AND coalesce(cf.type, '') = ? AND coalesce(ct.type, '') = ?
			AND `+d.ontologyInvalidExpr()+` AND e.expired_at IS NULL AND e.status <> '`+edgeStatusRejected+`' ORDER BY e.id`,
			s.Kind, s.From, s.To)
		if err != nil {
			return fmt.Errorf("%s sample: %w", signalInvalidEnds, err)
		}
		s.Sample = append(s.Sample, ids...)
	}
	return nil
}
