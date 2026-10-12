package http

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestRunRateLimiter_BucketRefillsAndSaysHowLongToWait(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRunRateLimiter(func() time.Time { return now })
	limit := auth.RateLimit{PerMinute: 60, Burst: 3} // one a second, three at once

	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("acme", "alice", limit); !ok {
			t.Fatalf("start %d of a burst of 3 was refused", i+1)
		}
	}
	ok, wait := l.allow("acme", "alice", limit)
	if ok || wait <= 0 || wait > time.Second {
		t.Errorf("the 4th start: ok=%v wait=%s, want a refusal with a wait of up to one second", ok, wait)
	}
	// Another user, and the same name in another tenant, have their own.
	if ok, _ := l.allow("acme", "bob", limit); !ok {
		t.Error("another user shares the bucket")
	}
	if ok, _ := l.allow("other", "alice", limit); !ok {
		t.Error("the same subject in another tenant shares the bucket")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("acme", "alice", limit); !ok {
		t.Error("one second later, at one a second, a start was still refused")
	}
	if ok, _ := l.allow("acme", "alice", limit); ok {
		t.Error("a second start in the same instant was allowed: the refill was more than one token")
	}
	// An idle bucket fills to its burst and no further.
	now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("acme", "alice", limit); !ok {
			t.Fatalf("after an hour idle, start %d was refused", i+1)
		}
	}
	if ok, _ := l.allow("acme", "alice", limit); ok {
		t.Error("an idle bucket grew past its burst")
	}
}

// Two tokens of one user with different rates share the user's bucket, and
// switching between them must not refill it.
func TestRunRateLimiter_ChangingTheRateDoesNotRefillTheBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRunRateLimiter(func() time.Time { return now })
	small := auth.RateLimit{PerMinute: 1, Burst: 2}
	large := auth.RateLimit{PerMinute: 1, Burst: 50}
	for i := 0; i < 2; i++ {
		l.allow("acme", "alice", small)
	}
	if ok, _ := l.allow("acme", "alice", large); ok {
		t.Error("presenting a token with a larger burst refilled an empty bucket")
	}
	if ok, _ := l.allow("acme", "alice", small); ok {
		t.Error("switching back was allowed")
	}
	// A smaller burst caps what is there.
	l2 := newRunRateLimiter(func() time.Time { return now })
	l2.allow("acme", "carol", large) // 49 left
	l2.allow("acme", "carol", small) // capped to 2, one taken
	l2.allow("acme", "carol", small) // the other
	if ok, _ := l2.allow("acme", "carol", large); ok {
		t.Error("a bucket capped by a smaller burst kept its larger balance")
	}
}

func TestRunRateFor_TheDefaultDoesNotBindAnOperator(t *testing.T) {
	s := &Server{defaultRunRate: auth.RateLimit{PerMinute: 6}}
	own := &auth.RateLimit{PerMinute: 2, Burst: 2}
	for name, c := range map[string]struct {
		p       auth.Principal
		want    int
		limited bool
	}{
		"isolated user":          {auth.Principal{Scopes: []string{auth.ScopeUser}}, 6, true},
		"member":                 {auth.Principal{Scopes: []string{auth.ScopeRunsCreate}}, 6, true},
		"tenant operator":        {auth.Principal{Scopes: []string{auth.ScopeTenant}}, 0, false},
		"admin":                  {auth.Principal{Scopes: []string{auth.ScopeAdmin}}, 0, false},
		"legacy token":           {auth.Principal{Legacy: true, Scopes: []string{auth.ScopeAdmin}}, 0, false},
		"its own rate wins":      {auth.Principal{Scopes: []string{auth.ScopeUser}, Restrictions: auth.Restrictions{Rate: own}}, 2, true},
		"an operator's own rate": {auth.Principal{Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Rate: own}}, 2, true},
	} {
		got, limited := s.runRateFor(c.p)
		if limited != c.limited || (limited && got.PerMinute != c.want) {
			t.Errorf("%s: rate=%+v limited=%v, want %d/min limited=%v", name, got, limited, c.want, c.limited)
		}
	}
	none := &Server{}
	if _, limited := none.runRateFor(auth.Principal{Scopes: []string{auth.ScopeUser}}); limited {
		t.Error("with no default and no rate on the token, a caller was rate limited")
	}
}

