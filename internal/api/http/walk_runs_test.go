package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// GET /v1/runs?walk_id= lists one team walk's runs. Before it, a canvas could
// only rebuild a walk from its user's run list — capped at 100 and filtered
// client-side on parent_context.

type walkRunsPage struct {
	Agents     []agentResponse `json:"agents"`
	NextCursor string          `json:"next_cursor"`
}

// listWalkRuns issues GET /v1/runs through the mux as the principal on ctx
// (nil = open mode) and decodes a 200 body.
func listWalkRuns(t *testing.T, mux http.Handler, ctx context.Context, query url.Values) (int, walkRunsPage, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs?"+query.Encode(), nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body walkRunsPage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body)
		}
	}
	return rec.Code, body, rec.Body.String()
}

// addWalkMembers writes n member runs of walkID under (tenant, user), the lineage
// a walk stamps on each run it spawns.
func addWalkMembers(t *testing.T, st store.Store, tenant, user, walkID string, n int) map[string]bool {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "writer", user)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
			AgentID: fmt.Sprintf("m_%d", i), UserID: user, TenantID: tenant,
			ParentContext: &store.ParentContext{WalkID: walkID, State: "a", StateVisit: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[r.ID] = true
	}
	return ids
}

func TestListRuns_PagesEveryWalkMemberOnceWithTheWalkItself(t *testing.T) {
	srv, mux := runReadServer(t)
	walkID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish("", nil)
	want := addWalkMembers(t, srv.store, "acme", "alice", walkID, 250)
	want[walkID] = true
	op := principalCtx("acme", "op", auth.ScopeTenant)

	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		q := url.Values{"walk_id": {walkID}, "limit": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		code, page, body := listWalkRuns(t, mux, op, q)
		if code != http.StatusOK {
			t.Fatalf("page %d = %d: %s", pages, code, body)
		}
		pages++
		for _, a := range page.Agents {
			seen[a.RunID]++
			if a.RunID == walkID && !a.Live {
				t.Error("the walk in flight is listed as not live")
			}
		}
		if page.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("the listing never ended")
		}
		cursor = page.NextCursor
	}
	if pages != 3 {
		t.Errorf("251 runs at 100 per page took %d pages, want 3", pages)
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("run %s listed %d times, want once", id, seen[id])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("listed %d distinct runs, want %d", len(seen), len(want))
	}
}

