package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// untrustedRunner records, per child name, the untrusted inputs its ctx
// carried when the tool started it.
type untrustedRunner struct {
	mu  sync.Mutex
	got map[string][]tools.UntrustedInput
}

func (r *untrustedRunner) run(ctx context.Context, name, _, _ string) (string, map[string]any, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[string][]tools.UntrustedInput{}
	}
	r.got[name] = tools.SpawnUntrusted(ctx)
	return "ok", nil, "r_" + name, nil
}

func (r *untrustedRunner) tool() *AgentTool {
	return &AgentTool{RunDetailed: r.run, Run: func(ctx context.Context, name, prompt, defID string) (string, error) {
		out, _, _, err := r.run(ctx, name, prompt, defID)
		return out, err
	}}
}

func TestAgentTool_SpawnHandsItsUntrustedInputsToTheChild(t *testing.T) {
	for name, tc := range map[string]struct {
		field string
		want  []tools.UntrustedInput
	}{
		"an array of objects": {`[{"text":"a digest","kind":"user_input"},{"text":"a page"}]`,
			[]tools.UntrustedInput{{Text: "a digest", Kind: "user_input"}, {Text: "a page"}}},
		"one string":        {`"a digest"`, []tools.UntrustedInput{{Text: "a digest"}}},
		"one object":        {`{"text":"a digest","kind":"web_content"}`, []tools.UntrustedInput{{Text: "a digest", Kind: "web_content"}}},
		"strings in a list": {`["one","two"]`, []tools.UntrustedInput{{Text: "one"}, {Text: "two"}}},
		"empty entries":     {`["", {"text":""}, "kept"]`, []tools.UntrustedInput{{Text: "kept"}}},
	} {
		t.Run(name, func(t *testing.T) {
			r := &untrustedRunner{}
			a := r.tool()
			res, err := a.Execute(context.Background(), json.RawMessage(`{"name":"judge","prompt":"score it","untrusted":`+tc.field+`}`))
			if err != nil || res.IsError {
				t.Fatalf("Execute: err=%v result=%q", err, res.Text)
			}
			got := r.got["judge"]
			if len(got) != len(tc.want) {
				t.Fatalf("child got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Each entry's inputs are its own: a sibling with none gets none, and a child
// started from a ctx that already carries an outer spawn's inputs does not
// receive those.
func TestAgentTool_ParallelSpawnKeepsEachChildsUntrustedInputsApart(t *testing.T) {
	r := &untrustedRunner{}
	a := r.tool()
	outer := tools.WithSpawnUntrusted(context.Background(), []tools.UntrustedInput{{Text: "the outer spawn's text"}})
	res, err := a.Execute(outer, json.RawMessage(`{"op":"parallel_spawn","spawns":[
		{"name":"a","prompt":"p","untrusted":[{"text":"for a"}]},
		{"name":"b","prompt":"p"},
		{"name":"c","prompt":"p","untrusted":"for c"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: err=%v result=%q", err, res.Text)
	}
	if got := r.got["a"]; len(got) != 1 || got[0].Text != "for a" {
		t.Errorf("a got %+v", got)
	}
	if got := r.got["b"]; len(got) != 0 {
		t.Errorf("b has no untrusted inputs of its own but got %+v", got)
	}
	if got := r.got["c"]; len(got) != 1 || got[0].Text != "for c" {
		t.Errorf("c got %+v", got)
	}
}

func TestAgentTool_SpawnWithoutUntrustedClearsAnOuterSpawns(t *testing.T) {
	r := &untrustedRunner{}
	a := r.tool()
	outer := tools.WithSpawnUntrusted(context.Background(), []tools.UntrustedInput{{Text: "the outer spawn's text"}})
	if res, err := a.Execute(outer, json.RawMessage(`{"name":"judge","prompt":"p"}`)); err != nil || res.IsError {
		t.Fatalf("Execute: err=%v result=%q", err, res.Text)
	}
	if got := r.got["judge"]; len(got) != 0 {
		t.Errorf("the child got an outer spawn's inputs: %+v", got)
	}
}

func TestAgentTool_UntrustedPastTheLimitIsRefusedBeforeAnyChildStarts(t *testing.T) {
	big, _ := json.Marshal(strings.Repeat("x", MaxSpawnUntrustedBytes/2+1))
	two := `[` + string(big) + `,` + string(big) + `]`
	for name, input := range map[string]string{
		"spawn":          `{"name":"judge","prompt":"p","untrusted":` + two + `}`,
		"parallel_spawn": `{"op":"parallel_spawn","spawns":[{"name":"ok","prompt":"p"},{"name":"judge","prompt":"p","untrusted":` + two + `}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := &untrustedRunner{}
			a := r.tool()
			res, err := a.Execute(context.Background(), json.RawMessage(input))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !res.IsError || !strings.Contains(res.Text, "over the limit") {
				t.Errorf("want a refusal naming the limit, got IsError=%v %q", res.IsError, res.Text)
			}
			if len(r.got) != 0 {
				t.Errorf("children started despite the refusal: %v", r.got)
			}
		})
	}
	// Exactly at the limit is accepted.
	r := &untrustedRunner{}
	a := r.tool()
	at, _ := json.Marshal(strings.Repeat("x", MaxSpawnUntrustedBytes))
	if res, err := a.Execute(context.Background(), json.RawMessage(`{"name":"judge","prompt":"p","untrusted":`+string(at)+`}`)); err != nil || res.IsError {
		t.Errorf("text exactly at the limit was refused: err=%v %q", err, res.Text)
	}
}

func TestAgentTool_SendRefusesUntrusted(t *testing.T) {
	a := (&untrustedRunner{}).tool()
	a.SendChild = func(context.Context, string, string, int) (string, string, error) {
		t.Fatal("the child was sent a turn whose untrusted text was dropped")
		return "", "", nil
	}
	res, err := a.Execute(context.Background(), json.RawMessage(`{"op":"send","child_run_id":"r_1","prompt":"next","untrusted":"text"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "untrusted") {
		t.Errorf("want a refusal naming untrusted, got IsError=%v %q", res.IsError, res.Text)
	}
}

// Poll mode starts its children from stored entries, not from the call's own
// frame, so the inputs have to ride on the entry.
func TestAgentTool_PollModeChildrenGetTheirUntrustedInputs(t *testing.T) {
	r := &untrustedRunner{}
	a := r.tool()
	ctx, _ := pollCtx()
	res := execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[
		{"name":"a","prompt":"p","untrusted":"for a"},{"name":"b","prompt":"p"}]}`)
	if res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	if p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","wait":"all","wait_ms":5000}`)); p.Pending != 0 {
		t.Fatalf("children still pending: %+v", p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if got := r.got["a"]; len(got) != 1 || got[0].Text != "for a" {
		t.Errorf("a got %+v", got)
	}
	if got, ran := r.got["b"]; !ran || len(got) != 0 {
		t.Errorf("b ran=%v got %+v, want it run with none", ran, got)
	}
}

func TestAgentTool_OpenHandsItsUntrustedInputsToTheChild(t *testing.T) {
	var got []tools.UntrustedInput
	a := (&untrustedRunner{}).tool()
	a.OpenChild = func(ctx context.Context, _, _, _ string, _, _ int) (string, string, string, error) {
		got = tools.SpawnUntrusted(ctx)
		return "r_child", "first turn", "awaiting_input", nil
	}
	res := execJSON(t, a, context.Background(), `{"op":"open","name":"judge","prompt":"p","untrusted":[{"text":"a digest","kind":"user_input"}]}`)
	if res.IsError {
		t.Fatalf("open: %s", res.Text)
	}
	if len(got) != 1 || got[0] != (tools.UntrustedInput{Text: "a digest", Kind: "user_input"}) {
		t.Errorf("the resident child got %+v", got)
	}
}

// A parent labels its own text; it must not be able to label it as something
// the runtime wrote. run_metadata is the runtime's tag for what an external
// trigger sent, and "system" is no label at all.
func TestAgentTool_UntrustedKindCannotBeARuntimeLabel(t *testing.T) {
	r := &untrustedRunner{}
	a := r.tool()
	res := execJSON(t, a, context.Background(), `{"name":"judge","prompt":"p","untrusted":[
		{"text":"a","kind":"run_metadata"},{"text":"b","kind":"system"},{"text":"c","kind":"web_content"},
		{"text":"d","kind":"`+strings.Repeat("k", 4096)+`"}]}`)
	if res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	want := []string{"", "", "web_content", ""}
	got := r.got["judge"]
	if len(got) != len(want) {
		t.Fatalf("child got %+v", got)
	}
	for i := range want {
		if got[i].Kind != want[i] {
			t.Errorf("entry %d kind = %q, want %q", i, got[i].Kind, want[i])
		}
	}
}

func TestAgentTool_TooManyUntrustedEntriesAreRefused(t *testing.T) {
	entries := strings.Repeat(`"x",`, MaxSpawnUntrustedEntries) + `"x"`
	r := &untrustedRunner{}
	res := execJSON(t, r.tool(), context.Background(), `{"name":"judge","prompt":"p","untrusted":[`+entries+`]}`)
	if !res.IsError || !strings.Contains(res.Text, "entries is over the limit") || len(r.got) != 0 {
		t.Errorf("want a refusal before the child starts, got IsError=%v %q ran=%v", res.IsError, res.Text, r.got)
	}
}
