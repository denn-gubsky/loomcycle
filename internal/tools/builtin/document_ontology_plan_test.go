package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// planOntologyMD has an entry of every section: a class with a subclass and an entity
// class, a relation kind with a sub-kind, a scheme of concepts projecting the class, an
// instance of the entity class.
const planOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\nA rule a regulator sets.\n\n#### Article\n\n### Company\n\n" +
	"## Relations\n@section relations\n\n### filed_under\nFiled under another.\n\n#### filed_mainly_under\n\n### cites\n\n" +
	"## Schemes\n@section schemes\n\n### topics\n@projects Provision\n\n" +
	"#### Risk Management\n@retention 7y\n\n##### outsourcing\n\n#### reporting\n\n" +
	"## Instances\n@section instances\n\n### Acme\n@a Company\n"

// planFixture is a tenant with planOntologyMD confirmed, documents in its tenant,
// user and agent scopes, and statements of every kind the plan counts.
type planFixture struct {
	t        *testing.T
	d        *Document
	op       context.Context // the tenant's operator
	user     context.Context // a run of user u1 with an agent, in the same tenant
	entries  map[string]string
	tenantID string
	// Documents and statements the counts are checked against.
	tenantDocs, userDocs, agentDocs []string
}

func newPlanFixture(t *testing.T, d *Document, base context.Context) *planFixture {
	t.Helper()
	base = tools.UpdateRunPolicy(base, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user", "agent"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user", "agent"}}
	})
	op := installTenantOntology(t, d, base, planOntologyMD, memrank.OntologyConfirmedStatus)
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user", "agent"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user", "agent"}}
		rp.Tools = []string{"*"}
	})
	if _, err := d.AdoptInstances(op, false); err != nil {
		t.Fatalf("adopt instances: %v", err)
	}
	read, err := d.OntologyTermsFromTree(op, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatal(err)
	}
	f := &planFixture{t: t, d: d, op: op, user: base, entries: map[string]string{}, tenantID: sqlScopeTenant(op)}
	for id, e := range planEntries(read) {
		f.entries[e.Kind+":"+e.Name] = id
	}
	for _, want := range []string{"class:Provision", "class:Article", "class:Company", "relation:filed_under",
		"relation:filed_mainly_under", "relation:cites", "scheme:topics", "concept:Risk Management",
		"concept:outsourcing", "concept:reporting", "instance:Acme"} {
		if f.entries[want] == "" {
			t.Fatalf("fixture entry %s missing: %v", want, f.entries)
		}
	}
	return f
}

