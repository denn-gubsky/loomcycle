package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func handlerGraph(handler string) string {
	return `{"entry":"work","states":[{"state":"work","handler":` + handler + `},{"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"work","to":"done","on":"success"}]}`
}

func withTopKey(pair string) string {
	return `{` + pair + `,` + strings.TrimPrefix(validTeamGraph, "{")
}

// The overlays a create must refuse: each is a definition the decoder would
// read differently from its text. None of them may be stored, and verify must
// report the same refusal at the key's own path without writing anything.
func TestTeamDef_AnOverlayNotReadAsWrittenIsRefusedAndStoresNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		overlay string
		path    string
		says    []string
	}{
		"two spellings of one handler field": {
			handlerGraph(`{"kind":"agent","agent":"no-such-agent","Agent":"reviewer"}`),
			"states[0].handler.Agent", []string{`"agent"`, `"Agent"`, "states[0].handler"}},
		"a top-level field matched only by case": {
			withTopKey(`"Hooks":{"run_end":["audit"]}`), "Hooks", []string{`Write "hooks"`}},
		"a misspelt top-level field": {
			withTopKey(`"hoks":{"run_end":["audit"]}`), "hoks", []string{`unknown key "hoks"`, `"hooks"`}},
		"a key the runtime does not know": {
			withTopKey(`"extras":1`), "extras", []string{`unknown key "extras"`}},
		"a key repeated exactly": {
			`{"entry":"nowhere",` + strings.TrimPrefix(validTeamGraph, "{"), "entry", []string{"written twice"}},
		"an unknown key in a team's own agent": {
			withTopKey(`"local":{"agents":{"helper":{"model":"m","tols":["Read"]}}}`), "local.agents.helper.tols", []string{`unknown key "tols"`}},
		"a field matched only by case in a team's own channel": {
			withTopKey(`"local":{"channels":{"inbox":{"Scope":"tenant"}}}`), "local.channels.inbox.Scope", []string{`Write "scope"`}},
	} {
		t.Run(name, func(t *testing.T) {
			tool, base, done := teamDefFixture(t)
			defer done()
			ctx := asOperator(base)

			res := teamOp(t, tool, ctx, "create", "strict", tc.overlay)
			if !res.IsError {
				t.Fatalf("create accepted the overlay: %s", res.Text)
			}
			for _, want := range tc.says {
				if !strings.Contains(res.Text, want) {
					t.Errorf("create refusal %q does not say %s", res.Text, want)
				}
			}
			if n := versionCount(t, tool, ctx, "strict"); n != 0 {
				t.Fatalf("the refused definition was stored: %d version(s)", n)
			}

			report := verifyDraft(t, tool, ctx, "strict", tc.overlay, "")
			if report.Valid {
				t.Fatalf("verify calls the overlay valid: %v", report.Issues)
			}
			found := false
			for _, i := range report.Issues {
				if i["path"] == tc.path {
					found = true
					if i["kind"] != "overlay_invalid" || i["severity"] != "refused" {
						t.Errorf("issue at %s = %v, want kind overlay_invalid, severity refused", tc.path, i)
					}
					if detail, _ := i["detail"].(string); !strings.Contains(res.Text, detail) {
						t.Errorf("verify's detail %q is not what create refused with (%q)", detail, res.Text)
					}
				}
			}
			if !found {
				t.Errorf("verify reports no issue at %q: %v", tc.path, report.Issues)
			}
			if n := versionCount(t, tool, ctx, "strict"); n != 0 {
				t.Errorf("verify wrote %d version(s)", n)
			}
		})
	}
}

// verify lists every such key, each at its path, so an editor can mark all
// the lines at once; inside a state it names the state.
func TestTeamDefVerify_ReportsEveryKeyIssueWithItsPath(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	ctx := asOperator(base)
	overlay := `{"Entry":"work","extras":1,
	  "states":[{"state":"work","handler":{"kind":"agent","agent":"a","Agent":"reviewer"}},{"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"work","to":"done","on":"success","when":"x"}]}`
	report := verifyDraft(t, tool, ctx, "strict", overlay, "")
	paths := map[string]map[string]any{}
	for _, i := range report.Issues {
		paths[i["path"].(string)] = i
	}
	for _, want := range []string{"Entry", "extras", "states[0].handler.Agent", "transitions[0].when"} {
		if paths[want] == nil {
			t.Errorf("no issue at %q: %v", want, report.Issues)
		}
	}
	if i := paths["states[0].handler.Agent"]; i != nil && (i["state"] != "work" || i["field"] != "Agent") {
		t.Errorf("the handler issue = %v, want state work and field Agent", i)
	}
	if report.Valid || report.Runnable {
		t.Errorf("valid=%v runnable=%v, want both false", report.Valid, report.Runnable)
	}
}

