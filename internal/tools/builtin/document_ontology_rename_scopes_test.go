package builtin

// document_ontology_rename_scopes_test.go — RFC GQ P5.4: the class-rename job in user and
// agent scopes, on a pre-split scope's agent plane, over the ontology's own mentions of
// the class (C12) and over classification records, and the release it takes when done.
// The fixture is renameFixture (document_ontology_rename_job_test.go): every scope holds
// rows written under the class before the rename.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestClassRename_RewritesAUserScopeWrittenBeforeTheRename: rows a user scope (and an
// agent scope) holds under the class from before the rename — types, the define_type
// row, identity and subject keys, classification records — are carried to the new name;
// the plan counts them and no longer refuses for them; Memo's rows in those scopes stay
// as they were.
func TestClassRename_RewritesAUserScopeWrittenBeforeTheRename(t *testing.T) {
	assertRenameRewritesUserScope(t, newRenameFixture(t))
}

func TestClassRename_RewritesAUserScopeWrittenBeforeTheRenameOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	assertRenameRewritesUserScope(t, newRenameFixtureOn(t, d, base))
}

func assertRenameRewritesUserScope(t *testing.T, f *renameFixture) {
	t.Helper()
	user, agent := f.scopes["user"], f.scopes["agent"]
	memoBefore := map[string]map[string]int{"user": f.rowsUnderIn(user, "Memo"), "agent": f.rowsUnderIn(agent, "Memo")}
	memoRecords := map[string][]string{}
	for _, name := range []string{"user", "agent"} {
		for step, n := range f.rowsUnderIn(f.scopes[name], "Bulletin") {
			if n == 0 && step != "annotation_classes" && step != "node_stats" && step != "measure_points" {
				t.Fatalf("the fixture's %s scope holds no row under Bulletin for %s", name, step)
			}
		}
		for _, rec := range f.classifications(f.scopes[name]) {
			if strings.Contains(rec, "Memo") || strings.Contains(rec, "memo") {
				memoRecords[name] = append(memoRecords[name], rec)
			}
		}
	}
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: f.bulletin, Name: "Circular"}})
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Changes[0].Job
	if job == nil || !job.Runnable || len(job.Blocked) != 0 {
		t.Fatalf("plan job block = %+v, want a runnable job with nothing blocking it", job)
	}
	for _, s := range job.Steps {
		if s.Name == "documents_type" && s.OtherScopes != 6 {
			t.Errorf("plan documents_type other_scopes = %d, want the user's and the agent's 3 each", s.OtherScopes)
		}
	}
	out, r := f.rename(f.ctx, f.bulletin, "Circular")
	if r.IsError {
		t.Fatalf("rename: %s", r.Text)
	}
	if got := jobState(t, out); got != jobStateDone {
		t.Fatalf("rename_job = %v, want done", out["rename_job"])
	}
	if job, _ := out["rename_job"].(map[string]any); asInt(job["scopes_done"]) != 3 {
		t.Errorf("rename_job.scopes_done = %v, want the tenant, user and agent scopes", job["scopes_done"])
	}
	for _, name := range []string{"user", "agent"} {
		sc := f.scopes[name]
		for step, n := range f.rowsUnderIn(sc, "Bulletin") {
			if n != 0 {
				t.Errorf("after the job the %s scope holds %d rows under Bulletin at %s", name, n, step)
			}
		}
		if got := f.rowsUnderIn(sc, "Memo"); !reflect.DeepEqual(got, memoBefore[name]) {
			t.Errorf("Memo's rows in the %s scope moved: %v, were %v", name, got, memoBefore[name])
		}
		if got := f.stringsIn(sc.key, `SELECT from_key, to_key FROM subject_key_aliases`); !reflect.DeepEqual(got, []string{"Bulletin:staff-one→Circular:staff-one"}) {
			t.Errorf("%s aliases = %v", name, got)
		}
		at := renameJSONInt(classificationAt)
		recs := f.classifications(sc)
		for _, want := range []string{"Weekly one→Circular→→" + at, "Plain one→→Circular→" + at} {
			if !containsString(recs, want) {
				t.Errorf("%s classification records = %v, want %q (the time kept to the nanosecond)", name, recs, want)
			}
		}
		for _, rec := range memoRecords[name] {
			if !containsString(recs, rec) {
				t.Errorf("%s: Memo's record %q changed: %v", name, rec, recs)
			}
		}
		wantTitles(t, name+" query_documents type=Circular",
			rowTitles(f.must(map[string]any{"op": "query_documents", "scope": name, "type": "Circular"}), "documents"),
			"Weekly one", "Monthly one", "Staff one")
	}
}

