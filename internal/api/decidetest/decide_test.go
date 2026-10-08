package decidetest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/limits"
)

// member is a non-isolated user token that may create runs and holds no
// substrate:tenant: with the gate on, it may NOT spend the operator's provider
// key. It reaches all three surfaces (the MCP route through the member path).
func (e *env) member(subject string) string {
	return e.mint("acme", subject, auth.ScopeRunsCreate, auth.ScopeRunsRead)
}

// wantStatus is a refusal's status on each surface.
type wantStatus struct{ http, grpc, mcp string }

func (w wantStatus) on(surface string) string {
	switch surface {
	case "http":
		return w.http
	case "grpc":
		return w.grpc
	}
	return w.mcp
}

// TestOffRunDecision_ARestrictedCallerNeverSpendsTheOperatorKey — a caller
// barred from the operator's provider key gets no model call on any surface,
// and is charged nothing. The restriction is the one the server derives from
// the bearer it authenticated: the test presents a minted token and sets the
// deployment's gate, and builds no context. This is the crossing that failed
// open before: the MCP dispatch stamps an identity and policies but never the
// key bit, and the driver reads an unstamped bit as "allowed".
func TestOffRunDecision_ARestrictedCallerNeverSpendsTheOperatorKey(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{gate: true})
			got := s.decide(e, e.member("alice"), oneQuestion)
			want := wantStatus{"403", codes.PermissionDenied.String(), "isError"}.on(s.name)
			if got.ok || got.code != "operator_key_restricted" || got.status != want {
				t.Errorf("outcome = %+v, want operator_key_restricted with status %s", got, want)
			}
			if n := e.model.calls(); n != 0 {
				t.Errorf("a restricted caller reached the decision model %d times", n)
			}
			if rows := e.offRunRows(); len(rows) != 0 {
				t.Errorf("a refused call was charged: %+v", rows)
			}
		})
	}
}

// TestOffRunDecision_ARestrictedCallerIsServedOnItsOwnKey — the same restricted
// caller, whose tenant has stored its own key for the provider, is answered:
// the call goes out carrying the TENANT's key, and the usage row says the
// tenant paid. Refusing it outright (what the other run-less surfaces do) would
// lock a bring-your-own-key tenant out of a feature it is paying for.
func TestOffRunDecision_ARestrictedCallerIsServedOnItsOwnKey(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{gate: true})
			e.storeTenantKey("acme", tenantKey)
			got := s.decide(e, e.member("alice"), oneQuestion)
			if !got.ok {
				t.Fatalf("outcome = %+v, want an answer", got)
			}
			if paid := e.model.paidWith(t); paid != tenantKey {
				t.Errorf("the call carried a key that is not the tenant's own (operator's: %v)", paid == operatorKey)
			}
			rows := e.offRunRows()
			if len(rows) != 1 || rows[0].CredentialSource != "tenant" || rows[0].TenantID != "acme" {
				t.Errorf("rows = %+v, want one row paid by tenant acme", rows)
			}
		})
	}
}

// TestOffRunDecision_AnotherTenantsKeyIsNotBorrowed — the key is looked up for
// the caller's own tenant: a restricted caller in a tenant with no key is still
// refused when a DIFFERENT tenant has stored one.
func TestOffRunDecision_AnotherTenantsKeyIsNotBorrowed(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{gate: true})
			e.storeTenantKey("globex", tenantKey)
			got := s.decide(e, e.member("alice"), oneQuestion)
			if got.ok || got.code != "operator_key_restricted" || e.model.calls() != 0 {
				t.Errorf("outcome = %+v after %d model calls, want the key refusal and none", got, e.model.calls())
			}
		})
	}
}

