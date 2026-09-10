package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// A canvas persists node positions by forking with a `layout`. applyTeamOverlay
// merges the definition PER TOP-LEVEL FIELD, so a field it does not name is
// silently dropped — the save appears to succeed and the positions are gone.
// Colors had this handled; Layout is its exact sibling and needs the same case.
func TestTeamDefTool_Fork_PersistsLayout(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	createTeam(t, tool, ctx, "layout-team", `{
	  "entry":"review",
	  "states":[
	    {"state":"review","handler":{"kind":"agent","agent":"reviewer"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"review","to":"done","on":"success"}]
	}`)

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"layout-team","overlay":{"layout":{"nodes":{"review":{"x":120,"y":40,"w":220,"h":90},"done":{"x":480,"y":40}}}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forked := decodeResult(t, res.Text)

	defJSON, _ := json.Marshal(forked["definition"])
	if !strings.Contains(string(defJSON), `"layout"`) {
		t.Fatalf("the fork dropped `layout` — a canvas save would silently lose every node position.\nstored definition: %s", defJSON)
	}

	var got struct {
		Layout struct {
			Nodes map[string]struct{ X, Y, W, H int } `json:"nodes"`
		} `json:"layout"`
		States []json.RawMessage `json:"states"`
	}
	if err := json.Unmarshal(defJSON, &got); err != nil {
		t.Fatalf("unmarshal stored definition: %v", err)
	}
	if n := got.Layout.Nodes["review"]; n.X != 120 || n.Y != 40 || n.W != 220 || n.H != 90 {
		t.Errorf("review position = %+v, want {120 40 220 90}", n)
	}
	// A layout-only fork must not disturb the graph it is laying out.
	if len(got.States) != 2 {
		t.Errorf("states = %d, want 2 — a layout overlay must not replace the graph", len(got.States))
	}
}

// The complement, and the reason Layout is worth carrying: laying a graph out
// must not change its identity, so a layout-only fork keeps the parent's hash.
func TestTeamDefTool_Fork_LayoutDoesNotChangeContentHash(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()

	created := createTeam(t, tool, ctx, "hash-team", `{
	  "entry":"review",
	  "states":[
	    {"state":"review","handler":{"kind":"agent","agent":"reviewer"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"review","to":"done","on":"success"}]
	}`)

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"fork","name":"hash-team","overlay":{"layout":{"nodes":{"review":{"x":999,"y":777}}}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forked := decodeResult(t, res.Text)

	if got, want := forked["content_sha256"], created["content_sha256"]; got != want {
		t.Errorf("laying out the graph changed its identity:\n  forked: %v\n  parent: %v", got, want)
	}
}
