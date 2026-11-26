package builtin

// document_ontology_rename_job_test.go — RFC GQ P5.3 and P5.4: the class-rename job.
//
// Every test starts from an EXISTING state: rows are written under the class's name in
// the tenant, user and agent scopes (through the Document ops, or — for the figures a
// pass computes — straight into their tables, as an earlier pass left them), the
// ontology names the class in four other entries, and only then is the class renamed.
// Each positive has a minimal pair beside it: the class Memo is never renamed, and its
// rows must come out exactly as they went in.

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// renameOntologyMD: Bulletin is renamed to Circular in these tests; Memo is the class
// that is not; Notice has no rows (a rename nothing rests on). The ontology names
// Bulletin in Memo's ref field, a relation's @domain, a scheme's @projects and an
// instance's @a (RFC GQ 9b C12).
const renameOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Bulletin\nA notice sent to everyone.\n@identity\n\n" +
	"#### Memo\n@identity\n- `reply_to` ref(Bulletin)\n\n" +
	"#### Notice\n\n" +
	"## Relations\n@section relations\n\n" +
	"### supervises\n\n" +
	"### issued_by\n@domain Bulletin | Memo\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@projects Bulletin, Memo\n\n#### Safety\n\n" +
	"## Instances\n@section instances\n\n" +
	"### Head office\n@a Bulletin\n"

// renameScopeNames are the scopes the fixture writes in, as the Document tool names them.
var renameFixtureScopes = []string{"tenant", "user", "agent"}

type renameFixture struct {
	*retiredFixture
	key      sqlmem.ScopeKey // the tenant scope
	scopes   map[string]renameScope
	bulletin string // the class's chunk
	memo     string
}

// classificationAt is a classification record's time, past a float64's exact range so a
// rewrite that decoded the record into float64 would show.
const classificationAt = 1764000000123456789

// newRenameFixtureOn writes, in the tenant, user and agent scopes, rows under Bulletin
// in every column the job moves, and the same under Memo.
func newRenameFixtureOn(t *testing.T, d *Document, base context.Context) *renameFixture {
	t.Helper()
	all := func(ctx context.Context) context.Context {
		return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
			rp.Memory = tools.MemoryPolicyValue{AllowedScopes: renameFixtureScopes}
			rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: renameFixtureScopes}
		})
	}
	ctx := all(operatorCtx(installTenantOntology(t, d, base, renameOntologyMD, memrank.OntologyConfirmedStatus)))
	if _, err := d.AdoptInstances(ctx, false); err != nil {
		t.Fatalf("adopt instances: %v", err)
	}
	rf := &retiredFixture{t: t, d: d, ctx: ctx}
	f := &renameFixture{retiredFixture: rf, scopes: map[string]renameScope{}}
	for _, s := range renameFixtureScopes {
		key, mscope, err := d.resolveScope(ctx, s)
		if err != nil {
			t.Fatalf("scope %s: %v", s, err)
		}
		f.scopes[s] = renameScope{key: key, mscope: mscope, tenant: s == "tenant", name: s}
	}
	f.key = f.scopes["tenant"].key
	f.bulletin, f.memo = f.chunk("Bulletin"), f.chunk("Memo")
	for _, s := range renameFixtureScopes {
		f.seed(s)
	}
	for _, class := range []string{"Bulletin", "Memo"} {
		var doc string
		for _, row := range f.stringsIn(f.key, `SELECT root_chunk_id FROM documents WHERE title = ?`, "Weekly "+map[string]string{"Bulletin": "one", "Memo": "two"}[class]) {
			doc = row
		}
		f.sql(`INSERT INTO chunk_annotations (chunk_id, level, brief, abstract, language, class, inputs_sha256, prompt_version,
			manual, written_at, written_by) VALUES (?, 'document', 'b', 'a', 'en', ?, 'x', 1, 0, 1, 'test')`, doc, class)
		f.sql(`INSERT INTO node_stats (node_id, kind, members, members_with_vector, inputs_sha256, members_sha256, computed_at)
			VALUES (?, 'class', 2, 0, 'x', 'y', 1)`, class)
		f.sql(`INSERT INTO measure_points (measure, node_id, subject_id, period_start, period, value_sum, members, groups, sources,
			basis, fallback_share, inputs_sha256, members_sha256, computed_at) VALUES ('count', ?, '', 0, 'month', 2, 2, 1, 1, 2, 0, 'x', 'y', 1)`, class)
	}
	return f
}