// TestOffRunDecision_ATransportsSyntheticAgentHoldsNoKey — a call outside a
// run belongs to no agent. The MCP dispatch stamps a synthetic agent name on
// its context, and a key stored for an agent of that name must not be found on
// one surface and missed on the others: the caller's own keys are its user's
// and its tenant's.
func TestOffRunDecision_ATransportsSyntheticAgentHoldsNoKey(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{gate: true})
			for _, agent := range []string{"mcp-operator", "http-admin", "grpc-admin"} {
				e.storeKey(credential.Identity{TenantID: "acme", Scope: "agent", ScopeID: agent, Name: keyName}, tenantKey)
			}
			bearer := e.member("alice")
			got := s.decide(e, bearer, oneQuestion)
			if got.ok || got.code != "operator_key_restricted" || e.model.calls() != 0 {
				t.Errorf("outcome = %+v after %d model calls, want the key refusal and none", got, e.model.calls())
			}
			// The caller's own user-scoped key is one it does hold.
			e.storeKey(credential.Identity{TenantID: "acme", Scope: "user", ScopeID: "alice", Name: keyName}, tenantKey)
			got = s.decide(e, bearer, oneQuestion)
			if !got.ok || e.model.paidWith(t) != tenantKey {
				t.Errorf("with the user's own key stored: %+v, want an answer on that key", got)
			}
			if rows := e.offRunRows(); len(rows) != 1 || rows[0].CredentialSource != "user" {
				t.Errorf("rows = %+v, want one row paid by the user", rows)
			}
		})
	}
}

// TestOffRunDecision_AnUnrestrictedCallerSpendsTheOperatorKey — the caller
// that holds the operator-key scope, the tenant operator (who holds it by
// implication), and the restricted caller on a deployment whose gate is off
// are all answered on the operator's key.
func TestOffRunDecision_AnUnrestrictedCallerSpendsTheOperatorKey(t *testing.T) {
	cases := []struct {
		name   string
		gate   bool
		bearer func(e *env) string
	}{
		{"holding the operator-key scope", true, func(e *env) string {
			return e.mint("acme", "bob", auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeProvidersOperatorKey)
		}},
		{"a tenant operator", true, func(e *env) string { return e.mint("acme", "ops", auth.ScopeTenant) }},
		{"the gate off", false, func(e *env) string { return e.member("alice") }},
	}
	for _, c := range cases {
		for _, s := range surfaces {
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				e := newEnv(t, envOptions{gate: c.gate})
				got := s.decide(e, c.bearer(e), oneQuestion)
				if !got.ok {
					t.Fatalf("outcome = %+v, want an answer", got)
				}
				if paid := e.model.paidWith(t); paid != operatorKey {
					t.Errorf("the call did not carry the operator's key")
				}
				if rows := e.offRunRows(); len(rows) != 1 || rows[0].CredentialSource != "operator" {
					t.Errorf("rows = %+v, want one row on the operator's key", rows)
				}
			})
		}
	}
}

// TestOffRunDecision_ACallerAtAHardBudgetIsRefusedBeforeAnyCall — the call that
// crosses a hard budget completes and is counted; the next one is refused with
// run admission's refusal before the model is asked, on every surface.
func TestOffRunDecision_ACallerAtAHardBudgetIsRefusedBeforeAnyCall(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			bearer := e.member("alice")
			e.setHardLimit("acme", "alice", 1000) // one call is 1120 tokens

			first := s.decide(e, bearer, oneQuestion)
			if !first.ok || e.model.calls() != 1 {
				t.Fatalf("the crossing call: %+v after %d model calls, want an answer and one call", first, e.model.calls())
			}
			if used := e.srv.LimitsTracker().UsedFor("user", "acme", "alice"); used != 1120 {
				t.Fatalf("alice's budget counter = %d after one call, want its 1120 tokens", used)
			}

			second := s.decide(e, bearer, oneQuestion)
			want := wantStatus{"429", codes.ResourceExhausted.String(), "isError"}.on(s.name)
			if second.ok || second.code != "token_limit_exceeded" || second.status != want {
				t.Errorf("the call over budget: %+v, want token_limit_exceeded with status %s", second, want)
			}
			if n := e.model.calls(); n != 1 {
				t.Errorf("the decision model was called %d times, want only the first", n)
			}
			if rows := e.offRunRows(); len(rows) != 1 {
				t.Errorf("the ledger has %d rows, want only the first call's", len(rows))
			}
			// Another user of the same tenant has their own budget.
			if other := s.decide(e, e.member("carol"), oneQuestion); !other.ok {
				t.Errorf("a different user was refused on alice's budget: %+v", other)
			}
		})
	}
}

