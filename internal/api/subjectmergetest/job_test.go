package subjectmergetest

// The ontology's rename and re-tag job (RFC GQ P5.6): GET /v1/_ontology/job and POST
// /v1/_ontology/job/resume, the GetOntologyJob and ResumeOntologyJob RPCs, and the MCP
// ontology tool's ops job and resume_job, over one real server; and the plan's
// class-rename block, which gRPC now carries.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// jobFromProto reads a job back into the shared type, field by field, so a field the
// RPC dropped shows as a difference from HTTP.
func jobFromProto(j *loomcyclepb.OntologyJobStatus) *connector.OntologyJobStatus {
	if j == nil {
		return nil
	}
	var done map[string]int
	for k, v := range j.GetDone() {
		if done == nil {
			done = map[string]int{}
		}
		done[k] = int(v)
	}
	return &connector.OntologyJobStatus{ID: j.GetId(), Kind: j.GetKind(), Entry: j.GetEntry(), From: j.GetFrom(), To: j.GetTo(),
		State: j.GetState(), Step: j.GetStep(), Done: done, Error: j.GetError(), Remain: int(j.GetRemaining()),
		Scope: j.GetScope(), ScopesDone: int(j.GetScopesDone()), ToTags: j.GetToTags(), ScopesTotal: int(j.GetScopesTotal()),
		CreatedAt: j.GetCreatedAt(), UpdatedAt: j.GetUpdatedAt()}
}

// renameJobPlanFromProto is the plan's job block, read back the same way.
func renameJobPlanFromProto(p *loomcyclepb.OntologyRenameJobPlan) *connector.OntologyRenameJobPlan {
	if p == nil {
		return nil
	}
	out := &connector.OntologyRenameJobPlan{Runnable: p.GetRunnable(), Blocked: p.GetBlocked(),
		CollisionsTotal: int(p.GetCollisionsTotal()), CollisionsOtherScopes: int(p.GetCollisionsOtherScopes()),
		ScopesNotCounted: int(p.GetScopesNotCounted()), Running: jobFromProto(p.GetRunning()), Notes: p.GetNotes(), Kind: p.GetKind()}
	for _, m := range p.GetTagMoves() {
		out.TagMoves = append(out.TagMoves, connector.OntologyTagMove{From: m.GetFrom(), To: m.GetTo()})
	}
	for _, s := range p.GetSteps() {
		out.Steps = append(out.Steps, connector.OntologyRenameJobStep{Name: s.GetName(), TenantScope: int(s.GetTenantScope()), OtherScopes: int(s.GetOtherScopes())})
	}
	for _, c := range p.GetCollisions() {
		out.Collisions = append(out.Collisions, connector.OntologyRenameCollision{Column: c.GetColumn(), Key: c.GetKey(), Into: c.GetInto(), Holder: c.GetHolder()})
	}
	return out
}