// seed writes, in one scope, documents and a chunk typed with each class, a subject under
// the consolidator's key form, the define_type registry row, and classification records
// naming the class as applied and as proposed (in another case). Titles and paths carry
// a neutral word, never a class's name, so the census finds the name only where a class
// name is stored.
func (f *renameFixture) seed(scope string) {
	f.t.Helper()
	for class, lc := range map[string]string{"Bulletin": "one", "Memo": "two"} {
		doc := f.must(map[string]any{"op": "create_document", "scope": scope, "title": "Weekly " + lc, "type": class,
			"path": "/kb/weekly-" + lc})
		f.must(map[string]any{"op": "create_document", "scope": scope, "title": "Monthly " + lc, "type": class,
			"path": "/kb/monthly-" + lc})
		plain := f.must(map[string]any{"op": "create_document", "scope": scope, "title": "Plain " + lc, "path": "/kb/plain-" + lc})
		f.must(map[string]any{"op": "create_chunk", "scope": scope, "document_id": plain["document_id"], "title": "Inline " + lc,
			"type": class, "body": "A note."})
		f.must(map[string]any{"op": "create_document", "scope": scope, "title": "Staff " + lc, "type": class,
			"subject": "Staff " + lc, "natural_key": class + ":staff-" + lc, "path": "/facts/staff-" + lc})
		f.must(map[string]any{"op": "define_type", "scope": scope, "name": class})
		f.classify(scope, asStr(doc["root_chunk_id"]), "applied_type", class)
		f.classify(scope, asStr(plain["root_chunk_id"]), "proposed_type", strings.ToLower(class))
	}
}

// classify writes a classification record on a root, as classify leaves one.
func (f *renameFixture) classify(scope, root, field, class string) {
	f.t.Helper()
	g := f.must(map[string]any{"op": "get_chunk", "scope": scope, "id": root})
	rec := json.RawMessage(`{"by":"classify","at":` + renameJSONInt(classificationAt) + `,"` + field + `":"` + class + `"}`)
	f.must(map[string]any{"op": "update_chunk", "scope": scope, "id": root, "revision": int(g["revision"].(float64)),
		"fields": map[string]any{classifyField: rec}})
}

func renameJSONInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func newRenameFixture(t *testing.T) *renameFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	return newRenameFixtureOn(t, d, base)
}

func (f *renameFixture) sql(stmt string, args ...any) {
	f.t.Helper()
	if err := f.d.exec(f.ctx, f.key, stmt, args...); err != nil {
		f.t.Fatalf("%s: %v", stmt, err)
	}
}

func (f *renameFixture) strings(stmt string, args ...any) []string {
	f.t.Helper()
	return f.stringsIn(f.key, stmt, args...)
}

// stringsIn is the sorted rows of a read in one scope key, each row's values joined.
func (f *renameFixture) stringsIn(key sqlmem.ScopeKey, stmt string, args ...any) []string {
	f.t.Helper()
	res, err := f.d.query(f.ctx, key, stmt, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", stmt, err)
	}
	out := []string{}
	for _, r := range res.Rows {
		var parts []string
		for _, v := range r {
			parts = append(parts, asStr(v))
		}
		out = append(out, strings.Join(parts, "→"))
	}
	sort.Strings(out)
	return out
}