// TestOffRunDecision_TheBudgetRefusalNamesTheTrippedScope — the HTTP 429 is the
// body run admission writes: the code, and the caller's own scope with what it
// used and its limit.
func TestOffRunDecision_TheBudgetRefusalNamesTheTrippedScope(t *testing.T) {
	e := newEnv(t, envOptions{})
	bearer := e.member("alice")
	e.setHardLimit("acme", "alice", 1000)
	if o := e.decideHTTP(bearer, oneQuestion); !o.ok {
		t.Fatalf("the crossing call: %+v", o)
	}
	resp, raw := e.do(http.MethodPost, "/v1/_decide", bearer, oneQuestion, nil)
	var body struct {
		Code    string `json:"code"`
		Scope   string `json:"scope"`
		ScopeID string `json:"scope_id"`
		Used    int64  `json:"used"`
		Limit   int64  `json:"limit"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusTooManyRequests || body.Code != "token_limit_exceeded" ||
		body.Scope != "user" || body.ScopeID != "alice" || body.Used != 1120 || body.Limit != 1000 {
		t.Errorf("status %d body %s, want 429 token_limit_exceeded for user alice at 1120 of 1000", resp.StatusCode, raw)
	}
}

// TestOffRunDecision_WritesOneUsageRowForTheCaller — a call outside a run
// leaves exactly one ledger row: both token counts, the provider and the model
// served, the caller's tenant and subject, priced, and no run, session or
// agent. The budget counter moves by the same tokens, and a tracker seeded
// from the ledger after a restart counts them again.
func TestOffRunDecision_WritesOneUsageRowForTheCaller(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{priced: true})
			if o := s.decide(e, e.member("alice"), oneQuestion); !o.ok {
				t.Fatalf("outcome = %+v, want an answer", o)
			}
			rows := e.offRunRows()
			if len(rows) != 1 {
				t.Fatalf("the ledger has %d rows with no run, want 1: %+v", len(rows), rows)
			}
			r := rows[0]
			if r.RunID != "" || r.SessionID != "" || r.AgentID != "" || r.ParentRunID != "" {
				t.Errorf("row names a run, session or agent: %+v", r)
			}
			if r.TenantID != "acme" || r.UserID != "alice" {
				t.Errorf("row is charged to tenant %q user %q, want acme / alice", r.TenantID, r.UserID)
			}
			if r.Provider != "ollama" || r.Model != "nimble" || r.InputTokens != 1116 || r.OutputTokens != 4 || r.CredentialSource != "operator" {
				t.Errorf("row = %+v, want ollama/nimble with 1116 input and 4 output tokens on the operator's key", r)
			}
			// 1116 input at $2 and 4 output at $10 per million tokens.
			if want := (1116*2.0 + 4*10.0) / 1e6; r.CostCurrency != "USD" || r.Cost < want*0.999 || r.Cost > want*1.001 {
				t.Errorf("row cost = %v %q, want %v USD", r.Cost, r.CostCurrency, want)
			}
			if used := e.srv.LimitsTracker().UsedFor("user", "acme", "alice"); used != 1120 {
				t.Errorf("the live budget counter = %d, want 1120", used)
			}
			restarted := limits.New(e.st)
			if err := restarted.Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			if used := restarted.UsedFor("user", "acme", "alice"); used != 1120 {
				t.Errorf("a tracker seeded from the ledger counts %d for alice, want 1120", used)
			}
			if used := restarted.UsedFor("tenant", "acme", "acme"); used != 1120 {
				t.Errorf("a tracker seeded from the ledger counts %d for tenant acme, want 1120", used)
			}
		})
	}
}

type usageRow struct {
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Source    string `json:"credential_source"`
	Input     int64  `json:"input_tokens"`
	Output    int64  `json:"output_tokens"`
	CallCount int64  `json:"call_count"`
}

func (e *env) usage(bearer string) []usageRow {
	e.t.Helper()
	resp, raw := e.do(http.MethodGet, "/v1/_usage?group_by=tenant,user,provider,model,source", bearer, "", nil)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET /v1/_usage = %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Rows []usageRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("usage body: %s", raw)
	}
	return out.Rows
}

// TestOffRunDecision_ShowsInTheUsageReportOfItsTenantOnly — a tenant operator
// sees its tenant's run-less decision calls in GET /v1/_usage, and another
// tenant's operator does not; the admin sees them.
func TestOffRunDecision_ShowsInTheUsageReportOfItsTenantOnly(t *testing.T) {
	e := newEnv(t, envOptions{})
	if o := e.decideHTTP(e.member("alice"), oneQuestion); !o.ok {
		t.Fatalf("outcome = %+v", o)
	}
	mine := e.usage(e.mint("acme", "ops", auth.ScopeTenant))
	if len(mine) != 1 || mine[0].TenantID != "acme" || mine[0].UserID != "alice" || mine[0].Provider != "ollama" ||
		mine[0].Model != "nimble" || mine[0].Input != 1116 || mine[0].Output != 4 || mine[0].CallCount != 1 || mine[0].Source != "operator" {
		t.Errorf("acme's operator sees %+v, want the one call by alice", mine)
	}
	if theirs := e.usage(e.mint("globex", "ops", auth.ScopeTenant)); len(theirs) != 0 {
		t.Errorf("another tenant's operator sees acme's usage: %+v", theirs)
	}
	if all := e.usage(legacyToken); len(all) != 1 || all[0].TenantID != "acme" {
		t.Errorf("the admin sees %+v, want acme's call", all)
	}
}

// TestOffRunDecision_NeedsTheScopeThatCreatesARun — a token that may only read
// runs is turned away by each surface's own gate before anything is asked: the
// HTTP route (which sits under the admin-only /v1/_* catch-all and is opened
// to the run-creation scope, no further), the RPC scope table, and the MCP
// per-tool gate. The model list has the same gate.
func TestOffRunDecision_NeedsTheScopeThatCreatesARun(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			got := s.decide(e, e.mint("acme", "reader", auth.ScopeRunsRead), oneQuestion)
			want := wantStatus{"403", codes.PermissionDenied.String(), "jsonrpc -32001"}.on(s.name)
			if got.ok || got.code != "insufficient_scope" || got.status != want {
				t.Errorf("outcome = %+v, want a scope refusal with status %s", got, want)
			}
			if n := e.model.calls(); n != 0 {
				t.Errorf("a caller without the scope reached the decision model %d times", n)
			}
		})
	}
	t.Run("the model list", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		reader := e.mint("acme", "reader", auth.ScopeRunsRead)
		if resp, raw := e.do(http.MethodGet, "/v1/_decide/models", reader, "", nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET /v1/_decide/models = %d %s, want 403", resp.StatusCode, raw)
		}
		if _, err := e.grpc.ListDecisionModels(e.grpcCtx(reader), &loomcyclepb.ListDecisionModelsRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("ListDecisionModels = %v, want PermissionDenied", err)
		}
		if e.mcpToolListed(reader) {
			t.Error("the MCP session of a read-only token lists the decision tool")
		}
		if !e.mcpToolListed(e.member("alice")) {
			t.Error("the MCP session of a token that may create runs does not list the decision tool")
		}
	})
	t.Run("no bearer", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		if resp, _ := e.do(http.MethodPost, "/v1/_decide", "", oneQuestion, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST /v1/_decide with no bearer = %d, want 401", resp.StatusCode)
		}
		if _, err := e.grpc.Decide(context.Background(), &loomcyclepb.DecideRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Errorf("Decide with no bearer = %v, want Unauthenticated", err)
		}
	})
}

// TestOffRunDecision_WhoIsCharged — the caller is always the authenticated
// principal: a minted admin under its own tenant and subject, the legacy single
// token under its placeholder identity, an isolated user under its own
// subject. With no authentication at all the call is the operator's, filed
// under the shared tenant with no user, and is never key-restricted: there is
// no principal to restrict, as on the other run-less surfaces.
func TestOffRunDecision_WhoIsCharged(t *testing.T) {
	cases := []struct {
		name         string
		opts         envOptions
		bearer       func(e *env) string
		tenant, user string
		// surfaces this caller can reach at all.
		only string
	}{
		{"a minted admin", envOptions{gate: true}, func(e *env) string { return e.mint("ops-co", "root", auth.ScopeAdmin) }, "ops-co", "root", ""},
		{"the legacy token", envOptions{gate: true}, func(*env) string { return legacyToken }, "default", "default", ""},
		{"open mode", envOptions{gate: true, open: true}, func(*env) string { return "" }, "", "", ""},
		// An isolated user's MCP session is for its own credentials only, and
		// gRPC's scope table admits it like HTTP does.
		{"an isolated user", envOptions{}, func(e *env) string { return e.mint("acme", "ivy", auth.ScopeUser) }, "acme", "ivy", "http,grpc"},
	}
	for _, c := range cases {
		for _, s := range surfaces {
			if c.only != "" && !strings.Contains(c.only, s.name) {
				continue
			}
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				e := newEnv(t, c.opts)
				got := s.decide(e, c.bearer(e), oneQuestion)
				if !got.ok {
					t.Fatalf("outcome = %+v, want an answer", got)
				}
				rows := e.offRunRows()
				if len(rows) != 1 || rows[0].TenantID != c.tenant || rows[0].UserID != c.user {
					t.Errorf("rows = %+v, want one row charged to tenant %q user %q", rows, c.tenant, c.user)
				}
			})
		}
	}
}

// TestOffRunDecision_AnIsolatedUserIsKeyRestrictedToo — the restriction is not
// a property of one token shape: an isolated user holds the run-creation scope
// by implication and no operator-key scope, so with the gate on it is refused.
func TestOffRunDecision_AnIsolatedUserIsKeyRestrictedToo(t *testing.T) {
	e := newEnv(t, envOptions{gate: true})
	got := e.decideHTTP(e.mint("acme", "ivy", auth.ScopeUser), oneQuestion)
	if got.ok || got.code != "operator_key_restricted" || e.model.calls() != 0 {
		t.Errorf("outcome = %+v after %d model calls, want the key refusal and none", got, e.model.calls())
	}
}

// A reply that would not survive a decode and re-encode: an integer zero (a
// definite no, which a double would still print as 0 but a Struct would carry
// as 0.0), keys out of order, a field this version does not know, and text an
// HTML-escaping encoder rewrites.
const pickyAnswers = `{"urgent":{"type":"noul","noul":0},` +
	`"team":{"type":"choice","choice":"billing","probabilities":{"billing":0.75,"abuse":0.25},"confidence":0.75,"zeta":1,"alpha":[1,2.50,"x"]},` +
	`"grade":{"type":"score","score":1.7,"legend":{"0":"poor","1":"fair","2":"good"}}}`

const threeQuestions = `{"state":{"ticket":"charged twice","amount":12},"questions":{` +
	`"urgent":{"type":"noul","instructions":"Does it need a reply within the hour?"},` +
	`"team":{"type":"choice","instructions":"Which team takes it?","criteria":{"billing":"payments and refunds","abuse":null}},` +
	`"grade":{"type":"score","instructions":"How complete is the report?","criteria":["poor","fair","good"]}}}`

// TestOffRunDecision_EverySurfaceReturnsTheSameAnswerBytes — the same reply
// from the model reaches a caller as the same bytes whichever surface it used,
// and a `noul` of 0 stays 0.
func TestOffRunDecision_EverySurfaceReturnsTheSameAnswerBytes(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.model.answers = pickyAnswers
	bearer := e.member("alice")
	got := map[string]outcome{}
	for _, s := range surfaces {
		got[s.name] = s.decide(e, bearer, threeQuestions)
		if !got[s.name].ok || len(got[s.name].answers) != 3 {
			t.Fatalf("%s: outcome = %+v, want three answers", s.name, got[s.name])
		}
	}
	for q, want := range got["http"].answers {
		for _, name := range []string{"grpc", "mcp"} {
			if a := got[name].answers[q]; a != want {
				t.Errorf("answer %q over %s = %s, over http = %s", q, name, a, want)
			}
		}
	}
	if a := got["grpc"].answers["urgent"]; a != `{"type":"noul","noul":0}` {
		t.Errorf("the noul answer arrived as %s, want the model's own bytes", a)
	}
	if got["mcp"].text != got["http"].text {
		t.Errorf("the MCP result text and the HTTP body differ:\n mcp: %s\nhttp: %s", got["mcp"].text, got["http"].text)
	}
	if !strings.Contains(got["http"].text, `"model":"decide","provider":"ollama","served_model":"nimble"`) ||
		!strings.Contains(got["http"].text, `"usage":{"input_tokens":1116,"output_tokens":4}`) {
		t.Errorf("the HTTP body is not the tool's output: %s", got["http"].text)
	}
}

// TestOffRunDecision_WithNoDecisionModelsSaysSo — a deployment that declares no
// decision models answers each surface with decision_not_configured, so a
// caller can tell it from a broken one; the MCP tool is still listed.
func TestOffRunDecision_WithNoDecisionModelsSaysSo(t *testing.T) {
	e := newEnv(t, envOptions{unconfigured: true})
	bearer := e.member("alice")
	for _, s := range surfaces {
		got := s.decide(e, bearer, oneQuestion)
		want := wantStatus{"503", codes.FailedPrecondition.String(), "isError"}.on(s.name)
		if got.ok || got.code != "decision_not_configured" || got.status != want {
			t.Errorf("%s: outcome = %+v, want decision_not_configured with status %s", s.name, got, want)
		}
	}
	resp, raw := e.do(http.MethodGet, "/v1/_decide/models", bearer, "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"code":"decision_not_configured"`) {
		t.Errorf("GET /v1/_decide/models = %d %s, want 503 decision_not_configured", resp.StatusCode, raw)
	}
	if _, err := e.grpc.ListDecisionModels(e.grpcCtx(bearer), &loomcyclepb.ListDecisionModelsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ListDecisionModels = %v, want FailedPrecondition", err)
	}
	if !e.mcpToolListed(bearer) {
		t.Error("the decision tool is not listed on a deployment without decision models")
	}
}

