package subjectmergetest

// The ontology change plan (RFC GQ P1.2): POST /v1/_ontology/plan, the
// PlanOntology RPC and the MCP ontology tool's op plan, over one real server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const planOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\n\n#### Article\n\n" +
	"## Relations\n@section relations\n\n### cites\n"

// planFixture is a tenant ontology (a class with a subclass, a relation), a
// document typed with each class and one statement of the relation.
type planFixture struct {
	provision, article, cites string
}

// opDocCall is docCall as the operator's own off-run call: only the operator
// may name a document the tenant ontology.
func (e *env) opDocCall(body string) map[string]any {
	e.t.Helper()
	res, err := e.doc.Execute(tools.WithCallerTier(e.docCtx(), tools.TierOperatorPlane), json.RawMessage(body))
	if err != nil || res.IsError {
		e.t.Fatalf("%s: %v %s", body, err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

func (e *env) seedPlanOntology() planFixture {
	e.t.Helper()
	out := e.opDocCall(fmt.Sprintf(`{"op":"import_md","scope":"tenant","markdown":%q}`, planOntologyMD))
	id, root := out["document_id"].(string), out["root_chunk_id"].(string)
	e.opDocCall(fmt.Sprintf(`{"op":"set_path","scope":"tenant","id":%q,"path":%q}`, id, meminject.OntologyPath))
	rev := e.opDocCall(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, root))["revision"]
	e.opDocCall(fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%v}`, root, meminject.OntologyConfirmedStatus, rev))

	var f planFixture
	chunks, _ := e.docCall(fmt.Sprintf(`{"op":"query_chunks","scope":"tenant","document_id":%q,"limit":50}`, id))["chunks"].([]any)
	for _, c := range chunks {
		m := c.(map[string]any)
		switch m["title"] {
		case "Provision":
			f.provision = m["id"].(string)
		case "Article":
			f.article = m["id"].(string)
		case "cites":
			f.cites = m["id"].(string)
		}
	}
	if f.provision == "" || f.article == "" || f.cites == "" {
		e.t.Fatalf("ontology chunks not found: %+v in %v", f, chunks)
	}
	e.docCall(`{"op":"create_document","scope":"tenant","title":"Rule","type":"Provision"}`)
	art := e.docCall(`{"op":"create_document","scope":"tenant","title":"Art 1","type":"Article"}`)["root_chunk_id"].(string)
	e.docCall(fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, art, e.dupRoot))
	return f
}

type planSurface struct {
	name string
	plan func(e *env, bearer string, changes []connector.OntologyChange) (connector.PlanOntologyResult, outcome)
}

var planSurfaces = []planSurface{
	{"http", (*env).planHTTP},
	{"grpc", (*env).planGRPC},
	{"mcp", (*env).planMCP},
}

// asPlan reads a success payload as the shared result type.
func (e *env) asPlan(raw string) connector.PlanOntologyResult {
	e.t.Helper()
	var p connector.PlanOntologyResult
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		e.t.Fatalf("not a plan: %s", raw)
	}
	return p
}

func (e *env) planHTTP(bearer string, changes []connector.OntologyChange) (connector.PlanOntologyResult, outcome) {
	body, _ := json.Marshal(changes)
	return e.planHTTPRaw(bearer, body)
}

// planHTTPRaw and planMCPRaw send the changes as given JSON, to ask with a key
// the shared type has no field for. gRPC has no twin: its message has fixed
// fields, so a stray key is the client's encode error, never a request.
func (e *env) planHTTPRaw(bearer string, changes json.RawMessage) (connector.PlanOntologyResult, outcome) {
	body, _ := json.Marshal(map[string]any{"changes": changes})
	resp, raw := e.do(http.MethodPost, "/v1/_ontology/plan", bearer, string(body), nil)
	if resp.StatusCode == http.StatusOK {
		return e.asPlan(string(raw)), outcome{ok: true, raw: string(raw)}
	}
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient") {
		return connector.PlanOntologyResult{}, outcome{code: "insufficient_scope", raw: string(raw)}
	}
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	return connector.PlanOntologyResult{}, outcome{code: b.Code, raw: string(raw)}
}

// planGRPC reads the response back into the shared result type, field by field,
// so a field the RPC dropped shows as a difference from HTTP.
func (e *env) planGRPC(bearer string, changes []connector.OntologyChange) (connector.PlanOntologyResult, outcome) {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	req := &loomcyclepb.PlanOntologyRequest{}
	for _, c := range changes {
		req.Changes = append(req.Changes, &loomcyclepb.OntologyChange{Op: c.Op, Entry: c.Entry, Name: c.Name, Parent: c.Parent, Body: c.Body})
	}
	resp, err := e.grpc.PlanOntology(ctx, req)
	if err != nil {
		st := status.Convert(err)
		o := outcome{raw: st.Message()}
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				o.code = info.GetReason()
			}
		}
		if o.code == "" && st.Code() == codes.PermissionDenied && strings.Contains(st.Message(), "insufficient scope") {
			o.code = "insufficient_scope"
		}
		return connector.PlanOntologyResult{}, o
	}
	entry := func(p *loomcyclepb.OntologyPlanEntry) connector.OntologyPlanEntry {
		return connector.OntologyPlanEntry{ChunkID: p.GetChunkId(), Name: p.GetName(), Section: p.GetSection(), Kind: p.GetKind()}
	}
	out := connector.PlanOntologyResult{
		Tenant: resp.GetTenant(), Fingerprint: resp.GetFingerprint(), Issues: resp.GetIssues(), NewIssues: resp.GetNewIssues(),
		ScopesCounted: int(resp.GetScopesCounted()), ScopesSkipped: int(resp.GetScopesSkipped()), Complete: resp.GetComplete(),
	}
	for _, c := range resp.GetChanges() {
		pc := connector.OntologyPlannedChange{Op: c.GetOp(), Entry: entry(c.GetEntry()), Risk: c.GetRisk(), Notes: c.GetNotes()}
		for _, s := range c.GetSubtree() {
			pc.Subtree = append(pc.Subtree, entry(s))
		}
		if len(c.GetCounts()) > 0 {
			pc.Counts = map[string]connector.OntologyPlanCount{}
		}
		for k, n := range c.GetCounts() {
			cnt := connector.OntologyPlanCount{N: int(n.GetN()), Effect: n.GetEffect(), TenantScope: int(n.GetTenantScope()),
				OtherScopes: int(n.GetOtherScopes()), Sample: n.GetSample()}
			for s, v := range n.GetByStatus() {
				if cnt.ByStatus == nil {
					cnt.ByStatus = map[string]int{}
				}
				cnt.ByStatus[s] = int(v)
			}
			pc.Counts[k] = cnt
		}
		out.Changes = append(out.Changes, pc)
	}
	return out, outcome{ok: true, raw: resp.String()}
}

func (e *env) planMCP(bearer string, changes []connector.OntologyChange) (connector.PlanOntologyResult, outcome) {
	body, _ := json.Marshal(changes)
	return e.planMCPRaw(bearer, body)
}

func (e *env) planMCPRaw(bearer string, changes json.RawMessage) (connector.PlanOntologyResult, outcome) {
	o := e.toolMCP(bearer, "ontology", map[string]any{"op": "plan", "changes": changes})
	if !o.ok {
		return connector.PlanOntologyResult{}, o
	}
	return e.asPlan(o.raw), o
}

// normalizedPlan makes an empty list and an absent one the same (JSON null and
// [] are one answer here, and proto3 cannot tell them apart).
func normalizedPlan(p connector.PlanOntologyResult) connector.PlanOntologyResult {
	ne := func(s []string) []string {
		if len(s) == 0 {
			return nil
		}
		return s
	}
	p.Issues, p.NewIssues = ne(p.Issues), ne(p.NewIssues)
	for i := range p.Changes {
		c := &p.Changes[i]
		c.Notes = ne(c.Notes)
		if len(c.Subtree) == 0 {
			c.Subtree = nil
		}
		if len(c.Counts) == 0 {
			c.Counts = nil
		}
		for k, n := range c.Counts {
			n.Sample = ne(n.Sample)
			if len(n.ByStatus) == 0 {
				n.ByStatus = nil
			}
			c.Counts[k] = n
		}
	}
	return p
}

// TestPlanOntology_SamePlanOnEveryTransport (RFC GQ P1.2/D10): the same changes,
// asked by a tenant operator over HTTP, gRPC and MCP of one server, give the
// same plan — every field — and none writes (the fingerprint is unchanged by
// the asking).
func TestPlanOntology_SamePlanOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	changes := []connector.OntologyChange{
		{Op: connector.OntologyPlanDelete, Entry: f.provision},
		{Op: connector.OntologyPlanRename, Entry: f.cites, Name: "refers_to"},
		{Op: connector.OntologyPlanEdit, Entry: f.article, Body: "A numbered part of a provision."},
	}
	var first connector.PlanOntologyResult
	for i, s := range planSurfaces {
		p, o := s.plan(e, operator, changes)
		if !o.ok {
			t.Fatalf("%s: plan refused: %s %s", s.name, o.code, o.raw)
		}
		p = normalizedPlan(p)
		if i == 0 {
			first = p
			typed := p.Changes[0].Counts["typed_documents"]
			if p.Tenant != tenant || len(p.Fingerprint) != 64 || len(p.Changes) != 3 ||
				p.Changes[0].Risk != connector.OntologyRiskDestroys || typed.N != 2 || len(p.Changes[0].Subtree) != 1 {
				t.Fatalf("http plan is not the fixture's: %+v", p)
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

// TestPlanOntology_SameRefusalOnEveryTransport: a refusal carries the same code
// on every transport, and a member token of the tenant is turned away at each
// transport's scope gate.
func TestPlanOntology_SameRefusalOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	f := e.seedPlanOntology()
	cases := []struct {
		name    string
		bearer  string
		changes []connector.OntologyChange
		code    string
	}{
		{"a member", member, []connector.OntologyChange{{Op: "retire", Entry: f.provision}}, "insufficient_scope"},
		{"no changes", operator, nil, "invalid_request"},
		{"an unknown op", operator, []connector.OntologyChange{{Op: "merge", Entry: f.provision}}, "invalid_request"},
		{"no such entry", operator, []connector.OntologyChange{{Op: "retire", Entry: "no-such-chunk"}}, "entry_not_found"},
		{"over the bound", operator, []connector.OntologyChange{{Op: "edit", Entry: f.provision,
			Body: strings.Repeat("a", connector.OntologyPlanMaxRequestBytes)}}, "invalid_body"},
		// Each body is within the Document's 32 KiB, and the two are under the
		// bound as protobuf (one byte a quote) but over it as JSON (two): the
		// bound is the JSON size on every transport, so gRPC refuses it too.
		{"over the bound as JSON only", operator, []connector.OntologyChange{
			{Op: "edit", Entry: f.provision, Body: strings.Repeat(`"`, 20000)},
			{Op: "edit", Entry: f.article, Body: strings.Repeat(`"`, 20000)}}, "invalid_body"},
	}
	for _, s := range planSurfaces {
		for _, c := range cases {
			if _, o := s.plan(e, c.bearer, c.changes); o.ok || o.code != c.code {
				t.Errorf("%s %s: ok=%v code %q, want %q (%.200s)", s.name, c.name, o.ok, o.code, c.code, o.raw)
			}
		}
	}
	// A key the change has no field for (a misspelt parent) is refused, not
	// dropped. Only HTTP and MCP can send one; see planHTTPRaw.
	unknown := json.RawMessage(`[{"op":"retire","entry":"` + f.provision + `","prent":"x"}]`)
	for _, s := range []struct {
		name string
		plan func(string, json.RawMessage) (connector.PlanOntologyResult, outcome)
	}{{"http", e.planHTTPRaw}, {"mcp", e.planMCPRaw}} {
		if _, o := s.plan(operator, unknown); o.ok || o.code != "invalid_body" {
			t.Errorf("%s an unknown key: ok=%v code %q, want invalid_body (%.200s)", s.name, o.ok, o.code, o.raw)
		}
	}
}