// classifications is every classification record of a scope's documents, as
// "<title>→<applied>→<proposed>→<at>".
func (f *renameFixture) classifications(sc renameScope) []string {
	f.t.Helper()
	out := []string{}
	for _, row := range f.stringsIn(sc.key, `SELECT root_chunk_id, title FROM documents`) {
		parts := strings.SplitN(row, "→", 2)
		cb, err := f.d.readBody(f.ctx, sc.mscope, sc.key, parts[0])
		if err != nil {
			f.t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(cb.Fields, &fields)
		raw, ok := fields[classifyField]
		if !ok {
			continue
		}
		var rec struct {
			Applied  string          `json:"applied_type"`
			Proposed string          `json:"proposed_type"`
			At       json.RawMessage `json:"at"`
		}
		_ = json.Unmarshal(raw, &rec)
		out = append(out, parts[1]+"→"+rec.Applied+"→"+rec.Proposed+"→"+string(rec.At))
	}
	sort.Strings(out)
	return out
}

// state is every value the job may move, in every scope, ids left out, so two fixtures
// compare.
func (f *renameFixture) state() map[string][]string {
	f.t.Helper()
	ont := f.read().DocumentID
	out := map[string][]string{
		"the class's former": {f.formerOf("Circular")},
		"mentions":           f.mentions("Bulletin", "Circular"),
		"annotations.class":  f.strings(`SELECT class FROM chunk_annotations WHERE class IS NOT NULL`),
		"node_stats":         f.strings(`SELECT node_id FROM node_stats WHERE kind = 'class'`),
		"measure_points":     f.strings(`SELECT node_id FROM measure_points`),
	}
	for name, sc := range f.scopes {
		k := sc.key
		out[name+" documents.type"] = f.stringsIn(k, `SELECT type FROM documents WHERE type IS NOT NULL AND id <> ?`, ont)
		out[name+" chunks.type"] = f.stringsIn(k, `SELECT type FROM chunks WHERE type IS NOT NULL AND document_id <> ?`, ont)
		out[name+" chunk_types"] = f.stringsIn(k, `SELECT name FROM chunk_types`)
		out[name+" identity_key"] = f.stringsIn(k, `SELECT identity_key FROM chunks WHERE identity_key IS NOT NULL`)
		out[name+" natural_key"] = f.stringsIn(k, `SELECT natural_key FROM chunk_memory_meta WHERE natural_key IS NOT NULL`)
		out[name+" aliases"] = f.stringsIn(k, `SELECT from_key, to_key FROM subject_key_aliases`)
		out[name+" classification"] = f.classifications(sc)
	}
	return out
}

// mentions is, per class name, what the ontology names it in (C12).
func (f *renameFixture) mentions(classes ...string) []string {
	f.t.Helper()
	out := []string{}
	for _, c := range classes {
		for _, m := range ontologyMentions(f.read(), []string{c}) {
			out = append(out, c+": "+m.what)
		}
	}
	sort.Strings(out)
	return out
}

func (f *renameFixture) formerOf(class string) string {
	for _, term := range f.read().Terms {
		if term.Name == class {
			return term.RenamedFrom
		}
	}
	return ""
}

// rename renames entry to name through update_chunk with a fresh plan's fingerprint, and
// returns the answer.
func (f *renameFixture) rename(ctx context.Context, entry, name string) (map[string]any, tools.Result) {
	f.t.Helper()
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": entry})
	return f.do(ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": entry, "revision": int(g["revision"].(float64)),
		"title": name, "plan_fingerprint": f.planFingerprint(entry)})
}

func jobState(t *testing.T, out map[string]any) string {
	t.Helper()
	job, _ := out["rename_job"].(map[string]any)
	if job == nil {
		t.Fatalf("no rename_job in %v", out)
	}
	return asStr(job["state"])
}

// rowsUnder counts, per step, the tenant-scope rows still under names.
func (f *renameFixture) rowsUnder(names ...string) map[string]int {
	f.t.Helper()
	return f.rowsUnderIn(f.scopes["tenant"], names...)
}

// rowsUnderIn counts, per step the finish check recounts, a scope's rows still under
// names.
func (f *renameFixture) rowsUnderIn(sc renameScope, names ...string) map[string]int {
	f.t.Helper()
	out := map[string]int{}
	for _, st := range renameSteps {
		if st.left == nil || (st.tenantOnly && !sc.tenant) {
			continue
		}
		n, err := st.left(f.d, f.ctx, sc, names, "")
		if err != nil {
			f.t.Fatalf("%s: %v", st.name, err)
		}
		out[st.name] = n
	}
	return out
}