// TestOffRunDecision_ListsTheModelsAndNothingSecret — the model list names the
// default and, per model, its name, provider, served model and limits, and
// carries neither the endpoint nor the key's name.
func TestOffRunDecision_ListsTheModelsAndNothingSecret(t *testing.T) {
	e := newEnv(t, envOptions{})
	bearer := e.member("alice")
	resp, raw := e.do(http.MethodGet, "/v1/_decide/models", bearer, "", nil)
	var list struct {
		Default string `json:"default"`
		Models  []struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Limits   struct {
				MaxQuestions int `json:"max_questions"`
				MinOptions   int `json:"min_options"`
				MaxOptions   int `json:"max_options"`
			} `json:"limits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/_decide/models = %d %s", resp.StatusCode, raw)
	}
	if list.Default != "decide" || len(list.Models) != 2 || list.Models[0].Name != "decide" || list.Models[0].Provider != "ollama" ||
		list.Models[0].Model != "nimble" || list.Models[1].Name != "deep" || list.Models[1].Model != "clef" ||
		list.Models[0].Limits.MaxQuestions != 64 || list.Models[0].Limits.MinOptions != 2 || list.Models[0].Limits.MaxOptions != 26 {
		t.Errorf("list = %s", raw)
	}
	for _, secret := range []string{e.model.url, strings.TrimPrefix(e.model.url, "http://"), keyName, operatorKey} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the model list carries the endpoint or the key: %s", raw)
		}
	}
	got, err := e.grpc.ListDecisionModels(e.grpcCtx(bearer), &loomcyclepb.ListDecisionModelsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDefaultModel() != "decide" || len(got.GetModels()) != 2 || got.GetModels()[1].GetName() != "deep" ||
		got.GetModels()[1].GetProvider() != "ollama" || got.GetModels()[1].GetModel() != "clef" ||
		got.GetModels()[0].GetLimits().GetMaxQuestions() != 64 || got.GetModels()[0].GetLimits().GetMaxOptions() != 26 {
		t.Errorf("ListDecisionModels = %v", got)
	}
}

// TestOffRunDecision_AFailureKeepsItsCodeOnEverySurface — each way a call can
// fail reaches the caller as the tool's own code, at the status that kind of
// failure has on the surface, and a failed call is charged nothing unless the
// model reported tokens.
func TestOffRunDecision_AFailureKeepsItsCodeOnEverySurface(t *testing.T) {
	reply := func(status int, body string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	cases := []struct {
		name   string
		call   string
		reply  func(http.ResponseWriter)
		opts   envOptions
		code   string
		status wantStatus
	}{
		{"no state", `{"questions":{"q":{"type":"noul","instructions":"x"}}}`, nil, envOptions{},
			"invalid_input", wantStatus{"400", codes.InvalidArgument.String(), "isError"}},
		{"a model outside the list", `{"model":"gpt","state":{},"questions":{"q":{"type":"noul","instructions":"x"}}}`, nil, envOptions{},
			"model_not_allowed", wantStatus{"400", codes.InvalidArgument.String(), "isError"}},
		{"a question of no known type", `{"state":{},"questions":{"q":{"type":"essay","instructions":"x"}}}`, nil, envOptions{},
			"bad_question", wantStatus{"400", codes.InvalidArgument.String(), "isError"}},
		{"a choice with one option", `{"state":{},"questions":{"q":{"type":"choice","instructions":"x","criteria":{"a":null}}}}`, nil, envOptions{},
			"bad_options", wantStatus{"400", codes.InvalidArgument.String(), "isError"}},
		{"a prompt over the model's context", oneQuestion,
			reply(400, `{"error":"prompt 0 has 12144 tokens; expected 1–8194 (input is never truncated)"}`), envOptions{},
			"prompt_too_large", wantStatus{"413", codes.InvalidArgument.String(), "isError"}},
		{"a model the provider does not serve", oneQuestion, reply(404, `{"error":"model \"nimble\" not found"}`), envOptions{},
			"model_not_found", wantStatus{"502", codes.FailedPrecondition.String(), "isError"}},
		{"a provider fault", oneQuestion, reply(500, `{"error":"boom"}`), envOptions{},
			"call_failed", wantStatus{"502", codes.Unavailable.String(), "isError"}},
		{"a model that does not answer in time", oneQuestion,
			func(http.ResponseWriter) { time.Sleep(300 * time.Millisecond) }, envOptions{timeout: 50 * time.Millisecond},
			"timeout", wantStatus{"504", codes.DeadlineExceeded.String(), "isError"}},
	}
	for _, c := range cases {
		for _, s := range surfaces {
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				e := newEnv(t, c.opts)
				e.model.reply = c.reply
				got := s.decide(e, e.member("alice"), c.call)
				if got.ok || got.code != c.code || got.status != c.status.on(s.name) {
					t.Errorf("outcome = %+v, want %s with status %s", got, c.code, c.status.on(s.name))
				}
				if rows := e.offRunRows(); len(rows) != 0 {
					t.Errorf("a failed call was charged: %+v", rows)
				}
			})
		}
	}
}

// TestOffRunDecision_HTTPRefusesWhatIsNotACall — a body that is not the tool's
// input is the caller's error, and one over the request cap is refused before
// it is read.
func TestOffRunDecision_HTTPRefusesWhatIsNotACall(t *testing.T) {
	e := newEnv(t, envOptions{maxRequestBytes: 512})
	bearer := e.member("alice")
	for _, c := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty", ``, http.StatusBadRequest, "bad_request"},
		{"not JSON", `{`, http.StatusBadRequest, "invalid_input"},
		{"an array", `[1,2]`, http.StatusBadRequest, "invalid_input"},
		{"over the cap", `{"state":{"pad":"` + strings.Repeat("x", 600) + `"},"questions":{}}`, http.StatusRequestEntityTooLarge, "request_too_large"},
	} {
		resp, raw := e.do(http.MethodPost, "/v1/_decide", bearer, c.body, nil)
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if resp.StatusCode != c.status || body.Code != c.code || body.Error == "" {
			t.Errorf("%s: %d %s, want %d with code %s and a message", c.name, resp.StatusCode, raw, c.status, c.code)
		}
	}
	if n := e.model.calls(); n != 0 {
		t.Errorf("the decision model was called %d times for requests that are not calls", n)
	}
}

// TestOffRunDecision_GRPCRefusesJSONThatIsNot — the free-form fields are JSON
// bytes; bytes that are not JSON are the caller's error, said before any call.
func TestOffRunDecision_GRPCRefusesJSONThatIsNot(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := e.grpcCtx(e.member("alice"))
	for name, req := range map[string]*loomcyclepb.DecideRequest{
		"state": {StateJson: []byte(`{"a":`), Questions: map[string]*loomcyclepb.DecisionQuestion{"q": {Type: "noul", Instructions: "x"}}},
		"criteria": {StateJson: []byte(`{}`), Questions: map[string]*loomcyclepb.DecisionQuestion{
			"q": {Type: "choice", Instructions: "x", CriteriaJson: []byte(`{"a":`)}}},
	} {
		// The message names the field at fault, not the encoder that tripped on it.
		if _, err := e.grpc.Decide(ctx, req); status.Code(err) != codes.InvalidArgument ||
			!strings.Contains(status.Convert(err).Message(), name+"_json is not valid JSON") {
			t.Errorf("invalid %s JSON: %v, want InvalidArgument naming %s_json", name, err, name)
		}
	}
	if n := e.model.calls(); n != 0 {
		t.Errorf("the decision model was called %d times", n)
	}
}

// TestOffRunDecision_EveryUsageReaderCountsTheCall — the ledger's other
// readers, each asked about the caller of one run-less decision through its
// own surface: the gRPC usage report, the per-subject view, and the erasure
// report's count of the subject's ledger rows. (The report's SQL, the rollup,
// the budget seed and the timing seed are held to a row with no run on both
// store tiers by the store contract's UsageRowsWithNoRun.)
func TestOffRunDecision_EveryUsageReaderCountsTheCall(t *testing.T) {
	e := newEnv(t, envOptions{priced: true})
	if o := e.decideHTTP(e.member("alice"), oneQuestion); !o.ok {
		t.Fatalf("outcome = %+v", o)
	}
	ops := e.mint("acme", "ops", auth.ScopeTenant)

	rep, err := e.grpc.UsageReport(e.grpcCtx(ops), &loomcyclepb.UsageReportRequest{GroupBy: []string{"tenant", "user", "model"}})
	if err != nil {
		t.Fatal(err)
	}
	if rows := rep.GetRows(); len(rows) != 1 || rows[0].GetTenantId() != "acme" || rows[0].GetUserId() != "alice" ||
		rows[0].GetModel() != "nimble" || rows[0].GetInputTokens() != 1116 || rows[0].GetOutputTokens() != 4 || rows[0].GetCallCount() != 1 {
		t.Errorf("the gRPC usage report = %v, want alice's one call", rows)
	}

	resp, raw := e.do(http.MethodGet, "/v1/_users/alice", ops, "", nil)
	var ins struct {
		Usage struct {
			Calls int64   `json:"calls"`
			Cost  float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &ins); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/_users/alice = %d %s", resp.StatusCode, raw)
	}
	if ins.Usage.Calls != 1 || ins.Usage.Cost <= 0 {
		t.Errorf("the subject view counts %d calls at cost %v, want the 1 priced call", ins.Usage.Calls, ins.Usage.Cost)
	}

	resp, raw = e.do(http.MethodGet, "/v1/_erasure?subject=alice", ops, "", nil)
	var erasure struct {
		Tier2 struct {
			Counts map[string]int64 `json:"counts"`
		} `json:"tier2_uncovered"`
	}
	if err := json.Unmarshal(raw, &erasure); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/_erasure = %d %s", resp.StatusCode, raw)
	}
	if n := erasure.Tier2.Counts["usage_ledger_calls"]; n != 1 {
		t.Errorf("the erasure report counts %d ledger calls for alice, want 1", n)
	}
}