// A call from inside a run, and a run with no principal, are not counted: an
// agent's own tool use is not its starter's rate, and a definition's own run
// has no credential to bound.
func TestAdmitPrincipalRate_CountsOnlyACallersOwnStarts(t *testing.T) {
	s := &Server{runRate: newRunRateLimiter(nil)}
	one := auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeUser},
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 1}}}
	ctx := auth.WithPrincipal(context.Background(), one)
	if err := s.admitPrincipalRate(ctx); err != nil {
		t.Fatalf("the first start: %v", err)
	}
	err := s.admitPrincipalRate(ctx)
	var rl *rateLimitedError
	if err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("the second start: err = %v, want a rate refusal", err)
	}
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Errorf("the refusal carries no wait: %v", err)
	}
	if !errors.Is(err, runner.ErrRateLimited) {
		t.Error("the refusal is not runner.ErrRateLimited to errors.Is")
	}
	inRun := tools.WithRunID(ctx, "r_parent")
	if err := s.admitPrincipalRate(inRun); err != nil {
		t.Errorf("a call from inside a run was counted: %v", err)
	}
	if err := s.admitPrincipalRate(context.Background()); err != nil {
		t.Errorf("a call with no principal was counted: %v", err)
	}
}

const rateBearer = "tok-rated" // per_minute 1, burst 2

// rateFixture is restrictedFixture plus a member token whose rate is two
// starts and then one a minute.
func rateFixture(t *testing.T) (*Server, func(method, path, body string) (int, string, http.Header)) {
	t.Helper()
	srv, ts, st := restrictedFixtureServer(t)
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_rated", Name: "u-rated", TenantID: "acme", Subject: "rated",
		TokenHash:        auth.HashToken(testTokenPepper, rateBearer),
		AllowedScopes:    []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		RestrictionsJSON: `{"rate":{"per_minute":1,"burst":2}}`,
	}); err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, string, http.Header) {
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rateBearer)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		return resp.StatusCode, sb.String(), resp.Header
	}
	return srv, do
}