func (f *planFixture) exec(ctx context.Context, v map[string]any) map[string]any {
	f.t.Helper()
	b, _ := json.Marshal(v)
	r, err := f.d.Execute(ctx, b)
	if err != nil || r.IsError {
		f.t.Fatalf("%v: %v %s", v["op"], err, r.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(r.Text), &out)
	return out
}

// doc creates a document of typ in scope and returns its id and a chunk of it.
func (f *planFixture) doc(ctx context.Context, scope, typ string) (string, string) {
	f.t.Helper()
	v := map[string]any{"op": "create_document", "scope": scope, "title": "Doc"}
	if typ != "" {
		v["type"] = typ
	}
	id := f.exec(ctx, v)["document_id"].(string)
	ch := f.exec(ctx, map[string]any{"op": "create_chunk", "scope": scope, "document_id": id, "title": "Part", "body": "text"})["id"].(string)
	return id, ch
}

func (f *planFixture) link(from, to, kind string) string {
	f.t.Helper()
	id, _ := f.exec(f.op, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": from, "to_id": to, "kind": kind})["id"].(string)
	return id
}

func (f *planFixture) propose(from, to, kind string) string {
	f.t.Helper()
	id, _ := f.exec(f.op, map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": from, "to_id": to, "kind": kind})["id"].(string)
	if id == "" {
		f.t.Fatalf("propose_statement returned no id")
	}
	return id
}

// populate writes the corpus every count is checked against.
func (f *planFixture) populate() {
	f.t.Helper()
	rm, out, rep := f.entries["concept:Risk Management"], f.entries["concept:outsourcing"], f.entries["concept:reporting"]
	// Tenant scope: Provision, Article and lower-case provision documents, about concepts.
	a, aCh := f.doc(f.op, "tenant", "Provision")
	b, bCh := f.doc(f.op, "tenant", "Article")
	c, cCh := f.doc(f.op, "tenant", "provision")
	f.tenantDocs = []string{a, b, c}
	f.link(aCh, rm, "subject")
	f.link(bCh, out, "subject")
	f.link(cCh, rep, "subject")
	f.reject(f.propose(cCh, out, "subject"))
	f.propose(aCh, out, "subject")
	// Statements of the relation kinds, in the tenant scope.
	f.link(aCh, bCh, "filed_under")
	f.link(bCh, cCh, "filed_mainly_under")
	f.link(cCh, aCh, "cites")
	// User and agent scopes: typed documents and an entity of the Company class.
	for _, scope := range []string{"user", "agent"} {
		id, _ := f.doc(f.user, scope, "Provision")
		if scope == "user" {
			f.userDocs = append(f.userDocs, id)
		} else {
			f.agentDocs = append(f.agentDocs, id)
		}
	}
	ud, udCh := f.doc(f.user, "user", "")
	f.exec(f.user, map[string]any{"op": "upsert_chunk", "scope": "user", "document_id": ud, "natural_key": "company:beta",
		"type": "Company", "subject": "Beta", "title": "Beta", "body": "a company"})
	f.exec(f.user, map[string]any{"op": "add_tags", "scope": "user", "id": udCh, "tags": []string{"topics/risk-management/outsourcing"}})
}

func (f *planFixture) reject(id string) {
	f.t.Helper()
	f.exec(f.op, map[string]any{"op": "reject_statement", "scope": "tenant", "id": id, "reason": "not about it"})
}

func (f *planFixture) plan(changes ...connector.OntologyChange) connector.PlanOntologyResult {
	f.t.Helper()
	res, err := f.d.PlanOntology(f.op, changes)
	if err != nil {
		f.t.Fatalf("PlanOntology(%+v): %v", changes, err)
	}
	if len(res.Changes) != len(changes) {
		f.t.Fatalf("plan has %d changes, want %d", len(res.Changes), len(changes))
	}
	return res
}

// count is the named count of a planned change; a missing one fails the test.
func planCount(t *testing.T, c connector.OntologyPlannedChange, name string) connector.OntologyPlanCount {
	t.Helper()
	got, ok := c.Counts[name]
	if !ok {
		t.Fatalf("%s %s has no %s count: %+v", c.Op, c.Entry.Name, name, c.Counts)
	}
	return got
}

// direct runs a count(*) in a scope key.
func (f *planFixture) direct(key sqlmem.ScopeKey, stmt string, args ...any) int {
	f.t.Helper()
	res, err := f.d.docSQL().Query(context.Background(), key, f.d.docSQL().Rebind(stmt), args)
	if err != nil {
		f.t.Fatalf("%s: %v", stmt, err)
	}
	return asInt(res.Rows[0][0])
}

func (f *planFixture) tenantKey() sqlmem.ScopeKey {
	return sqlmem.ScopeKey{Tenant: f.tenantID, Scope: "tenant", ScopeID: f.tenantID}
}

func (f *planFixture) userKey() sqlmem.ScopeKey {
	return sqlmem.ScopeKey{Tenant: f.tenantID, Scope: "user", ScopeID: "u1"}
}

// TestOntologyPlan_CountsEqualDirectQueries — for each change kind, the plan's counts
// equal the rows a direct query finds on the fixture (RFC GQ check P1), on both tiers.
func TestOntologyPlan_CountsEqualDirectQueries(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertPlanCountsEqualDirectQueries(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertPlanCountsEqualDirectQueries(t, d, ctx)
	})
}

func assertPlanCountsEqualDirectQueries(t *testing.T, d *Document, base context.Context) {
	f := newPlanFixture(t, d, base)
	f.populate()
	tk := f.tenantKey()

	// Delete of a class: typed rows across scopes, case-insensitively, subclasses included.
	del := f.plan(connector.OntologyChange{Op: connector.OntologyPlanDelete, Entry: f.entries["class:Provision"]}).Changes[0]
	wantTenantTyped := f.direct(tk, `SELECT count(*) FROM documents WHERE lower(type) IN ('provision', 'article')`)
	if got := planCount(t, del, "typed_documents"); got.TenantScope != wantTenantTyped || got.TenantScope != 3 ||
		got.OtherScopes != 2 || got.N != 5 || got.Effect != connector.OntologyEffectOrphaned {
		t.Errorf("typed_documents = %+v, want tenant %d (3) + other 2 orphaned", got, wantTenantTyped)
	}
	if got := planCount(t, del, "projects"); got.N != 1 || got.Sample[0] != f.entries["scheme:topics"] {
		t.Errorf("projects = %+v, want the topics scheme", got)
	}
	if del.Risk != connector.OntologyRiskDestroys || len(del.Subtree) != 1 || del.Subtree[0].Name != "Article" {
		t.Errorf("delete Provision: risk %s subtree %+v, want destroys with Article", del.Risk, del.Subtree)
	}

	// Retire of a relation kind: its statements and its sub-kind's, per status.
	ret := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["relation:filed_under"]}).Changes[0]
	wantKind := f.direct(tk, `SELECT count(*) FROM chunk_edges WHERE kind IN ('filed_under', 'filed_mainly_under')`)
	if got := planCount(t, ret, "statements_of_kind"); got.N != wantKind || wantKind != 2 || got.Effect != connector.OntologyEffectStale {
		t.Errorf("statements_of_kind = %+v, want %d stale", got, wantKind)
	}
	if ret.Risk != connector.OntologyRiskStales {
		t.Errorf("retire filed_under risk = %s, want stales", ret.Risk)
	}

	// Move of a concept: its members and the policy it would leave behind.
	mv := f.plan(connector.OntologyChange{Op: connector.OntologyPlanMove, Entry: f.entries["concept:outsourcing"],
		Parent: f.entries["concept:reporting"]}).Changes[0]
	wantMembers := f.direct(tk, `SELECT count(DISTINCT c.document_id) FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
		WHERE e.to_id = ? AND e.kind = 'subject' AND e.status = 'asserted' AND e.expired_at IS NULL`, f.entries["concept:outsourcing"])
	if got := planCount(t, mv, "members"); got.N != wantMembers || wantMembers != 1 || got.Effect != connector.OntologyEffectStale {
		t.Errorf("members = %+v, want %d stale", got, wantMembers)
	}
	if got := planCount(t, mv, "policy_changes"); got.N != 1 {
		t.Errorf("policy_changes = %+v, want outsourcing's inherited retention to change", got)
	}
	if got := planCount(t, mv, "concept_tags"); got.OtherScopes != 1 || got.TenantScope != 0 {
		t.Errorf("concept_tags = %+v, want the one user-scope document (the ontology's own mirror tags left out)", got)
	}

	// Rename of the entity class: identity of its subjects in every scope.
	rn := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: f.entries["class:Company"], Name: "Firm"}).Changes[0]
	wantSubjects := f.direct(tk, `SELECT count(*) FROM chunk_memory_meta WHERE substr(lower(natural_key), 1, 8) = 'company:'`) +
		f.direct(f.userKey(), `SELECT count(*) FROM chunk_memory_meta WHERE substr(lower(natural_key), 1, 8) = 'company:'`)
	if got := planCount(t, rn, "subject_keys"); got.N != wantSubjects || got.TenantScope != 1 || got.OtherScopes != 1 {
		t.Errorf("subject_keys = %+v, want %d (the adopted Acme + Beta in the user scope)", got, wantSubjects)
	}
	if got := planCount(t, rn, "instances"); got.N != 1 {
		t.Errorf("instances = %+v, want Acme", got)
	}
	if rn.Risk != connector.OntologyRiskStales || len(rn.Notes) == 0 {
		t.Errorf("rename Company: risk %s notes %v, want stales and the rename-job note", rn.Risk, rn.Notes)
	}

	// Edit: a meaning-only edit keeps everything; an edit adding @functional counts the
	// from-ends with two targets of the kind.
	meaning := f.plan(connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: f.entries["class:Provision"], Body: "A rule a supervisor sets.\n"}).Changes[0]
	if meaning.Risk != connector.OntologyRiskNone || planCount(t, meaning, "typed_documents").Effect != connector.OntologyEffectKept {
		t.Errorf("meaning edit: risk %s, typed_documents %+v; want none and kept", meaning.Risk, planCount(t, meaning, "typed_documents"))
	}
	cites := f.entries["relation:cites"]
	tenantDoc := f.tenantDocs[0]
	extra := f.exec(f.op, map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": tenantDoc, "title": "More", "body": "x"})["id"].(string)
	var from string
	res, err := f.d.docSQL().Query(context.Background(), tk, `SELECT from_id FROM chunk_edges WHERE kind = 'cites'`, nil)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("cites statement: %v %v", err, res)
	}
	from = asStr(res.Rows[0][0])
	f.link(from, extra, "cites")
	fn := f.plan(connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: cites, Body: "@functional\n"}).Changes[0]
	if got := planCount(t, fn, "characteristic_breaks"); got.N != 1 || got.Effect != connector.OntologyEffectStale || fn.Risk != connector.OntologyRiskStales {
		t.Errorf("edit cites @functional: breaks %+v risk %s, want 1 stale / stales", got, fn.Risk)
	}

	// Retire of an instance: its adopted subject and what is said about it.
	inst := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["instance:Acme"]}).Changes[0]
	read, err := f.d.OntologyTermsFromTree(f.op, "tenant", memrank.OntologyPath)
	if err != nil || len(read.Instances) != 1 || read.Instances[0].RegistryKey == "" {
		t.Fatalf("adopted instance: %v %+v", err, read.Instances)
	}
	wantSubject := f.direct(tk, `SELECT count(*) FROM chunk_memory_meta WHERE natural_key = ?`, read.Instances[0].RegistryKey)
	if got := planCount(t, inst, "subject"); got.N != wantSubject || wantSubject != 1 {
		t.Errorf("subject = %+v, want %d", got, wantSubject)
	}
}

