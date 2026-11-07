package mcp

import (
	"encoding/json"
	"testing"
)

// memberTools classifies every MCP tool whose input names a run, a session,
// a user, a walk or a data scope, by what holds a member (isolated or not) to
// its own conversations there. The census derives the list from the tool
// descriptors and the handlers map, so a tool that gains such a field fails it
// until it is classified here — and confined.
var memberTools = map[string]string{
	"spawn_run":              "applyPrincipal: the run's user is the caller's subject; a session_id continuation needs sessionOwnershipOK",
	"configured_run":         "applyPrincipal; configured-run gate: sessionOwnershipOK",
	"cancel_run":             "connector CancelRun: runOwnershipOK",
	"get_run":                "connector GetRun: runOwnershipOK",
	"compact_run":            "connector CompactRun: runOwnershipOK",
	"delete_run_tree":        "connector DeleteRunTree: runOwnershipOK / sessionOwnershipOK",
	"review_run":             "review gate: runOwnershipOK",
	"retune_run":             "runForSteer: liveRunOwnershipOK",
	"list_runs":              "connector ListRuns / walkRuns / childRuns: OwnRowsOnly / OwnedRowVisible",
	"stream_user_run_states": "OwnRowsOnly: another user's id is refused",
	"interruption_resolve":   "callerOwnsRun",
	"file":                   "File twin: runOwnershipOK on the run",
	"evaluation":             "Evaluation tool: scores, not transcripts; tenant-confined",
	"history":                "History scopes [self user] for a member",
	"memory":                 "Memory tool policy + browse rules (tenant-shared data, by the member trust model)",
	"document":               "Document tool policy + browse rules (tenant-shared data, by the member trust model)",
	"path":                   "Path tool policy + browse rules",
	"credentialdef":          "credential scopes; a member manages user and agent credentials",
	"channeldef":             "channel definitions, not messages",
	"review":                 "document statements, not conversations: a member reviews by its review grant",
	"publish_channel":        "resolveChannelScope: a user scope is the caller's own subject",
	"subscribe_channel":      "resolveChannelScope",
	"peek_channel":           "resolveChannelScope",
	"ack_channel":            "resolveChannelScope",
	"peek_team_channel":      "teamChannelKeyspace: own subject",
	"release_team_channel":   "teamChannelKeyspace: own subject",
}

var memberAddressingProps = map[string]bool{
	"run_id": true, "agent_id": true, "session_id": true, "user_id": true,
	"scope_id": true, "walk_id": true, "parent_run_id": true, "source_session_id": true, "scope": true,
}

func TestMemberToolCensus_EveryRunAddressingToolIsClassified(t *testing.T) {
	found := map[string]bool{}
	for _, d := range toolDescriptors() {
		if _, handled := handlersByName[d.Name]; !handled {
			continue
		}
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(d.InputSchema, &s); err != nil {
			t.Fatalf("%s: input schema: %v", d.Name, err)
		}
		for k := range s.Properties {
			if memberAddressingProps[k] {
				found[d.Name] = true
				break
			}
		}
	}
	if len(found) < 20 {
		t.Fatalf("found only %d addressing tools: the descriptors this census reads have changed", len(found))
	}
	for name := range found {
		if _, ok := memberTools[name]; !ok {
			t.Errorf("tool %s names a run, session, user or scope but is not in memberTools: classify it, and confine a member to its own there", name)
		}
	}
	for name := range memberTools {
		if !found[name] {
			t.Errorf("memberTools lists %s, which no longer names a run, session, user or scope", name)
		}
	}
}