// A token with a rate starts its burst and is then refused with 429, a
// Retry-After header, and no row written for the refused start.
func TestRateLimitedToken_IsRefusedPastItsBurstAndCostsNothing(t *testing.T) {
	srv, do := rateFixture(t)
	for i := 0; i < 2; i++ {
		if code, body, _ := do("POST", "/v1/runs", runBody("other")); code != 200 {
			t.Fatalf("start %d of a burst of 2: status %d: %s", i+1, code, body)
		}
	}
	sessions := func() int {
		rows, _, err := srv.store.ListSessions(context.Background(), store.SessionFilter{TenantID: "acme", UserID: "rated"}, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	before := sessions()
	// Refused whatever the body says: the check is made before it is read.
	for _, body := range []string{runBody("other"), `{"agent":"no-such-agent"`, ``} {
		code, out, hdr := do("POST", "/v1/runs", body)
		if code != http.StatusTooManyRequests || !strings.Contains(out, "rate_limited") {
			t.Errorf("a start past the burst (body %q): status %d, want 429 rate_limited: %s", body, code, out)
		}
		if ra := hdr.Get("Retry-After"); ra == "" || ra == "0" {
			t.Errorf("Retry-After = %q, want a positive number of seconds", ra)
		}
	}
	if got := sessions(); got != before {
		t.Errorf("refused starts opened %d session(s)", got-before)
	}
	// Reads are not starts.
	if code, body, _ := do("GET", "/v1/users/rated/agents", ""); code != 200 {
		t.Errorf("a read past the burst: status %d: %s", code, body)
	}
}

// Every other way to ask for a run is counted against the same bucket.
func TestRateLimitedToken_EveryIntakeCountsAgainstTheSameBucket(t *testing.T) {
	for name, start := range map[string]func(t *testing.T, srv *Server, do func(string, string, string) (int, string, http.Header)) (int, string){
		"a continuation": func(t *testing.T, srv *Server, do func(string, string, string) (int, string, http.Header)) (int, string) {
			sess, err := srv.store.CreateSession(context.Background(), "acme", "other", "rated")
			if err != nil {
				t.Fatal(err)
			}
			code, body, _ := do("POST", "/v1/sessions/"+sess.ID+"/messages", `{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`)
			return code, body
		},
		"a draft": func(t *testing.T, srv *Server, do func(string, string, string) (int, string, http.Header)) (int, string) {
			code, body, _ := do("POST", "/v1/runs", `{"agent":"other","start":false,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)
			return code, body
		},
		"a replay": func(t *testing.T, srv *Server, do func(string, string, string) (int, string, http.Header)) (int, string) {
			sess, err := srv.store.CreateSession(context.Background(), "acme", "other", "rated")
			if err != nil {
				t.Fatal(err)
			}
			code, body, _ := do("POST", "/v1/sessions/"+sess.ID+"/replay", `{"agent":"other"}`)
			return code, body
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, do := rateFixture(t)
			for i := 0; i < 2; i++ {
				if code, body, _ := do("POST", "/v1/runs", runBody("other")); code != 200 {
					t.Fatalf("burst start %d: status %d: %s", i+1, code, body)
				}
			}
			if code, body := start(t, srv, do); code != http.StatusTooManyRequests || !strings.Contains(body, "rate_limited") {
				t.Errorf("status %d, want 429 rate_limited: %s", code, body)
			}
		})
	}
}

// Each child of a batch is one start: a batch larger than what is left of the
// bucket has its later children refused, and the first ones run.
func TestRateLimitedToken_EachBatchChildIsOneStart(t *testing.T) {
	_, do := rateFixture(t)
	batch := `{"spawns":[` + runBody("other") + `,` + runBody("other") + `,` + runBody("other") + `]}`
	code, body, _ := do("POST", "/v1/runs:batch", batch)
	if code != 200 {
		t.Fatalf("batch: status %d: %s", code, body)
	}
	var res struct {
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || len(res.Results) != 3 {
		t.Fatalf("batch answer: %s (err %v)", body, err)
	}
	refused := 0
	for _, r := range res.Results {
		if strings.Contains(r.Error, "rate_limited") {
			refused++
		} else if r.Error != "" {
			t.Errorf("a child failed for another reason: %s", r.Error)
		}
	}
	if refused != 1 {
		t.Errorf("%d of 3 children refused with a burst of 2, want exactly 1: %s", refused, body)
	}
}

// rateCountedIntakes names every function that must take a run start from the
// caller's rate, and the functions that create a run row without doing so,
// each with the reason. A function that creates a run row and is in neither
// list fails the test.
var rateCountedIntakes = map[string]string{
	"handleRuns":          "count",
	"handleMessages":      "count",
	"RunOnce":             "count",
	"ReplaySession":       "count",
	"CreateConfiguredRun": "count",
	"Decision":            "count",
	"admitTeamRun":        "count",
}

func TestRateLimit_EveryIntakeTakesFromTheCallersRate(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if _, listed := rateCountedIntakes[fn.Name.Name]; !listed {
				continue
			}
			calls := 0
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "admitPrincipalRate" {
						calls++
					}
				}
				return true
			})
			found[fn.Name.Name] = true
			if calls != 1 {
				t.Errorf("%s (%s) calls admitPrincipalRate %d time(s), want exactly once: zero leaves the intake unbounded, twice charges one request two starts", fn.Name.Name, name, calls)
			}
		}
	}
	for fn := range rateCountedIntakes {
		if !found[fn] {
			t.Errorf("rateCountedIntakes lists %s, which no longer exists", fn)
		}
	}
	// Every function that creates a top-level run row is reached through one
	// of the counted intakes. runRowCreators (the agent-allowlist test) is the
	// reviewed list of those functions; each "admit" entry must be counted
	// here, directly or by the intake that calls it.
	reachedBy := map[string]string{
		"handleRuns": "handleRuns", "handleMessages": "handleMessages", "RunOnce": "RunOnce",
		"ReplaySession":           "ReplaySession",
		"createConfiguredRunCore": "CreateConfiguredRun (gRPC, MCP) or handleRuns (HTTP)",
	}
	for fn, how := range runRowCreators {
		if how != "admit" {
			continue
		}
		if _, ok := reachedBy[fn]; !ok {
			t.Errorf("%s creates a top-level run row and no counted intake is named for it: add it to rateCountedIntakes, or say here which counted intake reaches it", fn)
		}
	}
}
