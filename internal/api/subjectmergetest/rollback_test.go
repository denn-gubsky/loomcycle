package subjectmergetest

// Rolling the ontology back to a release (RFC GQ P5.7; its transports in P5.6): the
// plan of {op: release, seq} and POST /v1/_ontology/rollback, the PlanOntology and
// ApplyOntologyRollback RPCs, and the MCP ontology tool's ops plan and rollback, over
// one real server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// rollbackOutcome is one transport's answer to a rollback: the report on success,
// else the refusal's code and what its 409 carries beside it.
type rollbackOutcome struct {
	outcome
	res         connector.ApplyOntologyRollbackResult
	fingerprint string
	applied     []connector.OntologyRollbackStep
}

func (e *env) rollbackHTTP(bearer string, seq int64, fp string) rollbackOutcome {
	resp, raw := e.do(http.MethodPost, "/v1/_ontology/rollback", bearer, fmt.Sprintf(`{"seq":%d,"plan_fingerprint":%q}`, seq, fp), nil)
	o := rollbackOutcome{outcome: outcome{ok: resp.StatusCode == http.StatusOK, raw: string(raw)}}
	if o.ok {
		_ = json.Unmarshal(raw, &o.res)
		return o
	}
	var b struct {
		Code        string                           `json:"code"`
		Fingerprint string                           `json:"fingerprint"`
		Applied     []connector.OntologyRollbackStep `json:"applied"`
	}
	_ = json.Unmarshal(raw, &b)
	o.code, o.fingerprint, o.applied = b.Code, b.Fingerprint, b.Applied
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient") {
		o.code = "insufficient_scope"
	}
	return o
}

func (e *env) rollbackGRPC(bearer string, seq int64, fp string) rollbackOutcome {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.ApplyOntologyRollback(ctx, &loomcyclepb.ApplyOntologyRollbackRequest{Seq: seq, PlanFingerprint: fp})
	if err != nil {
		o := rollbackOutcome{outcome: grpcOutcome(err)}
		for _, d := range status.Convert(err).Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				o.fingerprint = info.GetMetadata()["fingerprint"]
				if a := info.GetMetadata()["applied"]; a != "" {
					_ = json.Unmarshal([]byte(a), &o.applied)
				}
			}
		}
		return o
	}
	r := resp.GetRollback()
	rep := connector.OntologyRollbackReport{Release: r.GetRelease(), ReleaseContentVersion: r.GetReleaseContentVersion(),
		ContentVersion: r.GetContentVersion(), Matches: r.GetMatches(), Risk: r.GetRisk(), Applied: []connector.OntologyRollbackStep{},
		RollbackRelease: r.GetRollbackRelease(), Notes: r.GetNotes(), Warnings: r.GetWarnings()}
	for _, a := range r.GetApplied() {
		rep.Applied = append(rep.Applied, connector.OntologyRollbackStep{Op: a.GetOp(), Entry: a.GetEntry(), Name: a.GetName(), NewID: a.GetNewId()})
	}
	for _, j := range r.GetRenameJobs() {
		rep.RenameJobs = append(rep.RenameJobs, *jobFromProto(j))
	}
	return rollbackOutcome{outcome: outcome{ok: true, raw: resp.String()},
		res: connector.ApplyOntologyRollbackResult{Tenant: resp.GetTenant(), Rollback: rep}}
}

func (e *env) rollbackMCP(bearer string, seq int64, fp string) rollbackOutcome {
	o := rollbackOutcome{outcome: e.toolMCP(bearer, "ontology", map[string]any{"op": "rollback", "seq": seq, "plan_fingerprint": fp})}
	if o.ok {
		_ = json.Unmarshal([]byte(o.raw), &o.res)
		return o
	}
	var b struct {
		Fingerprint string                           `json:"fingerprint"`
		Applied     []connector.OntologyRollbackStep `json:"applied"`
	}
	_ = json.Unmarshal([]byte(o.raw), &b)
	o.fingerprint, o.applied = b.Fingerprint, b.Applied
	return o
}

type rollbackSurface struct {
	name     string
	rollback func(e *env, bearer string, seq int64, fp string) rollbackOutcome
}

var rollbackSurfaces = []rollbackSurface{
	{"http", (*env).rollbackHTTP},
	{"grpc", (*env).rollbackGRPC},
	{"mcp", (*env).rollbackMCP},
}

// releasePlan is the operator's plan of rolling back to release seq, over HTTP.
func (e *env) releasePlan(seq int64) connector.PlanOntologyResult {
	e.t.Helper()
	p, o := e.planHTTP(operator, []connector.OntologyChange{{Op: connector.OntologyPlanRelease, Seq: seq}})
	if !o.ok {
		e.t.Fatalf("plan of release %d: %s %s", seq, o.code, o.raw)
	}
	return p
}

