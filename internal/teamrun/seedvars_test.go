package teamrun

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// declaredJSON declares two variables and reads them in three places: the
// entry agent, a vars state that overwrites one, and the agent after it.
const declaredJSON = `{
  "entry":"first",
  "vars":{"tone":"formal","lang":"en"},
  "states":[
    {"state":"first","handler":{"kind":"agent","agent":"writer","input_template":"${var.tone}/${var.lang}"}},
    {"state":"stamp","handler":{"kind":"vars","set":{"tone":"set-by-state"}}},
    {"state":"second","handler":{"kind":"agent","agent":"writer","input_template":"${var.tone}/${var.lang}"}},
    {"state":"done","handler":{"kind":"terminal"}}
  ],
  "transitions":[
    {"from":"first","to":"stamp","on":"success"},
    {"from":"stamp","to":"second","on":"success"},
    {"from":"second","to":"done","on":"success"}
  ]}`

// walkValues walks declaredJSON from task and returns, per agent state, the
// values its prompt was handed.
func walkValues(t *testing.T, task *Task) map[string]map[string]string {
	t.Helper()
	seen := map[string]map[string]string{}
	r := varsRunner(textSpawn(func(_ context.Context, _ string, p Prompt, _ string) (string, error) {
		seen[p.Values["team.state"]] = p.Values
		return "ok", nil
	}))
	if _, err := Walk(context.Background(), mustParse(t, declaredJSON), task, r); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return seen
}

func TestWalk_EntryStatePromptCarriesTheDeclaredDefaults(t *testing.T) {
	seen := walkValues(t, &Task{Input: "go"})
	first := seen["first"]
	if first["var.tone"] != "formal" || first["var.lang"] != "en" {
		t.Errorf("entry prompt values = tone %q lang %q, want the declared defaults formal/en", first["var.tone"], first["var.lang"])
	}
}

// Lowest first: the team's default, what the start supplied, what a state
// binds while the walk runs.
func TestWalk_SuppliedValueBeatsTheDefaultAndAVarsStateBeatsBoth(t *testing.T) {
	task := &Task{Input: "go"}
	task.SetVar("tone", "supplied")
	seen := walkValues(t, task)
	if got := seen["first"]["var.tone"]; got != "supplied" {
		t.Errorf("entry tone = %q, want the supplied value over the default", got)
	}
	if got := seen["first"]["var.lang"]; got != "en" {
		t.Errorf("entry lang = %q, want the default for the name the start did not supply", got)
	}
	if got := seen["second"]["var.tone"]; got != "set-by-state" {
		t.Errorf("tone after the vars state = %q, want what the state set", got)
	}
	if got := seen["second"]["var.lang"]; got != "en" {
		t.Errorf("lang after the vars state = %q, want it untouched", got)
	}
}

// A task that arrives mid-graph with a value — a walk continued from where an
// earlier one stopped — keeps it: the default fills only what is missing.
func TestWalk_ResumedTaskKeepsAValueItAlreadyCarries(t *testing.T) {
	task := &Task{Input: "go", State: "second", Vars: map[string]string{"tone": "changed-earlier"}}
	seen := walkValues(t, task)
	if _, ran := seen["first"]; ran {
		t.Fatal("the walk ran the entry state — it did not resume at `second`, so this proves nothing")
	}
	if got := seen["second"]["var.tone"]; got != "changed-earlier" {
		t.Errorf("tone = %q, want the carried value, not the default seeded over it", got)
	}
	if got := seen["second"]["var.lang"]; got != "en" {
		t.Errorf("lang = %q, want the default for the name the task did not carry", got)
	}
}

func TestSeedVars_PresentButEmptyIsKept(t *testing.T) {
	task := &Task{Vars: map[string]string{"tone": ""}}
	task.SeedVars(map[string]string{"tone": "formal", "lang": "en"})
	if v, ok := task.Vars["tone"]; !ok || v != "" {
		t.Errorf("tone = %q, want the caller's empty value kept", v)
	}
	if task.Vars["lang"] != "en" {
		t.Errorf("lang = %q, want the default", task.Vars["lang"])
	}
	var none Task
	none.SeedVars(nil)
	if none.Vars != nil {
		t.Error("seeding no defaults allocated a map — a team without vars must leave the task as it was")
	}
}

// A value is literal: the prompt is handed its characters, and substitution
// there is one pass that never reads what it wrote. The one place a value's
// built-in tokens ARE resolved is a vars state copying it, because Expand runs
// the token pass over the result of the variable pass; a ${var.*} inside it is
// still not followed.
func TestSeededValue_IsNotExpandedExceptForBuiltInTokensThroughAVarsState(t *testing.T) {
	d := teamgraph.Definition{Vars: map[string]string{"a": "${now.date} ${var.b}", "b": "B"}}
	task := &Task{}
	task.SeedVars(d.Vars)
	r := varsRunner(nil)
	st := agentState("writer")
	if got := r.envFor(st, task).Values()["var.a"]; got != "${now.date} ${var.b}" {
		t.Errorf("the value handed to prompt assembly = %q, want it untouched", got)
	}
	if _, err := r.RunHandler(context.Background(), varsState(map[string]string{"copy": "${var.a}"}), task); err != nil {
		t.Fatalf("vars: %v", err)
	}
	if got, want := task.Vars["copy"], "2026-09-10 ${var.b}"; got != want {
		t.Errorf("copy = %q, want %q", got, want)
	}
}