// A running member's row says what it is blocked on, like the user listing.
func TestListRuns_RunningMemberCarriesItsAwaitedState(t *testing.T) {
	srv, mux := runReadServer(t)
	walkID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish("", nil)
	var member string
	for id := range addWalkMembers(t, srv.store, "acme", "alice", walkID, 1) {
		member = id
	}
	if err := srv.store.AppendEvent(context.Background(), member, "tool_call",
		[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`)); err != nil {
		t.Fatal(err)
	}
	_, page, _ := listWalkRuns(t, mux, nil, url.Values{"walk_id": {walkID}})
	for _, a := range page.Agents {
		if a.RunID == member {
			if a.AwaitedState != "channel" || a.AwaitedOn != "findings" {
				t.Errorf("member awaited = (%q, %q), want (channel, findings)", a.AwaitedState, a.AwaitedOn)
			}
			return
		}
	}
	t.Fatalf("member %s not listed", member)
}

func TestListRuns_AnotherTenantGetsTheOpaque404(t *testing.T) {
	srv, mux := runReadServer(t)
	walkID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish("", nil)
	addWalkMembers(t, srv.store, "acme", "alice", walkID, 2)
	other := principalCtx("other", "op", auth.ScopeTenant)

	missing, _, missingBody := listWalkRuns(t, mux, other, url.Values{"walk_id": {"r_does_not_exist"}})
	code, _, body := listWalkRuns(t, mux, other, url.Values{"walk_id": {walkID}})
	if code != http.StatusNotFound || code != missing {
		t.Errorf("another tenant listing the walk = %d, want 404 — the answer an unknown walk gets (%d)", code, missing)
	}
	var a, b struct{ Code string }
	_ = json.Unmarshal([]byte(body), &a)
	_ = json.Unmarshal([]byte(missingBody), &b)
	if a.Code != "unknown_walk_id" || a.Code != b.Code {
		t.Errorf("404 codes (%q, %q), want both unknown_walk_id", a.Code, b.Code)
	}
	if code, page, _ := listWalkRuns(t, mux, principalCtx("other", "root", auth.ScopeAdmin), url.Values{"walk_id": {walkID}}); code != http.StatusOK || len(page.Agents) != 3 {
		t.Errorf("admin across tenants = %d with %d runs, want 200 with 3", code, len(page.Agents))
	}
}

// An isolated member (substrate:user alone) lists a walk only if it owns the
// walk's run; another user's walk is the opaque 404.
func TestListRuns_IsolatedMemberListsOnlyAWalkItOwns(t *testing.T) {
	srv, mux := runReadServer(t)
	walkID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish("", nil)
	addWalkMembers(t, srv.store, "acme", "alice", walkID, 2)

	if code, _, _ := listWalkRuns(t, mux, principalCtx("acme", "bob", auth.ScopeUser), url.Values{"walk_id": {walkID}}); code != http.StatusNotFound {
		t.Errorf("isolated member listing another user's walk = %d, want 404", code)
	}
	code, page, _ := listWalkRuns(t, mux, principalCtx("acme", "alice", auth.ScopeUser), url.Values{"walk_id": {walkID}})
	if code != http.StatusOK || len(page.Agents) != 3 {
		t.Errorf("isolated member listing its own walk = %d with %d runs, want 200 with 3", code, len(page.Agents))
	}
}

func TestListRuns_RefusesAMissingOrMalformedArgument(t *testing.T) {
	_, mux := runReadServer(t)
	for _, tc := range []struct {
		name string
		q    url.Values
		want string
	}{
		{"no walk_id", url.Values{}, "walk_id_required"},
		{"malformed walk_id", url.Values{"walk_id": {"team:triage"}}, "invalid_walk_id"},
		{"zero limit", url.Values{"walk_id": {"r_1"}, "limit": {"0"}}, "invalid_limit"},
		{"limit over the cap", url.Values{"walk_id": {"r_1"}, "limit": {"1001"}}, "invalid_limit"},
		{"foreign cursor", url.Values{"walk_id": {"r_1"}, "cursor": {"cur_0"}}, "invalid_cursor"},
	} {
		code, _, body := listWalkRuns(t, mux, nil, tc.q)
		var got struct{ Code string }
		_ = json.Unmarshal([]byte(body), &got)
		if code != http.StatusBadRequest || got.Code != tc.want {
			t.Errorf("%s: %d %s, want 400 %s", tc.name, code, body, tc.want)
		}
	}
}

// A walk with no starter state: its agent member is listed with the state it
// ran in. Every member is stamped with its walk since walks stopped stamping
// only a Starter's wave.
func TestListRuns_WalkWithoutAStarterListsItsAgentMembers(t *testing.T) {
	h := newStampHarness(t)
	seedTenantTeam(t, h.st, "acme", "solo", agentOnlyTeam)
	walkID := h.postTeamDef(alicePrincipal, `{"op":"run","name":"solo","input":"go"}`)

	code, page, body := listWalkRuns(t, h.srv.Mux(), alicePrincipal(context.Background()), url.Values{"walk_id": {walkID}})
	if code != http.StatusOK {
		t.Fatalf("GET /v1/runs = %d: %s", code, body)
	}
	var walk, member bool
	for _, a := range page.Agents {
		switch {
		case a.RunID == walkID:
			walk = true
		case a.ParentContext != nil && a.ParentContext.State == "a" && a.ParentContext.WalkID == walkID:
			member = true
		default:
			t.Errorf("listed %s (agent %s), which is not this walk's", a.RunID, a.Agent)
		}
	}
	if !walk || !member {
		t.Errorf("listing %s: walk run listed %v, agent member listed %v; want both", body, walk, member)
	}
}

func TestRequiredScopeFor_ListRunsIsARunsRead(t *testing.T) {
	if got := requiredScopeFor(http.MethodGet, "/v1/runs"); got != auth.ScopeRunsRead {
		t.Errorf("GET /v1/runs scope = %q, want %q", got, auth.ScopeRunsRead)
	}
	if got := requiredScopeFor(http.MethodPost, "/v1/runs"); got != auth.ScopeRunsCreate {
		t.Errorf("POST /v1/runs scope = %q, want %q", got, auth.ScopeRunsCreate)
	}
}
