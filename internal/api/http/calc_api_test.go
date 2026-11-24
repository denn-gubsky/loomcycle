package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// calcAPIHandler is POST /v1/_calc behind the real auth middleware, on a
// server holding only the Calc tool, with these tokens of tenant acme: a
// tenant operator, a member, an isolated user, a token limited to one agent,
// and a member token rated at one call.
func calcAPIHandler(t *testing.T) http.Handler {
	t.Helper()
	s, _ := tokenAuthServer(t, "legacy-calc-token")
	s.tools = []tools.Tool{&builtin.Calc{}}
	s.runRate = newRunRateLimiter(nil)
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}
	for _, tok := range []struct {
		bearer, subject string
		scopes          []string
		restrictions    string
	}{
		{"lct_calc_op", "op", []string{auth.ScopeTenant}, ""},
		{"lct_calc_member", "bob", member, ""},
		{"lct_calc_user", "carol", []string{auth.ScopeUser}, ""},
		{"lct_calc_agents", "dev", member, `{"agents":["score"]}`},
		{"lct_calc_rated", "rita", member, `{"rate":{"per_minute":1,"burst":1}}`},
	} {
		if _, err := s.store.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: "def_" + tok.subject, Name: tok.subject, TenantID: "acme", Subject: tok.subject,
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
			RestrictionsJSON: tok.restrictions,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s.authMiddleware(http.HandlerFunc(s.handleCalc))
}

// postCalc sends one call and returns the recorder and the decoded body.
func postCalc(t *testing.T, h http.Handler, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/_calc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// A tenant operator, a member and an isolated user each get the evaluator's
// exact answer; a token limited to certain agents is held to the run plane
// and refused before the tool runs.
func TestCalcAPI_EveryTokenButARunPlaneOneGetsTheEvaluatorsAnswer(t *testing.T) {
	h := calcAPIHandler(t)
	for _, tok := range []string{"lct_calc_op", "lct_calc_member", "lct_calc_user"} {
		rec, _ := postCalc(t, h, tok, `{"expr":"0.1 + 0.2"}`)
		var out struct {
			Result json.Number `json:"result"`
			Exact  bool        `json:"exact"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Result != "0.3" || !out.Exact {
			t.Errorf("%s: %d %s, want 200 with 0.3 exact", tok, rec.Code, rec.Body)
		}
	}
	if rec, _ := postCalc(t, h, "lct_calc_agents", `{"expr":"0.1 + 0.2"}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "restricted_token") {
		t.Errorf("a run-plane token: %d %s, want 403 restricted_token", rec.Code, rec.Body)
	}
}

// A refusal carries the tool's error code, its detail, hint and position and
// its category, with the status its category names: a validation refusal is
// 400 (fix the call), a business one 422 (the mathematics refused it).
func TestCalcAPI_ARefusalCarriesItsCodeHintAndCategoryWithItsStatus(t *testing.T) {
	h := calcAPIHandler(t)
	rec, out := postCalc(t, h, "lct_calc_member", `{"expr":"2**3"}`)
	if rec.Code != http.StatusBadRequest || out["code"] != "syntax" || out["errorCategory"] != "validation" {
		t.Fatalf("2**3: %d %s, want 400 syntax validation", rec.Code, rec.Body)
	}
	if hint, _ := out["hint"].(string); !strings.Contains(hint, "power is `^`") {
		t.Errorf("hint = %q, want the power hint", hint)
	}
	if _, ok := out["pos"].(float64); !ok || out["detail"] == "" || out["error"] != out["detail"] {
		t.Errorf("body %s: want pos, and detail as error", rec.Body)
	}

	rec, out = postCalc(t, h, "lct_calc_member", `{"expr":"1 / 0"}`)
	if rec.Code != http.StatusUnprocessableEntity || out["code"] != "division_by_zero" || out["errorCategory"] != "business" {
		t.Errorf("1 / 0: %d %s, want 422 division_by_zero business", rec.Code, rec.Body)
	}

	if rec, out := postCalc(t, h, "lct_calc_member", ``); rec.Code != http.StatusBadRequest || out["code"] != "bad_request" {
		t.Errorf("empty body: %d %s, want 400 bad_request", rec.Code, rec.Body)
	}
}

// `from` and `to` need a run: refused 400 with the reason, whatever else the
// call holds.
func TestCalcAPI_DataByReferenceIsRefused(t *testing.T) {
	h := calcAPIHandler(t)
	for _, body := range []string{`{"expr":"sum(t)","from":{"t":"volume:a.csv"}}`, `{"expr":"2","to":"memory:user/k"}`} {
		rec, out := postCalc(t, h, "lct_calc_op", body)
		if rec.Code != http.StatusBadRequest || out["code"] != "bad_request" || !strings.Contains(rec.Body.String(), "data by reference needs a run") {
			t.Errorf("%s: %d %s, want 400 data by reference needs a run", body, rec.Code, rec.Body)
		}
	}
}

// Each call takes one unit of the token's run rate: past it the call is 429
// with Retry-After, and never reaches the tool.
func TestCalcAPI_PastTheTokensRunRateIs429WithRetryAfter(t *testing.T) {
	h := calcAPIHandler(t)
	if rec, _ := postCalc(t, h, "lct_calc_rated", `{"expr":"1 + 1"}`); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", rec.Code, rec.Body)
	}
	rec, out := postCalc(t, h, "lct_calc_rated", `{"expr":"1 + 1"}`)
	if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("second call: %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs <= 0 || time.Duration(secs)*time.Second > time.Minute {
		t.Errorf("Retry-After = %q, want whole seconds in (0, 60]", rec.Header().Get("Retry-After"))
	}
}

// A refusal with no hint of its own names the help as its next step. In a run
// that is a call to Context, which a caller of this route does not have, so
// over HTTP it names the help topic and the guide instead. The in-run tool's
// own text is unchanged, which also pins calcRefusalHelpInRun to its wording.
func TestCalcAPI_ARefusalPointsAtHelpACallerCanRead(t *testing.T) {
	in := json.RawMessage(`{"expr":"1/0"}`)
	r, _ := (&builtin.Calc{}).Execute(context.Background(), in)
	if r.Error == nil || !strings.Contains(r.Error.Description, calcRefusalHelpInRun) {
		t.Fatalf("the in-run refusal of 1/0 no longer names %q (got %+v); update calcRefusalHelpInRun", calcRefusalHelpInRun, r.Error)
	}

	h := calcAPIHandler(t)
	rec, out := postCalc(t, h, "lct_calc_member", string(in))
	if rec.Code != http.StatusUnprocessableEntity || out["code"] != "division_by_zero" {
		t.Fatalf("1/0: %d %s, want 422 division_by_zero", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "Context op=help") {
		t.Errorf("body %s points at the in-run Context tool", rec.Body)
	}
	if desc, _ := out["description"].(string); !strings.Contains(desc, "help topic Calc/errors") || !strings.Contains(desc, "docs/CALC.md#when-a-call-is-refused") {
		t.Errorf("description = %q, want the help topic and the guide", desc)
	}
}