// TestClassRename_PreSplitScopeIsRewrittenOnItsAgentPlane: a user scope whose documents
// are still on its agent plane (from before the planes split, untouched since) is
// rewritten there and left where it is; the Document tool's next use of it moves the
// rewritten rows, which then read under the new name.
func TestClassRename_PreSplitScopeIsRewrittenOnItsAgentPlane(t *testing.T) {
	assertPreSplitScopeRewritten(t, newRenameFixture(t))
}

func TestClassRename_PreSplitScopeIsRewrittenOnItsAgentPlaneOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	assertPreSplitScopeRewritten(t, newRenameFixtureOn(t, d, base))
}

func assertPreSplitScopeRewritten(t *testing.T, f *renameFixture) {
	t.Helper()
	ctx := context.Background()
	key := f.scopes["user"].key
	dump, err := f.d.SqlMem.ExportScope(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	dump.DocTail = nil
	if _, err := f.d.SqlMem.DropScope(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := f.d.SqlMem.RestoreScope(ctx, key, dump); err != nil {
		t.Fatal(err)
	}
	f.d.SqlMem.ForgetEnsured(key)
	preSplit := func() bool {
		ok, err := f.d.docSQL().PlaneExists(ctx, key)
		held, herr := f.d.docSQL().AgentPlaneTables(ctx, key, []string{"documents"})
		return err == nil && herr == nil && !ok && len(held) == 1
	}
	if !preSplit() {
		t.Fatal("the user scope is not in the pre-split layout")
	}
	onAgentPlane := func(stmt string) []string {
		t.Helper()
		res, err := f.d.docSQL().Query(ctx, key.AgentPlane(), f.d.docSQL().Rebind(stmt), nil)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		out := []string{}
		for _, r := range res.Rows {
			out = append(out, asStr(r[0]))
		}
		return out
	}
	if got := onAgentPlane(`SELECT count(*) FROM documents WHERE type = 'Bulletin'`); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the agent plane holds %v Bulletin documents before the rename, want 3", got)
	}
	// A scope from before the alias table existed (P5.3) has none: the job makes it there
	// rather than provisioning the scope.
	if _, err := f.d.docSQL().Exec(ctx, key.AgentPlane(), "DROP TABLE subject_key_aliases", nil, 0); err != nil {
		t.Fatalf("drop the alias table: %v", err)
	}
	out, r := f.rename(f.ctx, f.bulletin, "Circular")
	if r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("rename: %s %v", r.Text, out["rename_job"])
	}
	if !preSplit() {
		t.Error("the job moved the pre-split scope off its agent plane")
	}
	for stmt, want := range map[string][]string{
		`SELECT count(*) FROM documents WHERE type = 'Bulletin'`:                        {"0"},
		`SELECT count(*) FROM documents WHERE type = 'Circular'`:                        {"3"},
		`SELECT count(*) FROM chunks WHERE identity_key LIKE 'bulletin:%'`:              {"0"},
		`SELECT natural_key FROM chunk_memory_meta WHERE natural_key LIKE '%staff-one'`: {"Circular:staff-one"},
		`SELECT to_key FROM subject_key_aliases WHERE from_key = 'Bulletin:staff-one'`:  {"Circular:staff-one"},
		`SELECT count(*) FROM chunk_types WHERE name = 'Bulletin' OR name = 'Circular'`: {"1"},
		`SELECT count(*) FROM chunk_types WHERE name = 'Memo'`:                          {"1"},
		`SELECT count(*) FROM documents WHERE type = 'Memo'`:                            {"3"},
	} {
		if got := onAgentPlane(stmt); !reflect.DeepEqual(got, want) {
			t.Errorf("agent plane %s = %v, want %v", stmt, got, want)
		}
	}
	// The Document tool's next use moves the scope; its rows read under the new name.
	wantTitles(t, "user query_documents type=Circular after the move",
		rowTitles(f.must(map[string]any{"op": "query_documents", "scope": "user", "type": "Circular"}), "documents"),
		"Weekly one", "Monthly one", "Staff one")
	if preSplit() {
		t.Error("the Document tool's use did not move the scope")
	}
	if got := f.rowsUnderIn(f.scopes["user"], "Bulletin"); got["documents_type"]+got["identity_keys"]+got["subject_keys"] != 0 {
		t.Errorf("after the move the user scope holds rows under Bulletin: %v", got)
	}
}