// TestPlanOntology_ReleasePlanIsTheSameOnEveryTransport: the plan of rolling back
// to release 1 — the fixture's added class deleted, carrying release 1 — is the same
// over HTTP, gRPC (which now carries seq and release) and MCP, every field.
func TestPlanOntology_ReleasePlanIsTheSameOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.seedReleases()
	changes := []connector.OntologyChange{{Op: connector.OntologyPlanRelease, Seq: 1}}
	var first connector.PlanOntologyResult
	for i, s := range planSurfaces {
		p, o := s.plan(e, operator, changes)
		if !o.ok {
			t.Fatalf("%s: plan refused: %s %s", s.name, o.code, o.raw)
		}
		p = normalizedPlan(p)
		if i == 0 {
			first = p
			if len(p.Changes) != 1 || p.Changes[0].Op != connector.OntologyPlanDelete || p.Changes[0].Entry.Name != "Clause" || p.Changes[0].Release != 1 {
				t.Fatalf("http plan = %+v, want Clause deleted, carrying release 1", p.Changes)
			}
			continue
		}
		if !reflect.DeepEqual(p, first) {
			a, _ := json.Marshal(p)
			b, _ := json.Marshal(first)
			t.Errorf("%s answered\n%s\nhttp answered\n%s", s.name, a, b)
		}
	}
}

// TestOntologyRollback_SameAnswerOnEveryTransport: each transport, on its own server
// seeded alike, refuses a rollback with no fingerprint (plan_required) and with a
// stale one (plan_stale), each carrying the current fingerprint and writing nothing;
// with the plan's fingerprint each deletes the added class, leaves the release's
// content version and takes a rollback release, and the reports are the same.
func TestOntologyRollback_SameAnswerOnEveryTransport(t *testing.T) {
	var first *connector.OntologyRollbackReport
	for _, s := range rollbackSurfaces {
		e := newEnv(t)
		e.seedReleases()
		fp := e.releasePlan(1).Fingerprint
		if o := s.rollback(e, operator, 1, ""); o.ok || o.code != "plan_required" || o.fingerprint != fp {
			t.Errorf("%s: no fingerprint: ok=%v code %q fingerprint %q (%.200s), want plan_required carrying %s", s.name, o.ok, o.code, o.fingerprint, o.raw, fp)
		}
		if o := s.rollback(e, operator, 1, strings.Repeat("0", 64)); o.ok || o.code != "plan_stale" || o.fingerprint != fp {
			t.Errorf("%s: a stale fingerprint: ok=%v code %q fingerprint %q (%.200s), want plan_stale carrying %s", s.name, o.ok, o.code, o.fingerprint, o.raw, fp)
		}
		if got := e.releasePlan(1).Fingerprint; got != fp {
			t.Fatalf("%s: a refused rollback changed the ontology (fingerprint %s → %s)", s.name, fp, got)
		}
		o := s.rollback(e, operator, 1, fp)
		if !o.ok {
			t.Fatalf("%s: rollback refused: %s %s", s.name, o.code, o.raw)
		}
		rep := o.res.Rollback
		if o.res.Tenant != tenant || !rep.Matches || rep.RollbackRelease != 3 || len(rep.Applied) != 1 ||
			rep.Applied[0].Op != connector.OntologyPlanDelete || rep.Applied[0].Name != "Clause" {
			t.Errorf("%s: rollback = %+v, want Clause deleted, matching release 1, rollback release 3", s.name, o.res)
		}
		// Chunk ids — and so the content versions, which hash them — differ from server
		// to server; the rest of the report must not.
		if rep.ContentVersion == "" || rep.ContentVersion != rep.ReleaseContentVersion {
			t.Errorf("%s: content version %q, release's %q; want equal", s.name, rep.ContentVersion, rep.ReleaseContentVersion)
		}
		rep.ContentVersion, rep.ReleaseContentVersion = "", ""
		for i := range rep.Applied {
			rep.Applied[i].Entry = ""
		}
		if first == nil {
			first = &rep
		} else if !reflect.DeepEqual(rep, *first) {
			t.Errorf("%s report = %+v\nhttp = %+v", s.name, rep, *first)
		}
	}
}

// TestOntologyRollback_SameRefusalOnEveryTransport: a member and an isolated user of
// the tenant are turned away at each transport's scope gate and write nothing; a
// release that does not exist is the plan's entry_not_found, and a seq that is not a
// release's number invalid_request, on all three.
func TestOntologyRollback_SameRefusalOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.seedReleases()
	fp := e.releasePlan(1).Fingerprint
	for _, s := range rollbackSurfaces {
		for _, who := range []struct{ name, bearer string }{{"a member", member}, {"an isolated user", isolated}} {
			if o := s.rollback(e, who.bearer, 1, fp); o.ok || o.code != "insufficient_scope" {
				t.Errorf("%s: %s's rollback: ok=%v code %q (%.200s), want refused at the scope gate", s.name, who.name, o.ok, o.code, o.raw)
			}
		}
		if o := s.rollback(e, operator, 9, fp); o.ok || o.code != "entry_not_found" {
			t.Errorf("%s: no such release: ok=%v code %q (%.200s), want entry_not_found", s.name, o.ok, o.code, o.raw)
		}
		if o := s.rollback(e, operator, 0, fp); o.ok || o.code != "invalid_request" {
			t.Errorf("%s: seq 0: ok=%v code %q (%.200s), want invalid_request", s.name, o.ok, o.code, o.raw)
		}
	}
	if got := e.releasePlan(1).Fingerprint; got != fp {
		t.Errorf("a refused rollback changed the ontology (fingerprint %s → %s)", fp, got)
	}
}
