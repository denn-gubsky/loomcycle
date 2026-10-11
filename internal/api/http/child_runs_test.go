package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// GET /v1/runs?parent_run_id= lists one run's direct children. Before it, the
// only way to find the runs an agent spawned was the user's own listing: at
// most 100 runs, only running ones unless asked, and filtered client-side.

// addRun writes one run of (tenant, user) under parent ("" = top-level).
func addRun(t *testing.T, st store.Store, tenant, user, agent, parent string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "writer", user)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agent, ParentRunID: parent, UserID: user, TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestListRuns_PagesEveryChildOfARunOnceWhateverItsStatus(t *testing.T) {
	srv, mux := runReadServer(t)
	parent := addRun(t, srv.store, "acme", "alice", "lead", "")
	want := map[string]bool{}
	var first store.Run
	for i := 0; i < 250; i++ {
		r := addRun(t, srv.store, "acme", "alice", fmt.Sprintf("c_%d", i), parent.ID)
		want[r.ID] = true
		if i == 0 {
			first = r
		}
	}
	// An ended child, a grandchild and another run's child.
	if err := srv.store.FinishRun(context.Background(), first.ID, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	addRun(t, srv.store, "acme", "alice", "grandchild", first.ID)
	addRun(t, srv.store, "acme", "alice", "elsewhere", addRun(t, srv.store, "acme", "alice", "other", "").ID)
	op := principalCtx("acme", "op", auth.ScopeTenant)

	seen := map[string]int{}
	completed, cursor, pages := 0, "", 0
	for {
		q := url.Values{"parent_run_id": {parent.ID}, "limit": {"100"}}
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
			if a.ParentRunID != parent.ID {
				t.Errorf("listed %s, whose parent run is %q", a.RunID, a.ParentRunID)
			}
			if a.Status == "completed" {
				completed++
			}
		}
		if page.NextCursor == "" || pages > 10 {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 3 || len(seen) != len(want) || completed != 1 {
		t.Errorf("%d pages, %d distinct runs, %d completed; want 3 pages, %d children, 1 completed", pages, len(seen), completed, len(want))
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("child %s listed %d times, want once", id, seen[id])
		}
	}
	if seen[parent.ID] != 0 {
		t.Error("the parent run is listed among its own children")
	}
}

// A run the caller cannot read lists nothing and says nothing: the answer is
// the one an unknown run gets.
func TestListRuns_AParentTheCallerCannotReadIsTheOpaque404(t *testing.T) {
	srv, mux := runReadServer(t)
	parent := addRun(t, srv.store, "acme", "alice", "lead", "")
	addRun(t, srv.store, "acme", "alice", "c", parent.ID)
	q := url.Values{"parent_run_id": {parent.ID}}

	missing, _, missingBody := listWalkRuns(t, mux, principalCtx("other", "op", auth.ScopeTenant), url.Values{"parent_run_id": {"r_does_not_exist"}})
	for name, who := range map[string]context.Context{
		"another tenant":                        principalCtx("other", "op", auth.ScopeTenant),
		"an isolated member of the same tenant": principalCtx("acme", "bob", auth.ScopeUser),
	} {
		code, _, body := listWalkRuns(t, mux, who, q)
		var a, b struct{ Code string }
		_ = json.Unmarshal([]byte(body), &a)
		_ = json.Unmarshal([]byte(missingBody), &b)
		if code != http.StatusNotFound || code != missing || a.Code != "unknown_parent_run_id" || a.Code != b.Code {
			t.Errorf("%s: %d %s, want the 404 unknown_parent_run_id an unknown run gets (%d %s)", name, code, body, missing, missingBody)
		}
	}
	for name, who := range map[string]context.Context{
		"the owner, isolated": principalCtx("acme", "alice", auth.ScopeUser),
		"the tenant operator": principalCtx("acme", "op", auth.ScopeTenant),
		"an admin elsewhere":  principalCtx("other", "root", auth.ScopeAdmin),
	} {
		if code, page, body := listWalkRuns(t, mux, who, q); code != http.StatusOK || len(page.Agents) != 1 {
			t.Errorf("%s: %d with %d runs, want 200 with the one child: %s", name, code, len(page.Agents), body)
		}
	}
}

func TestListRuns_ByParentRefusesAMalformedArgument(t *testing.T) {
	_, mux := runReadServer(t)
	for _, tc := range []struct {
		name string
		q    url.Values
		want string
	}{
		{"both filters", url.Values{"walk_id": {"r_1"}, "parent_run_id": {"r_2"}}, "one_filter_only"},
		{"malformed parent_run_id", url.Values{"parent_run_id": {"team:triage"}}, "invalid_parent_run_id"},
		{"limit over the cap", url.Values{"parent_run_id": {"r_1"}, "limit": {"1001"}}, "invalid_limit"},
		{"foreign cursor", url.Values{"parent_run_id": {"r_1"}, "cursor": {"cur_0"}}, "invalid_cursor"},
	} {
		code, _, body := listWalkRuns(t, mux, nil, tc.q)
		var got struct{ Code string }
		_ = json.Unmarshal([]byte(body), &got)
		if code != http.StatusBadRequest || got.Code != tc.want {
			t.Errorf("%s: %d %s, want 400 %s", tc.name, code, body, tc.want)
		}
	}
}

// The connector's listing (MCP list_runs) goes through the same gate and
// carries a running child's awaited state.
func TestConnectorListChildRuns_SameGateAndAwaitedState(t *testing.T) {
	srv, _ := runReadServer(t)
	parent := addRun(t, srv.store, "acme", "alice", "lead", "")
	child := addRun(t, srv.store, "acme", "alice", "c", parent.ID)
	if err := srv.store.AppendEvent(context.Background(), child.ID, "tool_call",
		[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := srv.ListChildRuns(principalCtx("acme", "op", auth.ScopeTenant), parent.ID, 0, "")
	if err != nil || len(got.Runs) != 1 || got.Runs[0].RunID != child.ID || got.Runs[0].AwaitedState != "channel" || got.Runs[0].AwaitedOn != "findings" {
		t.Errorf("ListChildRuns = %+v, %v; want the one child awaiting channel findings", got, err)
	}
	if _, err := srv.ListChildRuns(principalCtx("other", "op", auth.ScopeTenant), parent.ID, 0, ""); err == nil {
		t.Error("another tenant listed the run's children")
	}
}

// The run-state stream's parent_run_id filter passes one run's direct
// children and nothing else: not the run itself, not a grandchild, not a
// top-level run.
func TestStreamUserRunStates_ParentRunFilterPassesOnlyDirectChildren(t *testing.T) {
	srv, _ := runReadServer(t)
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan connector.RunStateEvent, 16)
	go func() {
		_ = srv.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{UserID: "alice", ParentRunID: "r_parent"},
			func(evt connector.RunStateEvent) error {
				events <- evt
				return nil
			})
	}()
	waitForSubscriber(t, bus)

	for _, m := range []runStateMeta{
		{RunID: "r_parent", AgentID: "a1", Agent: "lead", UserID: "alice"},
		{RunID: "r_other", AgentID: "a2", Agent: "other", UserID: "alice", ParentRunID: "r_elsewhere"},
		{RunID: "r_grand", AgentID: "a3", Agent: "deep", UserID: "alice", ParentRunID: "r_child"},
		{RunID: "r_child", AgentID: "a4", Agent: "helper", UserID: "alice", ParentRunID: "r_parent"},
	} {
		srv.publishRunState(m, "running", "", "")
	}
	select {
	case evt := <-events:
		if evt.RunID != "r_child" || evt.ParentRunID != "r_parent" {
			t.Errorf("the filter passed %s (parent %q), want only r_child", evt.RunID, evt.ParentRunID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the filtered stream never carried the child")
	}
	select {
	case evt := <-events:
		t.Errorf("the filter also passed %s (parent %q)", evt.RunID, evt.ParentRunID)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestParseStreamFilter_ParentRunID(t *testing.T) {
	got := parseStreamFilter("u1", map[string][]string{"parent_run_id": {" r_abc "}})
	if got.ParentRunID != "r_abc" {
		t.Errorf("ParentRunID = %q, want r_abc (trimmed)", got.ParentRunID)
	}
	if got := parseStreamFilter("u1", map[string][]string{}); got.ParentRunID != "" {
		t.Errorf("ParentRunID = %q with no param, want empty", got.ParentRunID)
	}
}

// The open frame echoes the parent-run filter the server applied, so a client
// can tell a filter that matched nothing from one the server never read.
func TestStreamUserAgents_OpenFrameEchoesTheParentRunFilter(t *testing.T) {
	srv, _, cleanup := streamFixture(t)
	defer cleanup()
	reader, cancel, _ := openStream(t, srv, "user-a", "parent_run_id=r_parent_1")
	defer cancel()
	ev, data, ok := readSSEFrame(t, reader)
	if !ok || ev != "stream_open" {
		t.Fatalf("first frame = %q %s, want stream_open", ev, data)
	}
	var open map[string]any
	if err := json.Unmarshal([]byte(data), &open); err != nil {
		t.Fatal(err)
	}
	if open["filter_parent_run_id"] != "r_parent_1" || open["filter_walk_id"] != "" {
		t.Errorf("stream_open = %s, want filter_parent_run_id r_parent_1 and an empty filter_walk_id", data)
	}
}