func classRenameLeavesNoOldName(t *testing.T, f *renameFixture) {
	t.Helper()
	memoBefore := f.rowsUnder("Memo")
	for step, n := range f.rowsUnder("Bulletin") {
		if n == 0 {
			t.Fatalf("the fixture holds no row under Bulletin for %s", step)
		}
	}
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: f.bulletin, Name: "Circular"}})
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Changes[0].Job
	if job == nil || !job.Runnable || len(job.Steps) != len(renameSteps) {
		t.Fatalf("plan job block = %+v, want a runnable job of %d steps", job, len(renameSteps))
	}
	for _, s := range job.Steps {
		if s.TenantScope == 0 && s.Name != "classification_records" { // the plan does not read document fields
			t.Errorf("plan step %s counts no tenant row", s.Name)
		}
	}

	out, r := f.rename(f.ctx, f.bulletin, "Circular")
	if r.IsError {
		t.Fatalf("rename: %s", r.Text)
	}
	if got := jobState(t, out); got != jobStateDone {
		t.Fatalf("rename_job.state = %q, want done (%v)", got, out["rename_job"])
	}
	for _, sc := range f.scopes {
		for step, n := range f.rowsUnderIn(sc, "Bulletin") {
			if n != 0 {
				t.Errorf("after the job, %s in the %s scope holds %d rows under the old name", step, sc.name, n)
			}
		}
	}
	for step, n := range f.rowsUnder("Circular") {
		if n == 0 {
			t.Errorf("after the job, %s holds no row under the new name", step)
		}
	}
	st := f.state()
	if !containsString(st["tenant identity_key"], "circular:weekly one") || !containsString(st["tenant natural_key"], "Circular:staff-one") {
		t.Errorf("keys after the job: identity %v, subject %v", st["tenant identity_key"], st["tenant natural_key"])
	}
	if !containsString(st["tenant aliases"], "Bulletin:staff-one→Circular:staff-one") {
		t.Errorf("aliases = %v, want the subject's old key pointing at its new one", st["tenant aliases"])
	}
	if got := f.formerOf("Circular"); got != "Bulletin" {
		t.Errorf("the renamed class's @renamed_from = %q, want Bulletin", got)
	}
	// The minimal pair: Memo's rows are exactly as before.
	if got := f.rowsUnder("Memo"); !reflect.DeepEqual(got, memoBefore) {
		t.Errorf("Memo's rows moved: %v, were %v", got, memoBefore)
	}
	// The class reads its rows under the new name.
	wantTitles(t, "query_documents type=Circular", rowTitles(f.must(map[string]any{"op": "query_documents", "scope": "tenant", "type": "Circular"}), "documents"),
		"Weekly one", "Monthly one", "Staff one", "Head office")
	plan, err = f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: f.bulletin}})
	if err != nil {
		t.Fatal(err)
	}
	if c := plan.Changes[0].Counts["typed_documents"]; c.TenantScope != 4 || c.OtherScopes != 6 {
		t.Errorf("after the job the plan counts %d tenant and %d other documents of the class, want 4 and 6", c.TenantScope, c.OtherScopes)
	}
}

// TestClassRename_NoStoredTypeIdentityOrSubjectKeyHoldsTheOldName is RFC GQ's check
// P5: after a class rename no stored type, identity key or subject key (nor any other
// column the job moves) holds the old name, and the class not renamed keeps its rows.
func TestClassRename_NoStoredTypeIdentityOrSubjectKeyHoldsTheOldName(t *testing.T) {
	classRenameLeavesNoOldName(t, newRenameFixture(t))
}

func TestClassRename_NoStoredTypeIdentityOrSubjectKeyHoldsTheOldNameOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	classRenameLeavesNoOldName(t, newRenameFixtureOn(t, d, base))
}

// TestClassRename_InterruptedJobResumes: a job stopped after each of its steps, then
// resumed by the next rename call, leaves exactly what an uninterrupted job leaves. The
// rows read as the class at every stop.
func TestClassRename_InterruptedJobResumes(t *testing.T) {
	whole := newRenameFixture(t)
	if out, r := whole.rename(whole.ctx, whole.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("uninterrupted rename: %s", r.Text)
	}
	want := whole.state()
	for i, step := range renameSteps[:len(renameSteps)-1] {
		t.Run(step.name, func(t *testing.T) {
			f := newRenameFixture(t)
			f.d.renameJobStop = func(scope, s string) bool { return scope == "tenant" && s == step.name }
			out, r := f.rename(f.ctx, f.bulletin, "Circular")
			if r.IsError {
				t.Fatalf("rename: %s", r.Text)
			}
			if got := jobState(t, out); got != jobStateRunning {
				t.Fatalf("stopped after %s: state %q, want running", step.name, got)
			}
			job, _ := f.d.unfinishedOntologyJob(f.ctx, f.key)
			if job == nil || job.Step != renameSteps[i+1].name {
				t.Fatalf("stopped after %s: job %+v, want it at %s", step.name, job, renameSteps[i+1].name)
			}
			// Mid-job, the rows still under the old name read as the class.
			if n := len(rowTitles(f.must(map[string]any{"op": "query_documents", "scope": "tenant", "type": "Circular"}), "documents")); n != 4 {
				t.Errorf("mid-job, type=Circular finds %d documents, want 4", n)
			}
			f.d.renameJobStop = nil
			// The next rename (of a class nothing rests on) resumes the job first.
			if _, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement"); r.IsError {
				t.Fatalf("the rename that resumes: %s", r.Text)
			}
			if job, _ := f.d.unfinishedOntologyJob(f.ctx, f.key); job != nil {
				t.Fatalf("the job is still unfinished: %+v", job)
			}
			if got := f.state(); !reflect.DeepEqual(got, want) {
				t.Errorf("resumed after %s:\n got %v\nwant %v", step.name, got, want)
			}
		})
	}
	// A job row left pending by a rename that never reached the ontology is abandoned,
	// and nothing is moved.
	f := newRenameFixture(t)
	before := f.state()
	s := &renameJobStart{entry: f.bulletin, from: []string{"Bulletin"}, to: "Circular"}
	if err := f.d.createRenameJob(f.ctx, f.key, s); err != nil {
		t.Fatal(err)
	}
	if _, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement"); r.IsError {
		t.Fatalf("the rename after a pending job: %s", r.Text)
	}
	if got := f.strings(`SELECT state FROM ontology_jobs WHERE id = ?`, s.jobID); !reflect.DeepEqual(got, []string{jobStateAbandoned}) {
		t.Errorf("the pending job's state = %v, want abandoned", got)
	}
	if got := f.state(); !reflect.DeepEqual(got, before) {
		t.Errorf("an abandoned job moved rows:\n got %v\nwant %v", got, before)
	}
}

