package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// callListRuns drives one list_runs call and returns its result.
func callListRuns(t *testing.T, mc *mockConnector, args string) loommcp.CallToolResult {
	t.Helper()
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_runs","arguments":` + args + `}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &res); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	return res
}

// A walk_id lists that walk through ListWalkRuns, passing the page arguments
// through and returning the page with its next_cursor.
func TestServer_ListRuns_ByWalkIDListsThatWalksPage(t *testing.T) {
	listed := false
	mc := &mockConnector{
		listCallback: func() { listed = true },
		listWalkResult: connector.WalkRuns{
			Runs:       []connector.Run{{RunID: "r_walk"}, {RunID: "r_member", AwaitedState: "channel"}},
			NextCursor: "run_0000000000000001_r_member",
		},
	}
	res := callListRuns(t, mc, `{"walk_id":"r_walk","limit":2,"cursor":"run_0000000000000000_r_a"}`)
	if res.IsError {
		t.Fatalf("list_runs by walk_id errored: %v", res.Content)
	}
	if got, _ := mc.listWalkArgs.Load().(listWalkCall); got != (listWalkCall{"r_walk", 2, "run_0000000000000000_r_a"}) {
		t.Errorf("ListWalkRuns saw %+v, want walk r_walk, limit 2 and the cursor", got)
	}
	if listed {
		t.Error("a walk_id listing also went through the user listing")
	}
	var got connector.WalkRuns
	if err := json.Unmarshal([]byte(res.Content[0].Text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Runs) != 2 || got.NextCursor != "run_0000000000000001_r_member" || got.Runs[1].AwaitedState != "channel" {
		t.Errorf("result = %+v, want the page and its next_cursor", got)
	}
}

// Both, neither, or an argument that belongs to the other form is a validation
// refusal before any read.
func TestServer_ListRuns_RefusesAMixedOrEmptyRequest(t *testing.T) {
	for args, want := range map[string]string{
		`{"user_id":"u","walk_id":"r_w"}`:              "exactly one of user_id, walk_id or parent_run_id",
		`{"walk_id":"r_w","parent_run_id":"r_p"}`:      "exactly one of user_id, walk_id or parent_run_id",
		`{"user_id":"u","parent_run_id":"r_p"}`:        "exactly one of user_id, walk_id or parent_run_id",
		`{}`:                                           "exactly one of user_id, walk_id or parent_run_id",
		`{"walk_id":"r_w","status":"running"}`:         "status filters a user_id listing only",
		`{"parent_run_id":"r_p","status":"completed"}`: "status filters a user_id listing only",
		`{"user_id":"u","cursor":"run_00_r_a"}`:        "cursor pages a walk_id or parent_run_id listing only",
	} {
		listed := false
		mc := &mockConnector{listCallback: func() { listed = true }}
		res := callListRuns(t, mc, args)
		if !res.IsError || !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("list_runs %s = %v, want a tool error saying %q", args, res.Content, want)
		}
		if listed || mc.listWalkArgs.Load() != nil {
			t.Errorf("list_runs %s reached the connector", args)
		}
	}
}

// A foreign cursor comes back as a validation error the caller can act on.
func TestServer_ListRuns_ForeignCursorIsAValidationError(t *testing.T) {
	mc := &mockConnector{listWalkErr: fmt.Errorf("%w %q", store.ErrInvalidRunCursor, "cur_0")}
	res := callListRuns(t, mc, `{"walk_id":"r_w","cursor":"cur_0"}`)
	if !res.IsError || !strings.Contains(string(res.StructuredContent), `"validation"`) {
		t.Errorf("foreign cursor = %v / %s, want a validation tool error", res.Content, res.StructuredContent)
	}
}

// The user listing is unchanged.
func TestServer_ListRuns_ByUserIDStillListsTheUser(t *testing.T) {
	listed := false
	mc := &mockConnector{listCallback: func() { listed = true }}
	if res := callListRuns(t, mc, `{"user_id":"u","status":"running"}`); res.IsError {
		t.Fatalf("list_runs by user_id errored: %v", res.Content)
	}
	if !listed || mc.listWalkArgs.Load() != nil {
		t.Errorf("user listing reached ListRuns %v, ListWalkRuns %v; want only ListRuns", listed, mc.listWalkArgs.Load() != nil)
	}
}

// A parent_run_id lists that run's children through ListChildRuns, not the
// walk listing, passing the page arguments through.
func TestServer_ListRuns_ByParentRunIDListsThatRunsChildren(t *testing.T) {
	listed := false
	mc := &mockConnector{
		listCallback: func() { listed = true },
		listWalkResult: connector.WalkRuns{
			Runs:       []connector.Run{{RunID: "r_child", ParentRunID: "r_parent", AwaitedState: "channel"}},
			NextCursor: "run_0000000000000001_r_child",
		},
	}
	res := callListRuns(t, mc, `{"parent_run_id":"r_parent","limit":1,"cursor":"run_0000000000000000_r_a"}`)
	if res.IsError {
		t.Fatalf("list_runs by parent_run_id errored: %v", res.Content)
	}
	if got, _ := mc.listWalkArgs.Load().(listWalkCall); got != (listWalkCall{"parent:r_parent", 1, "run_0000000000000000_r_a"}) {
		t.Errorf("the connector saw %+v, want ListChildRuns of r_parent with limit 1 and the cursor", got)
	}
	if listed {
		t.Error("a parent_run_id listing also went through the user listing")
	}
	var got connector.WalkRuns
	if err := json.Unmarshal([]byte(res.Content[0].Text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Runs) != 1 || got.Runs[0].ParentRunID != "r_parent" || got.NextCursor != "run_0000000000000001_r_child" {
		t.Errorf("result = %+v, want the page and its next_cursor", got)
	}
}

// The stream tool's walk_id and parent_run_id reach the connector: a filter
// that is not forwarded does not error, it streams every run.
func TestServer_StreamUserRunStates_ForwardsItsFilters(t *testing.T) {
	mc := &mockConnector{streamEvents: []connector.RunStateEvent{{RunID: "r1", UserID: "user-a", Status: "running"}}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stream_user_run_states","arguments":{"user_id":"user-a","walk_id":"r_walk","parent_run_id":"r_parent","max_events":1,"timeout_ms":500}}}` + "\n"
	if resps, _ := driveServer(t, srv, in); len(resps) != 1 {
		t.Fatalf("got %d responses", len(resps))
	}
	if got := mc.lastStreamReq; got.WalkID != "r_walk" || got.ParentRunID != "r_parent" {
		t.Errorf("the connector received walk %q and parent run %q, want r_walk and r_parent", got.WalkID, got.ParentRunID)
	}
}
