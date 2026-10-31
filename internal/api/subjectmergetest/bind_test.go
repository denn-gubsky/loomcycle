package subjectmergetest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// bindSurface is one transport's subject binding.
type bindSurface struct {
	name string
	bind func(e *env, bearer, subject, principal string) outcome
}

var bindSurfaces = []bindSurface{
	{"http", (*env).bindHTTP},
	{"grpc", (*env).bindGRPC},
	{"mcp", (*env).bindMCP},
}

// users registers the tenant's users the binding may name.
func (e *env) users(subjects ...string) {
	e.t.Helper()
	for _, s := range subjects {
		if err := e.st.UserCreate(context.Background(), store.UserRow{TenantID: tenant, Subject: s, AccessMode: "tenant", Status: "active"}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) bindHTTP(bearer, subject, principal string) outcome {
	body, _ := json.Marshal(map[string]string{"subject": subject, "principal": principal})
	resp, raw := e.do(http.MethodPost, "/v1/_ontology/subjects/principal", bearer, string(body), nil)
	if resp.StatusCode == http.StatusOK {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return outcome{ok: true, result: m, raw: string(raw)}
	}
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient") {
		return outcome{code: "insufficient_scope", raw: string(raw)}
	}
	var b struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &b)
	return outcome{code: b.Code, raw: b.Error}
}

// grpcRefusal reads a refused RPC as an outcome: the ErrorInfo reason, or
// "insufficient_scope" for a caller the scope gate turned away.
func grpcRefusal(err error) outcome {
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
	return o
}

func (e *env) bindGRPC(bearer, subject, principal string) outcome {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.BindSubjectPrincipal(ctx, &loomcyclepb.BindSubjectPrincipalRequest{Subject: subject, Principal: principal})
	if err != nil {
		return grpcRefusal(err)
	}
	// Rendered in the shared result's JSON names, so the three compare as one.
	m := map[string]any{
		"tenant": resp.GetTenant(), "document_id": resp.GetDocumentId(), "root_chunk_id": resp.GetRootChunkId(),
		"natural_key": resp.GetNaturalKey(), "principal": resp.GetPrincipal(), "changed": resp.GetChanged(),
	}
	if resp.GetPath() != "" {
		m["path"] = resp.GetPath()
	}
	if resp.GetPrevious() != "" {
		m["previous"] = resp.GetPrevious()
	}
	return outcome{ok: true, result: m, raw: resp.String()}
}

func (e *env) bindMCP(bearer, subject, principal string) outcome {
	o := e.toolMCP(bearer, "ontology", map[string]string{"op": "bind_principal", "subject": subject, "principal": principal})
	if !o.ok && o.code != "" && o.code != "insufficient_scope" {
		var b struct{ Error string }
		_ = json.Unmarshal([]byte(o.raw), &b)
		o.raw = b.Error
	}
	return o
}

// boundTo is what the store says: the natural keys of the tenant subjects bound
// to principal, as an erasure of that principal would find them.
func (e *env) boundTo(principal string) []string {
	e.t.Helper()
	got, err := e.doc.TenantSubjectsBoundTo(e.docCtx(), principal)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []string{}
	for _, b := range got {
		out = append(out, b.NaturalKey)
	}
	return out
}