// TestClassRename_SecondRenameIsRefusedWhileOneRuns: while a rename job is unfinished,
// another class rename is refused (rename_job_running) and changes nothing; the lease
// is the runtime's alone; once the job can finish, the next rename finishes it and goes
// through.
func TestClassRename_SecondRenameIsRefusedWhileOneRuns(t *testing.T) {
	f := newRenameFixture(t)
	f.d.renameJobStop = func(string, string) bool { return true } // never gets past a step
	if out, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateRunning {
		t.Fatalf("first rename: %s", r.Text)
	}
	_, r := f.rename(f.ctx, f.memo, "Note")
	if !r.IsError || !strings.Contains(r.Text, renameJobRunningCode) {
		t.Fatalf("second rename = %s, want %s", r.Text, renameJobRunningCode)
	}
	if _, ok := termByName(f.read(), "Memo"); !ok {
		t.Error("the refused rename renamed Memo")
	}
	if got := f.rowsUnder("Note"); got["documents_type"] != 0 {
		t.Errorf("the refused rename moved rows: %v", got)
	}
	// The minimal pair: a rename nothing rests on is refused too, since every class
	// rename passes the same gate.
	if _, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement"); !r.IsError || !strings.Contains(r.Text, renameJobRunningCode) {
		t.Errorf("rename of Notice while the job runs = %s, want %s", r.Text, renameJobRunningCode)
	}
	for _, op := range []string{"lease", "release"} {
		if _, r := f.do(f.ctx, map[string]any{"op": op, "scope": "tenant", "name": ontologyRenameLeaseName}); !r.IsError {
			t.Errorf("op=%s of %s by the operator: %s, want a refusal", op, ontologyRenameLeaseName, r.Text)
		}
	}
	f.d.renameJobStop = nil
	out, r := f.rename(f.ctx, f.memo, "Note")
	if r.IsError {
		t.Fatalf("second rename once the first can finish: %s", r.Text)
	}
	if got := jobState(t, out); got != jobStateDone {
		t.Errorf("second job: %q", got)
	}
	if got := f.rowsUnder("Bulletin", "Memo"); got["documents_type"]+got["subject_keys"] != 0 {
		t.Errorf("rows left under the old names: %v", got)
	}
}

// TestClassRename_IdentityCollisionIsReportedByThePlanAndRefuses: a stored key that
// would become one another row already holds — an identity key or a subject key under
// the new name, the P5.1 gap where a row's key was recorded under the class's new name
// — is listed by the plan, and the rename is refused before anything is written.
func TestClassRename_IdentityCollisionIsReportedByThePlanAndRefuses(t *testing.T) {
	renameCollisionRefused(t, newRenameFixture)
}

// TestClassRename_IdentityCollisionIsReportedByThePlanAndRefusesOnPostgres: the same on
// the postgres tier, whose string functions the collision query leans on.
func TestClassRename_IdentityCollisionIsReportedByThePlanAndRefusesOnPostgres(t *testing.T) {
	pgDocumentOrSkip(t) // skips when no postgres is configured
	renameCollisionRefused(t, func(t *testing.T) *renameFixture {
		d, base := pgDocumentOrSkip(t) // a clean postgres per case
		return newRenameFixtureOn(t, d, base)
	})
}

