package grpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// calcAdapter is the gRPC server over the real connector (the HTTP server's
// Calc, which takes one unit of the token's run rate per call and refuses
// `from` / `to`), holding only the Calc tool.
func calcAdapter(t *testing.T) *Server {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-calc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4}}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{&builtin.Calc{}}, concurrency.New(4, 4, time.Second), st)
	return New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
}

func calcReq(in string) *loomcyclepb.CalcRequest {
	return &loomcyclepb.CalcRequest{InputJson: []byte(in)}
}

// calcErrorInfo is a refusal's google.rpc.ErrorInfo.
func calcErrorInfo(t *testing.T, err error) (codes.Code, *errdetails.ErrorInfo) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok {
			return st.Code(), ei
		}
	}
	t.Fatalf("%v carries no ErrorInfo", err)
	return 0, nil
}

// Every principal POST /v1/_calc admits passes the Calc RPC's gate — a tenant
// operator, a member and an isolated user — and gets the evaluator's exact
// answer as result_json; a token limited to certain agents is refused
// PermissionDenied, as it is 403 over HTTP.
func TestGrpcCalc_EveryTokenButARunPlaneOneGetsTheEvaluatorsAnswer(t *testing.T) {
	method := grpcMethodPrefix + "Calc"
	adapter := calcAdapter(t)
	for _, p := range []auth.Principal{
		{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}},
		{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeUser}},
	} {
		ctx := auth.WithPrincipal(context.Background(), p)
		if err := enforceScope(ctx, method); err != nil {
			t.Errorf("%s %v on Calc: %v, want admitted", p.Subject, p.Scopes, err)
			continue
		}
		resp, err := adapter.Calc(ctx, calcReq(`{"expr":"0.1 + 0.2"}`))
		if err != nil {
			t.Fatalf("%s: Calc: %v", p.Subject, err)
		}
		var out struct {
			Result json.Number `json:"result"`
			Exact  bool        `json:"exact"`
		}
		if err := json.Unmarshal(resp.GetResultJson(), &out); err != nil || out.Result != "0.3" || !out.Exact {
			t.Errorf("%s: result_json %s, want 0.3 exact", p.Subject, resp.GetResultJson())
		}
	}
	runPlane := auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant},
		Restrictions: auth.Restrictions{Agents: []string{"score"}}}
	if err := enforceScope(auth.WithPrincipal(context.Background(), runPlane), method); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a run-plane token on Calc: %v, want PermissionDenied", err)
	}
}

// A refusal is a status whose code is its category's — InvalidArgument for a
// validation refusal, FailedPrecondition for a business one — and whose
// ErrorInfo names the tool's code, the category and the hint.
func TestGrpcCalc_ARefusalMapsItsCategoryToTheStatusCode(t *testing.T) {
	adapter := calcAdapter(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob"})

	_, err := adapter.Calc(ctx, calcReq(`{"expr":"2**3"}`))
	code, ei := calcErrorInfo(t, err)
	if code != codes.InvalidArgument || ei.GetReason() != "syntax" || ei.GetMetadata()["category"] != "validation" ||
		!strings.Contains(ei.GetMetadata()["hint"], "power is `^`") || ei.GetMetadata()["pos"] == "" {
		t.Errorf("2**3 = %v / %+v, want InvalidArgument syntax with the hint and pos", code, ei)
	}

	_, err = adapter.Calc(ctx, calcReq(`{"expr":"1 / 0"}`))
	code, ei = calcErrorInfo(t, err)
	if code != codes.FailedPrecondition || ei.GetReason() != "division_by_zero" || ei.GetMetadata()["category"] != "business" {
		t.Errorf("1 / 0 = %v / %+v, want FailedPrecondition division_by_zero", code, ei)
	}

	_, err = adapter.Calc(ctx, calcReq(`{"expr":"2","to":"memory:user/k"}`))
	code, ei = calcErrorInfo(t, err)
	if code != codes.InvalidArgument || ei.GetReason() != "bad_request" || !strings.Contains(status.Convert(err).Message(), "data by reference needs a run") {
		t.Errorf("to = %v / %+v (%v), want InvalidArgument: data by reference needs a run", code, ei, err)
	}

	if _, err := adapter.Calc(ctx, &loomcyclepb.CalcRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty input_json: %v, want InvalidArgument", err)
	}
}

// Each call takes one unit of the token's run rate: past it the call is
// ResourceExhausted with the limiter's wait as RetryInfo.
func TestGrpcCalc_PastTheTokensRunRateIsResourceExhausted(t *testing.T) {
	adapter := calcAdapter(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "rita",
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 1}}})
	if _, err := adapter.Calc(ctx, calcReq(`{"expr":"1 + 1"}`)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := adapter.Calc(ctx, calcReq(`{"expr":"1 + 1"}`))
	v := viewOf(t, err)
	if v.code != codes.ResourceExhausted || v.reason != "rate_limited" || !v.hasRetry || v.retryIn <= 0 || v.retryIn > time.Minute {
		t.Errorf("second call = %v / %q retry %v %v (%v), want ResourceExhausted rate_limited with RetryInfo", v.code, v.reason, v.hasRetry, v.retryIn, err)
	}
}

// A refusal carries no pointer at the in-run Context tool, which a gRPC
// caller does not have: the status holds the detail, the code, the hint and
// the position, never the run's next-step sentence.
func TestGrpcCalc_ARefusalNamesNoInRunHelpCall(t *testing.T) {
	adapter := calcAdapter(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob"})
	_, err := adapter.Calc(ctx, calcReq(`{"expr":"1/0"}`))
	code, ei := calcErrorInfo(t, err)
	if code != codes.FailedPrecondition || ei.GetReason() != "division_by_zero" {
		t.Fatalf("1/0 = %v / %+v, want FailedPrecondition division_by_zero", code, ei)
	}
	all := status.Convert(err).Message()
	for k, v := range ei.GetMetadata() {
		all += " " + k + "=" + v
	}
	if strings.Contains(all, "Context op=help") {
		t.Errorf("the refusal %q points at the in-run Context tool", all)
	}
}