// TestBindSubjectPrincipal_SameResultOnEveryTransport (RFC FR §5, P4): the same
// bind and the same clear, driven once per transport against identical tenants,
// answer the same and leave the same store.
func TestBindSubjectPrincipal_SameResultOnEveryTransport(t *testing.T) {
	var first []map[string]any
	for _, s := range bindSurfaces {
		e := newEnv(t)
		e.users("u-ada")
		bound := s.bind(e, operator, "person:ada-king", "u-ada")
		if !bound.ok {
			t.Fatalf("%s: bind refused: %s %s", s.name, bound.code, bound.raw)
		}
		afterBind := e.boundTo("u-ada")
		cleared := s.bind(e, operator, e.dup, "")
		if !cleared.ok {
			t.Fatalf("%s: clear refused: %s %s", s.name, cleared.code, cleared.raw)
		}
		got := []map[string]any{
			normalizedBinding(e, bound.result), {"bound": afterBind},
			normalizedBinding(e, cleared.result), {"bound": e.boundTo("u-ada")},
		}
		if first == nil {
			first = got
			want := []map[string]any{
				{"tenant": tenant, "is_duplicate": true, "natural_key": "person:ada-king", "path": "/facts/ada-king", "principal": "u-ada", "changed": true},
				{"bound": []string{"person:ada-king"}},
				{"tenant": tenant, "is_duplicate": true, "natural_key": "person:ada-king", "path": "/facts/ada-king", "principal": "", "previous": "u-ada", "changed": true},
				{"bound": []string{}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: got %v, want %v", s.name, got, want)
			}
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Errorf("%s answered %v, http answered %v", s.name, got, first)
		}
	}
}

// normalizedBinding drops the environment's random ids from a bind result, keeping
// what they say: that it names the seeded duplicate.
func normalizedBinding(e *env, r map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		switch k {
		case "document_id", "root_chunk_id":
		default:
			out[k] = v
		}
	}
	out["is_duplicate"] = r["document_id"] == e.dup && r["root_chunk_id"] == e.dupRoot
	return out
}

// TestBindSubjectPrincipal_SameRefusalOnEveryTransport: each refusal carries the
// same code on every transport, none writes anything, and a conflict names the
// subject that holds the binding.
func TestBindSubjectPrincipal_SameRefusalOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.users("u-ada", "u-eve")
	cases := []struct{ name, subject, principal, code string }{
		{"no such subject", "person:nobody", "u-ada", "subject_not_found"},
		{"not a subject", e.report, "u-ada", "not_a_subject"},
		{"not a user of the tenant", e.dup, "u-ghost", "principal_not_found"},
		{"missing subject", "", "u-ada", "invalid_request"},
	}
	for _, s := range bindSurfaces {
		for _, c := range cases {
			if o := s.bind(e, operator, c.subject, c.principal); o.ok || o.code != c.code {
				t.Errorf("%s %s: ok=%v code %q, want %q (%s)", s.name, c.name, o.ok, o.code, c.code, o.raw)
			}
		}
	}
	if b := e.boundTo("u-ghost"); len(b) != 0 {
		t.Fatalf("a refused bind wrote: %v", b)
	}
	if o := bindSurfaces[0].bind(e, operator, e.dup, "u-ada"); !o.ok {
		t.Fatalf("bind: %s %s", o.code, o.raw)
	}
	for _, s := range bindSurfaces {
		o := s.bind(e, operator, e.surv, "u-ada")
		if o.ok || o.code != "binding_conflict" || !strings.Contains(o.raw, "person:ada-king") {
			t.Errorf("%s: a second subject for u-ada: ok=%v code %q (%s), want binding_conflict naming person:ada-king", s.name, o.ok, o.code, o.raw)
		}
	}
	if b := e.boundTo("u-ada"); !reflect.DeepEqual(b, []string{"person:ada-king"}) {
		t.Errorf("after the refused binds u-ada is bound to %v", b)
	}
}

// TestBindSubjectPrincipal_MembersAreRefusedOnEveryTransport (RFC FR §9): a member
// token of the tenant is refused the binding on all three transports, and nothing
// is written.
func TestBindSubjectPrincipal_MembersAreRefusedOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.users("u-ada")
	for _, s := range bindSurfaces {
		if o := s.bind(e, member, e.dup, "u-ada"); o.ok || o.code != "insufficient_scope" {
			t.Errorf("%s: a member's bind: ok=%v code %q (%s), want refused at the scope gate", s.name, o.ok, o.code, o.raw)
		}
	}
	if b := e.boundTo("u-ada"); len(b) != 0 {
		t.Fatalf("a member's bind was written: %v", b)
	}
}