func renameCollisionRefused(t *testing.T, newFixture func(*testing.T) *renameFixture) {
	t.Helper()
	for _, c := range []struct{ column, stmt, into string }{
		{"identity_key", `UPDATE chunks SET identity_key = 'circular:weekly one' WHERE identity_key = 'memo:weekly two'`, "circular:weekly one"},
		{"natural_key", `UPDATE chunk_memory_meta SET natural_key = 'Circular:staff-one' WHERE natural_key = 'Memo:staff-two'`, "Circular:staff-one"},
	} {
		t.Run(c.column, func(t *testing.T) {
			f := newFixture(t)
			plan := func() *connector.OntologyRenameJobPlan {
				p, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: f.bulletin, Name: "Circular"}})
				if err != nil {
					t.Fatal(err)
				}
				return p.Changes[0].Job
			}
			// The minimal pair first: without the colliding row nothing is reported.
			if job := plan(); job.CollisionsTotal != 0 {
				t.Fatalf("collisions before the colliding row: %+v", job.Collisions)
			}
			f.sql(c.stmt)
			job := plan()
			if job.CollisionsTotal != 1 || len(job.Collisions) != 1 || job.Collisions[0].Column != c.column || job.Collisions[0].Into != c.into {
				t.Fatalf("plan collisions = %d %+v, want one %s into %q", job.CollisionsTotal, job.Collisions, c.column, c.into)
			}
			if job.Runnable {
				t.Error("the plan calls a colliding rename runnable")
			}
			before := f.state()
			_, r := f.rename(f.ctx, f.bulletin, "Circular")
			if !r.IsError || !strings.Contains(r.Text, renameCollisionCode) || !strings.Contains(r.Text, c.into) {
				t.Fatalf("rename = %s, want %s naming %q", r.Text, renameCollisionCode, c.into)
			}
			if _, ok := termByName(f.read(), "Bulletin"); !ok {
				t.Error("the refused rename renamed the class")
			}
			if got := f.state(); !reflect.DeepEqual(got, before) {
				t.Errorf("the refused rename moved rows:\n got %v\nwant %v", got, before)
			}
			if got := f.strings(`SELECT count(*) FROM ontology_jobs`); !reflect.DeepEqual(got, []string{"0"}) {
				t.Errorf("the refused rename recorded a job: %v", got)
			}
		})
	}
}

// TestClassRename_NodeStatsAndSeriesRowsFollowTheName: a class's statistics and series
// are keyed by its name and move with it; a statistics row already under the new name
// replaces the old one; a series point is never deleted, so one whose twin already
// exists under the new name stays where it is; a concept's rows and the class not
// renamed are untouched.
func TestClassRename_NodeStatsAndSeriesRowsFollowTheName(t *testing.T) {
	f := newRenameFixture(t)
	concept := f.chunk("supervises") // an ontology chunk id: a concept-like node id
	f.sql(`INSERT INTO node_stats (node_id, kind, members, members_with_vector, inputs_sha256, members_sha256, computed_at)
		VALUES (?, 'concept', 1, 0, 'x', 'y', 1)`, concept)
	f.sql(`INSERT INTO measure_points (measure, node_id, subject_id, period_start, period, value_sum, members, groups, sources,
		basis, fallback_share, inputs_sha256, members_sha256, computed_at) VALUES ('count', ?, '', 0, 'month', 1, 1, 1, 1, 1, 0, 'x', 'y', 1)`, concept)
	// Already under the new name: a statistics row, and the twin of one series point.
	f.sql(`INSERT INTO node_stats (node_id, kind, members, members_with_vector, inputs_sha256, members_sha256, computed_at)
		VALUES ('Circular', 'class', 9, 0, 'new', 'y', 2)`)
	f.sql(`INSERT INTO measure_points (measure, node_id, subject_id, period_start, period, value_sum, members, groups, sources,
		basis, fallback_share, inputs_sha256, members_sha256, computed_at) VALUES ('count', 'Bulletin', '', 0, 'month', 3, 3, 1, 1, 3, 0, 'x', 'y', 2)`)
	f.sql(`INSERT INTO measure_points (measure, node_id, subject_id, period_start, period, value_sum, members, groups, sources,
		basis, fallback_share, inputs_sha256, members_sha256, computed_at) VALUES ('count', 'Circular', '', 0, 'month', 3, 3, 1, 1, 3, 0, 'x', 'y', 2)`)
	if out, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("rename: %s", r.Text)
	}
	sorted := func(v ...string) []string { sort.Strings(v); return v }
	if got := f.strings(`SELECT node_id, kind, inputs_sha256 FROM node_stats`); !reflect.DeepEqual(got,
		sorted("Circular→class→new", "Memo→class→x", concept+"→concept→x")) {
		t.Errorf("node_stats = %v", got)
	}
	if got := f.strings(`SELECT node_id, computed_at FROM measure_points`); !reflect.DeepEqual(got,
		sorted("Bulletin→2", "Circular→1", "Circular→2", "Memo→1", concept+"→1")) {
		t.Errorf("measure_points = %v (the point whose twin exists stays, none is deleted)", got)
	}
}