// TestClassRename_OntologyMentionsFollowTheNameInTheSameChange (RFC GQ 9b C12, owner
// ruling 15.1): the entries that name the class — an instance's @a, a scheme's @projects,
// a relation's @domain, a class's ref field — name the new class as soon as the rename's
// first slice has run, before any row moves; the instance's recorded key follows its
// subject's, so adoption finds the same subject; the plan listed them as notes and did
// not refuse; Memo's mentions stay.
func TestClassRename_OntologyMentionsFollowTheNameInTheSameChange(t *testing.T) {
	f := newRenameFixture(t)
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: f.bulletin, Name: "Circular"}})
	if err != nil || plan.Changes[0].Job == nil {
		t.Fatalf("plan: %v %+v", err, plan)
	}
	job := plan.Changes[0].Job
	notes := strings.Join(append(append([]string{}, plan.Changes[0].Notes...), job.Notes...), "\n")
	if job.Steps[0].Name != "ontology_mentions" || job.Steps[0].TenantScope != 4 {
		t.Errorf("plan step %+v, want ontology_mentions counting the four entries", job.Steps[0])
	}
	for _, want := range []string{"the instance \"Head office\"", "the scheme \"topics\"", "the relation \"issued_by\"", "the field \"reply_to\""} {
		if !strings.Contains(notes, want) {
			t.Errorf("plan notes %q do not name %s", notes, want)
		}
	}
	if strings.Contains(notes, renameJobPendingCode) {
		t.Errorf("plan notes %q refuse the rename", notes)
	}
	memoBefore := f.mentions("Memo")
	f.d.renameJobStop = func(scope, step string) bool { return step == "ontology_mentions" }
	out, r := f.rename(f.ctx, f.bulletin, "Circular")
	if r.IsError || jobState(t, out) != jobStateRunning {
		t.Fatalf("rename stopped after the mentions: %s %v", r.Text, out["rename_job"])
	}
	if got := f.rowsUnder("Bulletin"); got["documents_type"] == 0 {
		t.Fatalf("the rows moved before the stop: %v", got)
	}
	read := f.read()
	if got := f.mentions("Bulletin"); len(got) != 0 {
		t.Errorf("the ontology still names Bulletin: %v", got)
	}
	if got := f.mentions("Memo"); !reflect.DeepEqual(got, memoBefore) {
		t.Errorf("Memo's mentions = %v, were %v", got, memoBefore)
	}
	var instance memrank.OntologyInstance
	for _, in := range read.Instances {
		if in.Name == "Head office" {
			instance = in
		}
	}
	if instance.Class != "Circular" || instance.RegistryKey != "Circular:head-office" {
		t.Errorf("instance = class %q registry_key %q, want Circular and Circular:head-office", instance.Class, instance.RegistryKey)
	}
	if rel := relationByName(read, "issued_by"); !reflect.DeepEqual(rel.Domain, []string{"Circular", "Memo"}) {
		t.Errorf("issued_by @domain = %v", rel.Domain)
	}
	for _, s := range read.Schemes {
		if s.Name == "topics" && !reflect.DeepEqual(s.Projects, []string{"Circular", "Memo"}) {
			t.Errorf("topics @projects = %v", s.Projects)
		}
	}
	if memo, _ := termByName(read, "Memo"); len(memo.FieldSpecs) == 0 || memo.FieldSpecs[len(memo.FieldSpecs)-1].Range != "Circular" {
		t.Errorf("Memo's fields = %+v, want reply_to ref(Circular)", memo.FieldSpecs)
	}
	// The rewrite is an edit with a history, like any other: the earlier body is kept.
	if got := f.strings(`SELECT count(*) FROM chunk_revisions WHERE chunk_id = ?`, f.chunk("issued_by")); len(got) != 1 || got[0] == "0" {
		t.Errorf("issued_by's revisions = %v, want the rewrite recorded", got)
	}
	f.d.renameJobStop = nil
	if _, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement"); r.IsError {
		t.Fatalf("the rename that resumes: %s", r.Text)
	}
	rep, err := f.d.AdoptInstances(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Adopted != 0 {
		t.Errorf("adoption after the rename minted %d subjects, want none (the instance keeps its subject)", rep.Adopted)
	}
	if got := f.strings(`SELECT natural_key FROM chunk_memory_meta WHERE natural_key LIKE '%head-office'`); !reflect.DeepEqual(got, []string{"Circular:head-office"}) {
		t.Errorf("the instance's subject keys = %v, want one under the new class", got)
	}
}