// TestOntologyPlan_WritesNothing — a plan of every op leaves the ontology's fingerprint,
// every document table's row count in the tenant and user scopes, the set of scopes,
// and the scopes' own state exactly as they were (RFC GQ check P1). The plan runs in a
// fresh process (a Manager that has provisioned and touched nothing) with scope GC on,
// so a last-use stamp shows (sqlite: a file's mtime; postgres: a scope_access row) and
// so does the provisioning DDL a first read per process runs on postgres (a role
// setting changed behind its back is reset by it). The tenant scope has no role record,
// so a resolve that migrates would write one.
func TestOntologyPlan_WritesNothing(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		root := t.TempDir()
		d, ctx, _ := documentFixture(t)
		mgr, err := sqlmem.New(sqlmem.Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mgr.Close() })
		d.SqlMem = mgr
		fresh, err := sqlmem.New(sqlmem.Config{Root: root, ScopeTTLMS: 3600_000})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fresh.Close() })
		prepare, traces := sqliteScopeTraces(t, root)
		assertPlanWritesNothing(t, d, ctx, fresh, prepare, traces)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
		fresh, err := sqlmem.NewPostgres(context.Background(),
			sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000, ScopeTTLMS: 3600_000})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fresh.Close() })
		raw, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		prepare, traces := pgScopeTraces(t, raw)
		assertPlanWritesNothing(t, d, ctx, fresh, prepare, traces)
	})
}