// TestMergeSubjects_PrincipalConflictOnEveryTransport (RFC FR §3 step 3): two
// subjects bound to different users are refused a merge on every transport with
// the same code, and nothing changes.
func TestMergeSubjects_PrincipalConflictOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.users("u-ada", "u-eve")
	if o := e.bindHTTP(operator, e.dup, "u-ada"); !o.ok {
		t.Fatalf("bind: %s %s", o.code, o.raw)
	}
	if o := e.bindHTTP(operator, e.surv, "u-eve"); !o.ok {
		t.Fatalf("bind: %s %s", o.code, o.raw)
	}
	for _, s := range surfaces {
		if o := s.merge(e, operator, e.dup, e.surv); o.ok || o.code != "principal_conflict" {
			t.Errorf("%s: ok=%v code %q, want principal_conflict (%s)", s.name, o.ok, o.code, o.raw)
		}
	}
	if st := e.state(); st["dup_status"] == "merged" || len(st["events"].([]map[string]any)) != 0 {
		t.Fatalf("a refused merge changed the store: %v", st)
	}
	if a, v := e.boundTo("u-ada"), e.boundTo("u-eve"); !reflect.DeepEqual(a, []string{"person:ada-king"}) || !reflect.DeepEqual(v, []string{"person:ada-lovelace"}) {
		t.Errorf("bindings after a refused merge: u-ada %v, u-eve %v", a, v)
	}
}

// erasureBound reads the bound_subjects of an erasure report on each transport, in
// the shared JSON names.
func (e *env) erasureBound(transport, subject string) []map[string]any {
	e.t.Helper()
	switch transport {
	case "http":
		resp, raw := e.do(http.MethodGet, "/v1/_erasure?subject="+url.QueryEscape(subject), operator, "", nil)
		if resp.StatusCode != http.StatusOK {
			e.t.Fatalf("GET /v1/_erasure = %d: %s", resp.StatusCode, raw)
		}
		var rep struct {
			BoundSubjects []map[string]any `json:"bound_subjects"`
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			e.t.Fatal(err)
		}
		return rep.BoundSubjects
	case "grpc":
		ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+operator)
		rep, err := e.grpc.ErasureReport(ctx, &loomcyclepb.ErasureReportRequest{Subject: subject})
		if err != nil {
			e.t.Fatal(err)
		}
		out := []map[string]any{}
		for _, b := range rep.GetBoundSubjects() {
			out = append(out, map[string]any{"document_id": b.GetDocumentId(), "natural_key": b.GetNaturalKey(), "path": b.GetPath()})
		}
		return out
	default:
		o := e.toolMCP(operator, "erasure", map[string]string{"op": "report", "subject": subject})
		if !o.ok {
			e.t.Fatalf("mcp erasure report: %s %s", o.code, o.raw)
		}
		out := []map[string]any{}
		raw, _ := json.Marshal(o.result["bound_subjects"])
		_ = json.Unmarshal(raw, &out)
		return out
	}
}

// TestErasure_FindsTheMergedSubjectOnEveryTransport (RFC FR §3, §5): an erasure
// report names the subject bound to the person on every transport, gRPC included;
// and after a merge moves the binding, it names the survivor, which now holds the
// person's facts, and not the retired duplicate.
func TestErasure_FindsTheMergedSubjectOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.users("u-ada")
	if o := e.bindHTTP(operator, e.dup, "u-ada"); !o.ok {
		t.Fatalf("bind: %s %s", o.code, o.raw)
	}
	want := func(doc, key, path string) []map[string]any {
		return []map[string]any{{"document_id": doc, "natural_key": key, "path": path}}
	}
	for _, tr := range []string{"http", "grpc", "mcp"} {
		if got := e.erasureBound(tr, "u-ada"); !reflect.DeepEqual(got, want(e.dup, "person:ada-king", "/facts/ada-king")) {
			t.Errorf("%s: bound_subjects = %v, want the duplicate", tr, got)
		}
	}
	if o := e.mergeHTTP(operator, e.dup, e.surv); !o.ok {
		t.Fatalf("merge: %s %s", o.code, o.raw)
	}
	for _, tr := range []string{"http", "grpc", "mcp"} {
		if got := e.erasureBound(tr, "u-ada"); !reflect.DeepEqual(got, want(e.surv, "person:ada-lovelace", "/facts/ada-lovelace")) {
			t.Errorf("%s after the merge: bound_subjects = %v, want the survivor", tr, got)
		}
	}
	if fmt.Sprint(e.boundTo("u-ada")) != "[person:ada-lovelace]" {
		t.Errorf("store after the merge: u-ada bound to %v", e.boundTo("u-ada"))
	}
}