// TestClassRename_ScopePastTheBudgetLeavesTheJobUnfinished: a slice that ends inside a
// user scope leaves the job unfinished at that scope, with the tenant scope finished and
// the user's rows readable as the class; the next rename resumes it at that scope and
// finishes it.
func TestClassRename_ScopePastTheBudgetLeavesTheJobUnfinished(t *testing.T) {
	f := newRenameFixture(t)
	f.d.renameJobStop = func(scope, step string) bool { return scope == "user" && step == "ontology_mentions" }
	out, r := f.rename(f.ctx, f.bulletin, "Circular")
	if r.IsError {
		t.Fatalf("rename: %s", r.Text)
	}
	job, _ := out["rename_job"].(map[string]any)
	if asStr(job["state"]) != jobStateRunning || asStr(job["scope"]) != "user" || asInt(job["scopes_done"]) < 1 || asInt(job["remaining"]) == 0 {
		t.Fatalf("rename_job = %v, want running in the user scope with rows left there", job)
	}
	for step, n := range f.rowsUnder("Bulletin") {
		if n != 0 {
			t.Errorf("the tenant scope, finished, holds %d rows under Bulletin at %s", n, step)
		}
	}
	if got := f.rowsUnderIn(f.scopes["user"], "Bulletin"); got["documents_type"] != 3 {
		t.Errorf("the user scope's rows moved before its slice: %v", got)
	}
	wantTitles(t, "mid-job user query_documents type=Circular",
		rowTitles(f.must(map[string]any{"op": "query_documents", "scope": "user", "type": "Circular"}), "documents"),
		"Weekly one", "Monthly one", "Staff one")
	// The resume starts at the user scope: the finished tenant scope is not walked again.
	f.d.renameJobStop = func(scope, step string) bool { return scope == "tenant" }
	if _, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement"); r.IsError {
		t.Fatalf("the rename that resumes: %s", r.Text)
	}
	if j, _ := f.d.unfinishedOntologyJob(f.ctx, f.key); j != nil {
		t.Fatalf("the job is still unfinished (did it walk the tenant scope again?): %+v", j)
	}
	for _, name := range []string{"user", "agent"} {
		if got := f.rowsUnderIn(f.scopes[name], "Bulletin"); got["documents_type"]+got["subject_keys"] != 0 {
			t.Errorf("after the resume the %s scope holds rows under Bulletin: %v", name, got)
		}
	}
}

// TestClassRename_CollisionInAUserScopeIsReportedByThePlanAndRefuses: a key in a user
// scope the job would move onto one another row there holds is counted by the plan (and
// never listed: it is another user's row) and refuses the rename before anything is
// written.
func TestClassRename_CollisionInAUserScopeIsReportedByThePlanAndRefuses(t *testing.T) {
	assertUserScopeCollisionRefused(t, newRenameFixture(t))
}

func TestClassRename_CollisionInAUserScopeIsReportedByThePlanAndRefusesOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	assertUserScopeCollisionRefused(t, newRenameFixtureOn(t, d, base))
}

func assertUserScopeCollisionRefused(t *testing.T, f *renameFixture) {
	t.Helper()
	plan := func() *connector.OntologyRenameJobPlan {
		p, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: f.bulletin, Name: "Circular"}})
		if err != nil {
			t.Fatal(err)
		}
		return p.Changes[0].Job
	}
	if job := plan(); job.CollisionsTotal != 0 {
		t.Fatalf("collisions before the colliding row: %+v", job)
	}
	user := f.scopes["user"].key
	if err := f.d.exec(f.ctx, user, `UPDATE chunk_memory_meta SET natural_key = 'Circular:staff-one' WHERE natural_key = 'Memo:staff-two'`); err != nil {
		t.Fatal(err)
	}
	job := plan()
	if job.CollisionsTotal != 1 || job.CollisionsOtherScopes != 1 || len(job.Collisions) != 0 || job.Runnable {
		t.Fatalf("plan = total %d other %d listed %+v runnable %v; want one, in another scope, unlisted, not runnable",
			job.CollisionsTotal, job.CollisionsOtherScopes, job.Collisions, job.Runnable)
	}
	before := f.state()
	_, r := f.rename(f.ctx, f.bulletin, "Circular")
	if !r.IsError || !strings.Contains(r.Text, renameCollisionCode) || !strings.Contains(r.Text, "1 in user and agent scopes") {
		t.Fatalf("rename = %s, want %s naming the other scopes", r.Text, renameCollisionCode)
	}
	if got := f.state(); !reflect.DeepEqual(got, before) {
		t.Errorf("the refused rename moved rows:\n got %v\nwant %v", got, before)
	}
}