// sqliteScopeTraces backdates every scope file and reports each file's mtime.
func sqliteScopeTraces(t *testing.T, root string) (prepare func(), traces func() string) {
	files := func() []string {
		var out []string
		_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(path, ".db") {
				out = append(out, path)
			}
			return nil
		})
		sort.Strings(out)
		return out
	}
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	prepare = func() {
		for _, f := range files() {
			if err := os.Chtimes(f, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	traces = func() string {
		var b strings.Builder
		for _, f := range files() {
			info, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s=%d|", filepath.Base(f), info.ModTime().UnixNano())
		}
		return b.String()
	}
	return prepare, traces
}

// pgScopeTraces sets a role setting provisioning resets on every scope role, and
// reports the roles' settings and the scope_access rows.
func pgScopeTraces(t *testing.T, raw *sql.DB) (prepare func(), traces func() string) {
	ctx := context.Background()
	roles := func() []string {
		rows, err := raw.QueryContext(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'sqlmem\_%' ORDER BY rolname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	prepare = func() {
		for _, r := range roles() {
			if _, err := raw.ExecContext(ctx, `ALTER ROLE "`+r+`" SET work_mem TO '1MB'`); err != nil {
				t.Fatal(err)
			}
		}
	}
	traces = func() string {
		var b strings.Builder
		for _, r := range roles() {
			var cfg string
			_ = raw.QueryRowContext(ctx, `SELECT coalesce(array_to_string(s.setconfig, ','), '')
				FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole WHERE r.rolname = $1`, r).Scan(&cfg)
			fmt.Fprintf(&b, "%s{%s}|", r, cfg)
		}
		var touched int
		if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM sqlmem_meta.scope_access`).Scan(&touched); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "scope_access=%d", touched)
		return b.String()
	}
	return prepare, traces
}

func assertPlanWritesNothing(t *testing.T, d *Document, base context.Context, fresh *sqlmem.Manager, prepare func(), traces func() string) {
	f := newPlanFixture(t, d, base)
	f.populate()
	docID := mustOntologyID(t, d, f)
	if _, err := d.docSQL().Exec(context.Background(), f.tenantKey(), `DELETE FROM document_roles`, nil, 0); err != nil {
		t.Fatalf("drop the role record: %v", err)
	}
	snapshot := func() string {
		fp, err := d.ontologyFingerprint(f.op, f.tenantKey(), docID)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString(fp)
		scopes, err := d.SqlMem.ListScopes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "|scopes=%d", len(scopes))
		for _, key := range []sqlmem.ScopeKey{f.tenantKey(), f.userKey()} {
			for _, table := range documentTableNames {
				res, err := d.docSQL().Query(context.Background(), key, `SELECT count(*) FROM `+table, nil)
				if err != nil {
					continue // an index name, or a table this scope never made
				}
				fmt.Fprintf(&b, "|%s.%s=%d", key.Scope, table, asInt(res.Rows[0][0]))
			}
		}
		return b.String()
	}
	was := snapshot()
	if !strings.Contains(was, "tenant.chunk_edges=") || !strings.Contains(was, "user.documents=") ||
		!strings.Contains(was, "tenant.document_roles=0") {
		t.Fatalf("the snapshot reads no tables, or the role record is still there: %s", was)
	}
	prepare()
	wasTraces := traces()
	planner := &Document{Store: d.Store, SqlMem: fresh, Bus: d.Bus}
	changes := []connector.OntologyChange{
		{Op: connector.OntologyPlanDelete, Entry: f.entries["scheme:topics"]},
		{Op: connector.OntologyPlanRetire, Entry: f.entries["relation:filed_under"]},
		{Op: connector.OntologyPlanRename, Entry: f.entries["class:Provision"], Name: "Regulation"},
		{Op: connector.OntologyPlanMove, Entry: f.entries["class:Company"], Parent: f.entries["class:Provision"]},
		{Op: connector.OntologyPlanEdit, Entry: f.entries["relation:cites"], Body: "@functional\n@asymmetric\n"},
		{Op: connector.OntologyPlanDelete, Entry: f.entries["instance:Acme"]},
	}
	res, err := planner.PlanOntology(f.op, changes)
	if err != nil {
		t.Fatalf("PlanOntology: %v", err)
	}
	if now := traces(); now != wasTraces {
		t.Errorf("the plan left a trace on the scopes (a last-use stamp or provisioning DDL):\nbefore %s\nafter  %s", wasTraces, now)
	}
	if now := snapshot(); now != was {
		t.Errorf("the plan wrote:\nbefore %s\nafter  %s", was, now)
	}
	if !res.Complete || res.ScopesCounted < 2 {
		t.Errorf("plan counted %d scopes, complete %v; want the user and agent scopes counted", res.ScopesCounted, res.Complete)
	}
	fp, _ := d.ontologyFingerprint(f.op, f.tenantKey(), docID)
	if res.Fingerprint == "" || res.Fingerprint == fp || len(res.Fingerprint) != 64 {
		t.Errorf("fingerprint %q: want the sha256 hex of the stored fingerprint, not the raw one", res.Fingerprint)
	}
	again, err := planner.PlanOntology(f.op, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: f.entries["relation:cites"]}})
	if err != nil || again.Fingerprint != res.Fingerprint {
		t.Errorf("two plans of an unchanged ontology have fingerprints %s and %s (%v)", res.Fingerprint, again.Fingerprint, err)
	}
}

func mustOntologyID(t *testing.T, d *Document, f *planFixture) string {
	t.Helper()
	id, err := d.resolveOntologyID(f.op, f.tenantKey())
	if err != nil || id == "" {
		t.Fatalf("ontology id: %q %v", id, err)
	}
	return id
}

// TestOntologyPlan_DeleteOfConceptDestroysItsSubtreeStatements — deleting a concept
// counts every statement to it and to its narrower concepts, proposed and rejected
// included (a delete removes them all), as destroyed.
func TestOntologyPlan_DeleteOfConceptDestroysItsSubtreeStatements(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	f.populate()
	c := f.plan(connector.OntologyChange{Op: connector.OntologyPlanDelete, Entry: f.entries["concept:Risk Management"]}).Changes[0]
	got := planCount(t, c, "statements")
	// Risk Management 1 asserted; outsourcing 1 asserted, 1 proposed, 1 rejected.
	if got.N != 4 || got.ByStatus["asserted"] != 2 || got.ByStatus["proposed"] != 1 || got.ByStatus["rejected"] != 1 ||
		got.Effect != connector.OntologyEffectDestroyed {
		t.Errorf("statements = %+v, want 4 destroyed (2 asserted, 1 proposed, 1 rejected)", got)
	}
	if c.Risk != connector.OntologyRiskDestroys || len(c.Subtree) != 1 || c.Subtree[0].Name != "outsourcing" {
		t.Errorf("risk %s subtree %+v, want destroys with outsourcing", c.Risk, c.Subtree)
	}
	if m := planCount(t, c, "members"); m.N != 2 || m.Effect != connector.OntologyEffectDestroyed {
		t.Errorf("members = %+v, want the 2 documents placed under it", m)
	}
}

// TestOntologyPlan_DeleteOfRelationOrphansNotDestroys — a relation kind's statements
// name it by string: a delete leaves them behind with an undeclared kind. The plan
// reports them orphaned, destroys no statement, and still classes the delete destroys
// (RFC GQ 9c). The statements are there after the plan.
func TestOntologyPlan_DeleteOfRelationOrphansNotDestroys(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	f.populate()
	c := f.plan(connector.OntologyChange{Op: connector.OntologyPlanDelete, Entry: f.entries["relation:filed_under"]}).Changes[0]
	if got := planCount(t, c, "statements"); got.N != 0 {
		t.Errorf("statements keyed by the relation's chunk = %+v, want none", got)
	}
	kind := planCount(t, c, "statements_of_kind")
	if kind.N != 2 || kind.Effect != connector.OntologyEffectOrphaned || kind.ByStatus["asserted"] != 2 {
		t.Errorf("statements_of_kind = %+v, want 2 orphaned", kind)
	}
	if c.Risk != connector.OntologyRiskDestroys {
		t.Errorf("risk = %s, want destroys", c.Risk)
	}
	if n := f.direct(f.tenantKey(), `SELECT count(*) FROM chunk_edges WHERE kind IN ('filed_under', 'filed_mainly_under')`); n != 2 {
		t.Errorf("after the plan %d statements of the kind remain, want 2", n)
	}
}

// TestOntologyPlan_ClassCountsCoverUserAndAgentScopes — documents of a class in a user
// scope and an agent scope are counted, as counts only: no sample id comes from them.
func TestOntologyPlan_ClassCountsCoverUserAndAgentScopes(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	f.populate()
	res := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["class:Provision"]})
	got := planCount(t, res.Changes[0], "typed_documents")
	if got.OtherScopes != 2 {
		t.Errorf("typed_documents other_scopes = %d, want the user and the agent document", got.OtherScopes)
	}
	if res.ScopesCounted < 2 || !res.Complete || res.ScopesSkipped != 0 {
		t.Errorf("scopes counted %d skipped %d complete %v, want the user and agent scopes counted", res.ScopesCounted, res.ScopesSkipped, res.Complete)
	}
	for _, id := range got.Sample {
		for _, other := range append(append([]string{}, f.userDocs...), f.agentDocs...) {
			if id == other {
				t.Errorf("sample lists %s, a document of a user or agent scope", id)
			}
		}
	}
}

// TestOntologyPlan_ClassCountIsCaseInsensitive — a document typed "provision" is a
// Provision, as identity keys lower-case the class name.
func TestOntologyPlan_ClassCountIsCaseInsensitive(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	f.populate()
	c := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["class:Provision"]}).Changes[0]
	got := planCount(t, c, "typed_documents")
	lower := f.tenantDocs[2] // typed "provision"
	found := false
	for _, id := range got.Sample {
		found = found || id == lower
	}
	if got.TenantScope != 3 || !found {
		t.Errorf("typed_documents = %+v, want 3 in the tenant scope including the lower-case %s", got, lower)
	}
}

// TestOntologyPlan_RenameReportsIssuesOfTheChangedOntology — renaming a class a scheme
// projects reports the scheme's @projects as naming no class, as a new issue.
func TestOntologyPlan_RenameReportsIssuesOfTheChangedOntology(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	res := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: f.entries["class:Provision"], Name: "Regulation"})
	found := false
	for _, s := range res.NewIssues {
		found = found || (strings.Contains(s, `scheme "topics"`) && strings.Contains(s, `"Provision" is not a class`))
	}
	if !found {
		t.Errorf("new issues %v, want the topics scheme's @projects Provision", res.NewIssues)
	}
	for _, s := range res.Issues {
		if strings.Contains(s, "Regulation") {
			t.Errorf("issue %q names the new class as missing", s)
		}
	}
	unchanged := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["relation:cites"]})
	if len(unchanged.NewIssues) != 0 {
		t.Errorf("retiring an unreferenced kind reports new issues %v", unchanged.NewIssues)
	}
}

// TestOntologyPlan_UnknownEntryIsRefused — an id that is not an in-force entry (no
// such chunk, a section, the root) is refused as not found, never planned as "none".
func TestOntologyPlan_UnknownEntryIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	read, err := d.OntologyTermsFromTree(f.op, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"absent": "no-such-chunk", "section": read.SectionChunks[memrank.SectionClasses], "root": read.RootChunkID} {
		_, err := d.PlanOntology(f.op, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: id}})
		if !errors.Is(err, connector.ErrOntologyPlanNotFound) {
			t.Errorf("%s: err = %v, want ErrOntologyPlanNotFound", name, err)
		}
	}
	// A concept cannot move under a class.
	_, err = d.PlanOntology(f.op, []connector.OntologyChange{{Op: connector.OntologyPlanMove, Entry: f.entries["concept:reporting"], Parent: f.entries["class:Provision"]}})
	if !errors.Is(err, connector.ErrOntologyPlanNotFound) {
		t.Errorf("concept under a class: err = %v, want ErrOntologyPlanNotFound", err)
	}
	for name, ch := range map[string]connector.OntologyChange{
		"unknown op":       {Op: "merge", Entry: f.entries["class:Provision"]},
		"rename, no name":  {Op: connector.OntologyPlanRename, Entry: f.entries["class:Provision"]},
		"move, no parent":  {Op: connector.OntologyPlanMove, Entry: f.entries["class:Provision"]},
		"edit, empty body": {Op: connector.OntologyPlanEdit, Entry: f.entries["class:Provision"]},
	} {
		if _, err := d.PlanOntology(f.op, []connector.OntologyChange{ch}); !errors.Is(err, connector.ErrOntologyPlanInvalid) {
			t.Errorf("%s: err = %v, want ErrOntologyPlanInvalid", name, err)
		}
	}
	many := make([]connector.OntologyChange, ontologyPlanMaxChanges+1)
	for i := range many {
		many[i] = connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["relation:cites"]}
	}
	if _, err := d.PlanOntology(f.op, many); !errors.Is(err, connector.ErrOntologyPlanInvalid) {
		t.Errorf("%d changes: err = %v, want ErrOntologyPlanInvalid", len(many), err)
	}
}

// TestOntologyPlan_OtherTenantsScopesAreNotCounted — a user scope of another tenant
// holding documents of the same class name adds nothing to the plan.
func TestOntologyPlan_OtherTenantsScopesAreNotCounted(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	f.populate()
	before := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["class:Provision"]})
	other := tools.UpdateRunPolicy(tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u9", TenantID: "other"}),
		func(rp *tools.RunPolicy) {
			rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
			rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}}
		})
	for i := 0; i < 3; i++ {
		f.doc(other, "user", "Provision")
	}
	after := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["class:Provision"]})
	if b, a := planCount(t, before.Changes[0], "typed_documents"), planCount(t, after.Changes[0], "typed_documents"); a.N != b.N {
		t.Errorf("typed_documents %d → %d after another tenant wrote 3 Provision documents", b.N, a.N)
	}
	if after.ScopesCounted != before.ScopesCounted {
		t.Errorf("scopes counted %d → %d: another tenant's scope was counted", before.ScopesCounted, after.ScopesCounted)
	}
}

// TestOntologyPlan_CountsAPreSplitScopeOnItsAgentPlane — a user scope whose documents
// are still on its agent plane (from before the planes split: postgres moves such a
// scope only on its next use) is counted there, read-only: the plan neither skips it
// as empty nor moves it.
func TestOntologyPlan_CountsAPreSplitScopeOnItsAgentPlane(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		assertPlanCountsPreSplitScope(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		assertPlanCountsPreSplitScope(t, d, ctx)
	})
}

func assertPlanCountsPreSplitScope(t *testing.T, d *Document, base context.Context) {
	f := newPlanFixture(t, d, base)
	f.populate()
	ctx := context.Background()
	key := f.userKey()
	dump, err := d.SqlMem.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	dump.DocTail = nil
	if _, err := d.SqlMem.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := d.SqlMem.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	d.SqlMem.ForgetEnsured(key)
	preSplit := func() bool {
		ok, err := d.docSQL().PlaneExists(ctx, key)
		held, herr := d.docSQL().AgentPlaneTables(ctx, key, []string{"documents"})
		return err == nil && herr == nil && !ok && len(held) == 1
	}
	if !preSplit() {
		t.Fatal("the user scope is not in the pre-split layout")
	}
	res := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["class:Provision"]})
	if got := planCount(t, res.Changes[0], "typed_documents"); got.OtherScopes != 2 || !res.Complete {
		t.Errorf("typed_documents other_scopes = %d, complete %v; want the pre-split user document and the agent one, complete",
			got.OtherScopes, res.Complete)
	}
	if !preSplit() {
		t.Error("the plan moved the pre-split scope")
	}
}

// TestOntologyPlan_ScopeBudgetCutsTheCountAndSaysSo — once the scope budget is spent,
// the user and agent scopes left are reported skipped and the plan incomplete, with
// nothing of them counted; the tenant scope's counts stand.
func TestOntologyPlan_ScopeBudgetCutsTheCountAndSaysSo(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newPlanFixture(t, d, ctx)
	f.populate()
	d.planScopeBudget = time.Nanosecond
	res := f.plan(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: f.entries["class:Provision"]})
	got := planCount(t, res.Changes[0], "typed_documents")
	if res.Complete || res.ScopesCounted != 0 || res.ScopesSkipped != 2 || got.OtherScopes != 0 || got.TenantScope != 3 {
		t.Errorf("complete %v counted %d skipped %d, typed_documents %+v; want incomplete, both scopes skipped, the tenant's 3 counted",
			res.Complete, res.ScopesCounted, res.ScopesSkipped, got)
	}
}