// TestClassRename_UpsertUnderTheOldSubjectKeyReachesTheSameSubject (owner ruling Q1): a
// writer still deriving the old key (<old class>:<slug>) updates the renamed subject
// instead of minting a second one, and the subject keeps its new key; a key nobody had
// still creates. In the tenant scope and in a user scope, whose subject keys the job
// rewrites too (P5.4).
func TestClassRename_UpsertUnderTheOldSubjectKeyReachesTheSameSubject(t *testing.T) {
	f := newRenameFixture(t)
	if _, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError {
		t.Fatalf("rename: %s", r.Text)
	}
	for _, scope := range []string{"tenant", "user"} {
		t.Run(scope, func(t *testing.T) {
			key := f.scopes[scope].key
			subject := f.stringsIn(key, `SELECT chunk_id FROM chunk_memory_meta WHERE natural_key = 'Circular:staff-one'`)
			if len(subject) != 1 {
				t.Fatalf("subject under the new key: %v", subject)
			}
			out := f.must(map[string]any{"op": "upsert_chunk", "scope": scope, "natural_key": "Bulletin:staff-one",
				"title": "Staff one", "type": "Circular", "subject": "Staff one", "body": "Sent on Mondays."})
			if out["id"] != subject[0] || out["created"] != false || out["natural_key"] != "Circular:staff-one" {
				t.Errorf("upsert under the old key = %v, want an update of %s keeping Circular:staff-one", out, subject[0])
			}
			if got := f.stringsIn(key, `SELECT natural_key FROM chunk_memory_meta WHERE natural_key LIKE '%staff-one'`); !reflect.DeepEqual(got, []string{"Circular:staff-one"}) {
				t.Errorf("subject keys after the upsert = %v, want one under the new key", got)
			}
			// The minimal pair: a key no subject had, nor any alias, still creates one.
			home := f.stringsIn(key, `SELECT document_id FROM chunks WHERE id = ?`, subject[0])
			fresh := f.must(map[string]any{"op": "upsert_chunk", "scope": scope, "natural_key": "Bulletin:staff-three", "document_id": home[0],
				"title": "Other staff", "type": "Circular", "subject": "Other staff", "body": "New."})
			if fresh["created"] != true {
				t.Errorf("upsert of a new key = %v, want created", fresh)
			}
		})
	}
}