// renameProvision renames the fixture's class Provision (a typed document rests on
// it) with the plan's fingerprint, which starts the rename job and runs it to its end.
func (e *env) renameProvision(f planFixture, name string) string {
	e.t.Helper()
	plan, o := e.planHTTP(operator, []connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: f.provision, Name: name}})
	if !o.ok {
		e.t.Fatalf("plan the rename: %s %s", o.code, o.raw)
	}
	rev := e.opDocCall(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, f.provision))["revision"]
	out := e.opDocCall(fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%v,"title":%q,"plan_fingerprint":%q}`,
		f.provision, rev, name, plan.Fingerprint))
	job, _ := out["rename_job"].(map[string]any)
	if job == nil || job["state"] != "done" {
		e.t.Fatalf("the rename's job = %v, want done", out["rename_job"])
	}
	return job["id"].(string)
}

// setJobState rewrites the job row's state, as a slice that ran out of time leaves it.
func (e *env) setJobState(state string) {
	e.t.Helper()
	key := sqlmem.ScopeKey{Tenant: tenant, Scope: "tenant", ScopeID: tenant}
	if _, err := e.doc.SqlMem.DocPlane().Exec(context.Background(), key, `UPDATE ontology_jobs SET state = ?`, []any{state}, 0); err != nil {
		e.t.Fatalf("set the job's state: %v", err)
	}
}

func (e *env) asJob(o outcome) connector.OntologyJobResult {
	e.t.Helper()
	var res connector.OntologyJobResult
	if o.ok {
		if err := json.Unmarshal([]byte(o.raw), &res); err != nil {
			e.t.Fatalf("not a job answer: %s", o.raw)
		}
	}
	return res
}

func (e *env) jobHTTP(bearer string) (connector.OntologyJobResult, outcome) {
	o := e.httpGetOutcome("/v1/_ontology/job", bearer)
	return e.asJob(o), o
}

func (e *env) resumeHTTP(bearer, id string) (connector.OntologyJobResult, outcome) {
	body := ""
	if id != "" {
		body = fmt.Sprintf(`{"id":%q}`, id)
	}
	resp, raw := e.do(http.MethodPost, "/v1/_ontology/job/resume", bearer, body, nil)
	o := outcome{ok: resp.StatusCode == http.StatusOK, raw: string(raw)}
	if !o.ok {
		var b struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &b)
		o.code = b.Code
		if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient") {
			o.code = "insufficient_scope"
		}
	}
	return e.asJob(o), o
}

func (e *env) jobGRPC(bearer string) (connector.OntologyJobResult, outcome) {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.GetOntologyJob(ctx, &loomcyclepb.GetOntologyJobRequest{})
	if err != nil {
		return connector.OntologyJobResult{}, grpcOutcome(err)
	}
	return connector.OntologyJobResult{Tenant: resp.GetTenant(), Job: jobFromProto(resp.GetJob())}, outcome{ok: true, raw: resp.String()}
}

func (e *env) resumeGRPC(bearer, id string) (connector.OntologyJobResult, outcome) {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.ResumeOntologyJob(ctx, &loomcyclepb.ResumeOntologyJobRequest{Id: id})
	if err != nil {
		return connector.OntologyJobResult{}, grpcOutcome(err)
	}
	return connector.OntologyJobResult{Tenant: resp.GetTenant(), Job: jobFromProto(resp.GetJob())}, outcome{ok: true, raw: resp.String()}
}

func (e *env) jobMCP(bearer string) (connector.OntologyJobResult, outcome) {
	o := e.toolMCP(bearer, "ontology", map[string]any{"op": "job"})
	return e.asJob(o), o
}

func (e *env) resumeMCP(bearer, id string) (connector.OntologyJobResult, outcome) {
	args := map[string]any{"op": "resume_job"}
	if id != "" {
		args["id"] = id
	}
	o := e.toolMCP(bearer, "ontology", args)
	return e.asJob(o), o
}

type jobSurface struct {
	name   string
	job    func(e *env, bearer string) (connector.OntologyJobResult, outcome)
	resume func(e *env, bearer, id string) (connector.OntologyJobResult, outcome)
}

var jobSurfaces = []jobSurface{
	{"http", (*env).jobHTTP, (*env).resumeHTTP},
	{"grpc", (*env).jobGRPC, (*env).resumeGRPC},
	{"mcp", (*env).jobMCP, (*env).resumeMCP},
}

// TestOntologyJob_SameAnswerOnEveryTransport: before any job the status is no job on
// every transport; after a class rename, the finished job reads the same on all three,
// every field; a job left running is resumed to done by each transport alike (the
// row's update time aside), and once nothing is unfinished a resume of that job is
// job_not_found.
func TestOntologyJob_SameAnswerOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	for _, s := range jobSurfaces {
		if res, o := s.job(e, operator); !o.ok || res.Tenant != tenant || res.Job != nil {
			t.Errorf("%s: status with no job = %+v (%s %s), want none", s.name, res, o.code, o.raw)
		}
	}
	id := e.renameProvision(f, "Clause")

	var first connector.OntologyJobResult
	for i, s := range jobSurfaces {
		res, o := s.job(e, operator)
		if !o.ok {
			t.Fatalf("%s: status refused: %s %s", s.name, o.code, o.raw)
		}
		if i == 0 {
			first = res
			j := res.Job
			if j == nil || j.ID != id || j.Kind != "class_rename" || j.State != "done" || j.To != "Clause" ||
				!reflect.DeepEqual(j.From, []string{"Provision"}) || j.ScopesTotal == 0 || j.CreatedAt == 0 || j.Done["documents_type"] == 0 {
				t.Fatalf("http status = %+v, want the finished rename of Provision", j)
			}
			continue
		}
		if !reflect.DeepEqual(res, first) {
			a, _ := json.Marshal(res)
			b, _ := json.Marshal(first)
			t.Errorf("%s status answered\n%s\nhttp answered\n%s", s.name, a, b)
		}
	}

	var resumed *connector.OntologyJobStatus
	for _, s := range jobSurfaces {
		e.setJobState("running")
		res, o := s.resume(e, operator, id)
		if !o.ok || res.Job == nil {
			t.Fatalf("%s: resume refused: %s %s", s.name, o.code, o.raw)
		}
		j := *res.Job
		j.UpdatedAt = 0
		if j.ID != id || j.State != "done" {
			t.Errorf("%s: resume = %+v, want job %s done", s.name, j, id)
		}
		if resumed == nil {
			resumed = &j
		} else if !reflect.DeepEqual(j, *resumed) {
			t.Errorf("%s resume = %+v\nhttp = %+v", s.name, j, *resumed)
		}
		if _, o := s.resume(e, operator, id); o.ok || o.code != "job_not_found" {
			t.Errorf("%s: resume with nothing unfinished: ok=%v code %q (%.200s), want job_not_found", s.name, o.ok, o.code, o.raw)
		}
	}
}

// TestOntologyJob_SameRefusalOnEveryTransport: a member and an isolated user of the
// tenant are turned away at each transport's scope gate, for the status and the
// resume alike, and a refused resume leaves the job where it was.
func TestOntologyJob_SameRefusalOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	id := e.renameProvision(f, "Clause")
	e.setJobState("running")
	for _, s := range jobSurfaces {
		for _, who := range []struct{ name, bearer string }{{"a member", member}, {"an isolated user", isolated}} {
			if _, o := s.job(e, who.bearer); o.ok || o.code != "insufficient_scope" {
				t.Errorf("%s: %s's status: ok=%v code %q (%.200s), want refused at the scope gate", s.name, who.name, o.ok, o.code, o.raw)
			}
			if _, o := s.resume(e, who.bearer, id); o.ok || o.code != "insufficient_scope" {
				t.Errorf("%s: %s's resume: ok=%v code %q (%.200s), want refused at the scope gate", s.name, who.name, o.ok, o.code, o.raw)
			}
		}
	}
	if res, _ := e.jobHTTP(operator); res.Job == nil || res.Job.State != "running" {
		t.Errorf("after the refused resumes the job = %+v, want still running", res.Job)
	}
}

// TestOntologyJob_ResumeWithoutAnIDIsRefusedOnEveryTransport: a resume names the job it
// carries on. Without one it is refused as a malformed request on every transport —
// HTTP's 400 invalid_body (an empty body and {"id": ""} alike), gRPC's INVALID_ARGUMENT
// invalid_request, MCP's invalid_request — and the job is left where it was.
func TestOntologyJob_ResumeWithoutAnIDIsRefusedOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	e.renameProvision(f, "Clause")
	e.setJobState("running")
	want := map[string]string{"http": "invalid_body", "grpc": "invalid_request", "mcp": "invalid_request"}
	for _, s := range jobSurfaces {
		if _, o := s.resume(e, operator, ""); o.ok || o.code != want[s.name] {
			t.Errorf("%s: resume without an id: ok=%v code %q (%.200s), want %s", s.name, o.ok, o.code, o.raw, want[s.name])
		}
	}
	if resp, raw := e.do(http.MethodPost, "/v1/_ontology/job/resume", operator, `{"id":""}`, nil); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(raw), "invalid_body") {
		t.Errorf(`http: resume with {"id":""} = %d %s, want 400 invalid_body`, resp.StatusCode, raw)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+operator)
	if _, err := e.grpc.ResumeOntologyJob(ctx, &loomcyclepb.ResumeOntologyJobRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("grpc: resume without an id = %v, want INVALID_ARGUMENT", err)
	}
	if res, _ := e.jobHTTP(operator); res.Job == nil || res.Job.State != "running" {
		t.Errorf("after the resumes without an id the job = %+v, want still running", res.Job)
	}
}

// TestOntologyJob_BusyIsTheSameOnEveryTransport: while another call holds the ontology
// rename lease a resume is job_busy on every transport, and on gRPC it is
// FAILED_PRECONDITION (the state the call needs is not there yet), not ABORTED.
func TestOntologyJob_BusyIsTheSameOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	id := e.renameProvision(f, "Clause")
	e.setJobState("running")
	key := sqlmem.ScopeKey{Tenant: tenant, Scope: "tenant", ScopeID: tenant}
	if _, err := e.doc.SqlMem.DocPlane().Exec(context.Background(), key,
		`UPDATE document_leases SET owner = 'runtime:other', expires_at = ? WHERE name = 'ontology-rename'`,
		[]any{time.Now().Add(10 * time.Minute).UnixNano()}, 0); err != nil {
		t.Fatalf("hold the lease: %v", err)
	}
	for _, s := range jobSurfaces {
		if _, o := s.resume(e, operator, id); o.ok || o.code != "job_busy" {
			t.Errorf("%s: resume while the lease is held: ok=%v code %q (%.200s), want job_busy", s.name, o.ok, o.code, o.raw)
		}
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+operator)
	if _, err := e.grpc.ResumeOntologyJob(ctx, &loomcyclepb.ResumeOntologyJobRequest{Id: id}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("grpc: resume while the lease is held = %v, want FAILED_PRECONDITION", err)
	}
	if res, _ := e.jobHTTP(operator); res.Job == nil || res.Job.State != "running" {
		t.Errorf("after the busy resumes the job = %+v, want still running", res.Job)
	}
}

// TestPlanOntology_RenameJobBlockIsTheSameOnEveryTransport: the plan of renaming a
// class something rests on carries the class-rename job's block, and HTTP, gRPC and
// MCP give the same one, every field; a rename nothing rests on carries none on all
// three.
func TestPlanOntology_RenameJobBlockIsTheSameOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	changes := []connector.OntologyChange{
		{Op: connector.OntologyPlanRename, Entry: f.provision, Name: "Clause"},
		{Op: connector.OntologyPlanRename, Entry: f.cites, Name: "refers_to"},
	}
	var first connector.PlanOntologyResult
	for i, s := range planSurfaces {
		p, o := s.plan(e, operator, changes)
		if !o.ok {
			t.Fatalf("%s: plan refused: %s %s", s.name, o.code, o.raw)
		}
		p = normalizedPlan(p)
		for _, c := range p.Changes {
			if c.Job != nil && len(c.Job.Blocked) == 0 {
				c.Job.Blocked = nil
			}
		}
		if i == 0 {
			first = p
			if job := p.Changes[0].Job; job == nil || !job.Runnable || job.Kind != "class_rename" || len(job.Steps) == 0 || p.Changes[1].Job != nil {
				t.Fatalf("http plan's job blocks = %+v / %+v, want a runnable class_rename job for Provision only", job, p.Changes[1].Job)
			}
			continue
		}
		if !reflect.DeepEqual(p, first) {
			a, _ := json.Marshal(p.Changes)
			b, _ := json.Marshal(first.Changes)
			t.Errorf("%s changes\n%s\nhttp changes\n%s", s.name, a, b)
		}
	}
}