// The cap is cleared by a sent 0, and the check for "sent" reads the key by
// its exact spelling while the decoder ignored case. A fork that wrote the key
// in another case had its 0 decoded and then not applied: the parent's cap
// was kept without a word. It is refused now, like any such key.
func TestTeamDefFork_ACapKeyInAnotherCaseIsRefusedNotIgnored(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	ctx := asOperator(base)
	createTeam(t, tool, ctx, "capped", withTopKey(`"max_iterations":7`))

	res := teamOp(t, tool, ctx, "fork", "capped", `{"Max_Iterations":0}`)
	if !res.IsError || !strings.Contains(res.Text, `Write "max_iterations"`) {
		t.Fatalf("fork = %q (error %v), want the key's spelling refused", res.Text, res.IsError)
	}
	if n := versionCount(t, tool, ctx, "capped"); n != 1 {
		t.Fatalf("the refused fork stored a version: %d", n)
	}
	// Spelt as the runtime spells it, the same fork clears the cap.
	res = teamOp(t, tool, ctx, "fork", "capped", `{"max_iterations":0}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	var out struct {
		Definition struct {
			MaxIterations int `json:"max_iterations"`
		} `json:"definition"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.Definition.MaxIterations != 0 {
		t.Errorf("forked cap = %d (err %v), want it cleared", out.Definition.MaxIterations, err)
	}
}

// Only what an author sends is judged. A definition stored before this rule,
// with keys the decoder read loosely, still loads and can be forked: the fork
// is built from the stored row as decoded, and the fork's own overlay is what
// has to be written exactly.
func TestTeamDef_ADefinitionStoredBeforeTheRuleStillLoadsAndForks(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	ctx := asOperator(base)
	created := createTeam(t, tool, ctx, "legacy", validTeamGraph)
	defID, _ := created["def_id"].(string)

	// A second version written straight to the store, as an older runtime
	// could have left it: a field in another case and a key nothing takes.
	old := `{"Entry":"review","extras":{"note":"kept by nobody"},
	  "states":[{"state":"review","handler":{"kind":"agent","Agent":"reviewer"}},{"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"review","to":"done","on":"success"}]}`
	row, err := tool.Store.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_older", Name: "legacy", Version: 2, ParentDefID: defID, Definition: json.RawMessage(old)})
	if err != nil {
		t.Fatalf("seed the older row: %v", err)
	}
	if err := tool.Store.TeamDefSetActive(ctx, "", "legacy", row.DefID, "", store.TeamDefPromoter{}); err != nil {
		t.Fatalf("promote the older row: %v", err)
	}

	got := teamOpByID(t, tool, ctx, "get", row.DefID)
	if got.IsError {
		t.Fatalf("get of the older definition: %s", got.Text)
	}
	res := teamOp(t, tool, ctx, "fork", "legacy", `{"max_iterations":3}`)
	if res.IsError {
		t.Fatalf("a fork of the older definition was refused: %s", res.Text)
	}
	var out struct {
		Definition struct {
			Entry  string `json:"entry"`
			States []struct {
				Handler struct {
					Agent string `json:"agent"`
				} `json:"handler"`
			} `json:"states"`
		} `json:"definition"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Definition.Entry != "review" || len(out.Definition.States) == 0 || out.Definition.States[0].Handler.Agent != "reviewer" {
		t.Errorf("the fork lost what the older definition held: %s", res.Text)
	}
}

func teamOpByID(t *testing.T, tool *TeamDef, ctx context.Context, op, defID string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","def_id":"`+defID+`"}`))
	if err != nil {
		t.Fatalf("%s %s: %v", op, defID, err)
	}
	return res
}