// TestClassRename_FinishedJobTakesAnApplyRelease (plan 4.2 step 7, ruling Q2): a direct
// rename takes a catch_up release of the ontology before it, and the finished job an
// apply release of the ontology it left, carrying the job's state; under a caller that
// takes its own release (an accepted change proposal) the job takes none.
func TestClassRename_FinishedJobTakesAnApplyRelease(t *testing.T) {
	f := newRenameFixture(t)
	if out, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("rename: %s", r.Text)
	}
	rels, err := f.d.ontologyReleases(f.ctx, f.key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || rels[0].Reason != OntologyReleaseApply || rels[1].Reason != OntologyReleaseCatchUp {
		t.Fatalf("releases = %+v, want an apply after a catch_up", rels)
	}
	if rels[0].ContentVersion != f.read().ContentVersion || rels[0].ContentVersion == rels[1].ContentVersion {
		t.Errorf("apply release content %s, catch_up %s, now %s", rels[0].ContentVersion, rels[1].ContentVersion, f.read().ContentVersion)
	}
	if got := f.strings(`SELECT plan_summary FROM ontology_releases WHERE seq = ?`, rels[0].Seq); len(got) != 1 || !strings.Contains(got[0], `"kind":"class_rename"`) {
		t.Errorf("apply release plan_summary = %v, want the job's state", got)
	}

	g := newRenameFixture(t)
	if out, r := g.rename(withRenameReleaseByCaller(g.ctx), g.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateDone {
		t.Fatalf("rename under a caller's release: %s", r.Text)
	}
	if rels, _ := g.d.ontologyReleases(g.ctx, g.key, 0); len(rels) != 1 || rels[0].Reason != OntologyReleaseCatchUp {
		t.Errorf("releases under a caller that takes its own = %+v, want the catch_up only", rels)
	}
}

// TestClassRename_BlockedInAUserScopeNamesNoKeyOfIt: a key that comes to collide in a
// user scope after the plan (written while the job ran) blocks the job there, and the
// job's error names the kind of scope and the step, never the user's key.
func TestClassRename_BlockedInAUserScopeNamesNoKeyOfIt(t *testing.T) {
	f := newRenameFixture(t)
	f.d.renameJobStop = func(scope, step string) bool { return scope == "user" && step == "ontology_mentions" }
	if out, r := f.rename(f.ctx, f.bulletin, "Circular"); r.IsError || jobState(t, out) != jobStateRunning {
		t.Fatalf("rename: %s", r.Text)
	}
	f.d.renameJobStop = nil
	if err := f.d.exec(f.ctx, f.scopes["user"].key, `UPDATE chunk_memory_meta SET natural_key = 'Circular:staff-one' WHERE natural_key = 'Memo:staff-two'`); err != nil {
		t.Fatal(err)
	}
	_, r := f.rename(f.ctx, f.chunk("Notice"), "Announcement")
	if !r.IsError || !strings.Contains(r.Text, renameJobRunningCode) {
		t.Fatalf("the rename after the collision = %s, want %s", r.Text, renameJobRunningCode)
	}
	job, err := f.d.unfinishedOntologyJob(f.ctx, f.key)
	if err != nil || job == nil || job.State != jobStateBlocked {
		t.Fatalf("job = %+v %v, want blocked", job, err)
	}
	if !strings.Contains(job.Error, "user scope: subject_keys") || strings.Contains(job.Error, "staff") || strings.Contains(r.Text, "staff") {
		t.Errorf("blocked job's error = %q (refusal %q), want the scope kind and step and no key", job.Error, r.Text)
	}
}