// TestClassRename_EveryColumnHoldingAClassNameIsListed is the census: one class name
// is written into a full fixture, the job runs, and then every column of every table of
// the tenant, user and agent scopes is searched for the name, and so is every chunk's
// body and fields in the Memory plane. A place still holding it must be one the job is
// not meant to rewrite, listed below with why; any other is a place the job misses.
// Before the job, every column the job lists must hold the name in each scope it has
// rows in, so the fixture really exercises each step.
func TestClassRename_EveryColumnHoldingAClassNameIsListed(t *testing.T) {
	f := newRenameFixture(t)
	const old = "bulletin" // matched case-insensitively, as class names are
	ont := f.read().DocumentID
	// Where the old name stays on purpose.
	kept := map[string]string{
		"subject_key_aliases.from_key": "the alias that keeps the old subject key working (ruling Q1)",
		"ontology_jobs.from_names":     "the job's own record of what it renamed",
		// A release is a copy of the ontology as it stood (RFC GQ P3.3): the catch_up
		// release before the rename holds the class under its old name, and the apply
		// release the job takes holds @renamed_from and the job's state.
		"ontology_releases.export_md":     "a release's copy of the ontology",
		"ontology_releases.plan_summary":  "the finished job's state, recorded on its release",
		"ontology_release_entries.title":  "a release's copy of the ontology",
		"ontology_release_bodies.body":    "a release's copy of the ontology",
		"ontology_release_bodies.fields":  "a release's copy of the ontology (an instance's registry_key)",
		"tenant memory doc.chunk:<class>": "the renamed class's own @renamed_from line",
	}
	// Rows of the ontology document itself: the chunks the job rewrote keep their
	// earlier bodies in their revision history.
	ontologyRow := map[string]string{
		"chunk_revisions": `chunk_id IN (SELECT id FROM chunks WHERE document_id = '` + ont + `')`,
	}
	type hit struct{ scope, place string }
	search := func() map[hit]bool {
		t.Helper()
		found := map[hit]bool{}
		for _, name := range renameFixtureScopes {
			sc := f.scopes[name]
			for _, table := range f.stringsIn(sc.key, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`) {
				for _, col := range f.stringsIn(sc.key, `SELECT name FROM pragma_table_info(?)`, table) {
					where := `lower(CAST("` + col + `" AS TEXT)) LIKE '%` + old + `%'`
					if extra, ok := ontologyRow[table]; ok && sc.tenant {
						where += ` AND NOT (` + extra + `)`
					}
					if n := f.stringsIn(sc.key, `SELECT count(*) FROM "`+table+`" WHERE `+where); n[0] != "0" {
						found[hit{name, table + "." + col}] = true
					}
				}
			}
			// The Memory plane: every chunk's body and fields.
			for _, id := range f.stringsIn(sc.key, `SELECT id FROM chunks`) {
				cb, err := f.d.readBody(f.ctx, sc.mscope, sc.key, id)
				if err != nil {
					t.Fatal(err)
				}
				text := strings.ToLower(cb.Body + string(cb.Fields))
				if id == f.bulletin {
					text = strings.ReplaceAll(text, "@renamed_from "+old, "")
					if strings.Contains(strings.ToLower(cb.Body), "@renamed_from "+old) {
						found[hit{name, "memory doc.chunk:<class>"}] = true
					}
				}
				if strings.Contains(text, old) {
					found[hit{name, "memory doc.chunk:" + id}] = true
				}
			}
		}
		return found
	}
	before := search()
	for _, name := range renameFixtureScopes {
		for _, st := range renameSteps {
			if st.table == "" || (name != "tenant" && (st.name == "annotation_classes" || st.name == "node_stats" || st.name == "measure_points")) {
				continue // outside SQL, or figures only the tenant scope keeps
			}
			if !before[hit{name, st.table + "." + st.column}] {
				t.Errorf("before the job, %s.%s in the %s scope holds no %q: the fixture does not exercise step %s", st.table, st.column, name, old, st.name)
			}
		}
	}
	memory := 0
	for h := range before {
		if strings.HasPrefix(h.place, "memory ") {
			memory++
		}
	}
	if memory < 2*3+4 { // two classification records per scope, and the four entries naming the class
		t.Errorf("before the job, %d chunks hold %q in the Memory plane; the fixture does not exercise the steps outside SQL", memory, old)
	}
	if out, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("rename: %s", r.Text)
	}
	var missed []string
	for h := range search() {
		if _, ok := kept[h.place]; ok {
			continue
		}
		if _, ok := kept[h.scope+" "+h.place]; ok {
			continue
		}
		missed = append(missed, h.scope+" "+h.place)
	}
	sort.Strings(missed)
	if len(missed) > 0 {
		t.Errorf("after the job these places still hold %q: %v — rewrite them in the job (renameSteps) or list them as kept, with why", old, missed)
	}
}

// TestClassRename_RenameOfAClassNothingRestsOnStartsNoJob: the minimal pair of the job
// tests: a class with no row renames directly, with no job row and no rename_job.
func TestClassRename_RenameOfAClassNothingRestsOnStartsNoJob(t *testing.T) {
	f := newRenameFixture(t)
	out, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement")
	if r.IsError {
		t.Fatalf("rename: %s", r.Text)
	}
	if _, has := out["rename_job"]; has {
		t.Errorf("a rename nothing rests on started a job: %v", out["rename_job"])
	}
	if got := f.strings(`SELECT count(*) FROM ontology_jobs`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("job rows = %v", got)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "@renamed_from") {
		t.Errorf("a direct rename recorded @renamed_from: %s", b)
	}
}
