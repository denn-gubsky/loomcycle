package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ownInput is a calling run's untrusted input as the loop places it on ctx.
// The third entry carries the label only the runtime may write.
func ownInput(ctx context.Context) context.Context {
	return tools.WithRunUntrusted(ctx, []tools.UntrustedInput{
		{Kind: "user_input", Text: "evidence one"},
		{Kind: "untrusted", Text: "evidence two"},
		{Kind: "run_metadata", Text: "trigger body"},
	})
}

var ownInputForwarded = []tools.UntrustedInput{
	{Kind: "user_input", Text: "evidence one"},
	{Kind: "untrusted", Text: "evidence two"},
	{Text: "trigger body"}, // the runtime's label does not pass by reference either
}

func sameInputs(t *testing.T, who string, got, want []tools.UntrustedInput) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s got %+v, want %+v", who, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s entry %d = %+v, want %+v", who, i, got[i], want[i])
		}
	}
}

// {"ref":"input"} stands for the calling run's own untrusted input: in order,
// with its kinds, in place among the entries written out beside it.
func TestAgentTool_RefEntryForwardsTheCallingRunsUntrustedInput(t *testing.T) {
	r := &untrustedRunner{}
	res := execJSON(t, r.tool(), ownInput(context.Background()),
		`{"name":"judge","prompt":"p","untrusted":[{"text":"before","kind":"tool_output"},{"ref":"input"},"after"]}`)
	if res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	want := append([]tools.UntrustedInput{{Text: "before", Kind: "tool_output"}}, ownInputForwarded...)
	want = append(want, tools.UntrustedInput{Text: "after"})
	sameInputs(t, "the child", r.got["judge"], want)
}

// The one object form {"ref":"input"}, alone and not in a list, is a
// reference too: a program may write one object where the schema has an array.
func TestAgentTool_RefEntryAloneIsAReference(t *testing.T) {
	r := &untrustedRunner{}
	res := execJSON(t, r.tool(), ownInput(context.Background()), `{"name":"judge","prompt":"p","untrusted":{"ref":"input"}}`)
	if res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	sameInputs(t, "the child", r.got["judge"], ownInputForwarded)
}

// The bare string "input" was valid before references existed, as five
// characters of text. It still is: only the object form is a reference.
func TestAgentTool_UntrustedStringInputIsStillLiteralText(t *testing.T) {
	for name, field := range map[string]string{"one string": `"input"`, "in a list": `["input"]`, "as text": `[{"text":"input"}]`} {
		t.Run(name, func(t *testing.T) {
			r := &untrustedRunner{}
			res := execJSON(t, r.tool(), ownInput(context.Background()), `{"name":"judge","prompt":"p","untrusted":`+field+`}`)
			if res.IsError {
				t.Fatalf("spawn: %s", res.Text)
			}
			sameInputs(t, "the child", r.got["judge"], []tools.UntrustedInput{{Text: "input"}})
		})
	}
}

// A run that was given no untrusted input has nothing to forward. That is an
// empty result, not a failed call.
func TestAgentTool_RefEntryResolvesToNothingWhenTheRunHasNoUntrustedInput(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"never set": context.Background(),
		"set empty": tools.WithRunUntrusted(context.Background(), []tools.UntrustedInput{}),
	} {
		t.Run(name, func(t *testing.T) {
			r := &untrustedRunner{}
			res := execJSON(t, r.tool(), ctx, `{"name":"judge","prompt":"p","untrusted":[{"ref":"input"},"kept"]}`)
			if res.IsError {
				t.Fatalf("spawn: %s", res.Text)
			}
			sameInputs(t, "the child", r.got["judge"], []tools.UntrustedInput{{Text: "kept"}})
		})
	}
}

func TestAgentTool_MalformedRefEntryIsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ field, want string }{
		"unknown ref":   {`[{"ref":"parent"}]`, `unknown ref "parent"`},
		"empty ref":     {`[{"ref":""}]`, `unknown ref ""`},
		"ref and text":  {`[{"ref":"input","text":"also this"}]`, "not both"},
		"ref and kind":  {`[{"ref":"input","kind":"web_content"}]`, "not both"},
		"ref not a str": {`[{"ref":7}]`, "untrusted"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &untrustedRunner{}
			res, err := r.tool().Execute(ownInput(context.Background()), json.RawMessage(`{"name":"judge","prompt":"p","untrusted":`+tc.field+`}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Errorf("want a refusal containing %q, got IsError=%v %q", tc.want, res.IsError, res.Text)
			}
			if len(r.got) != 0 {
				t.Errorf("a child started despite the refusal: %v", r.got)
			}
		})
	}
}

// The limits bound what the child is handed, so they are checked on what a
// reference resolves to: a reference to more text than a child may be given
// is refused as that text written out would be, and so is one that pushes the
// entry count past its bound.
func TestAgentTool_RefEntryPastTheLimitIsRefusedLikeALiteral(t *testing.T) {
	bigCtx := tools.WithRunUntrusted(context.Background(), []tools.UntrustedInput{
		{Kind: "untrusted", Text: strings.Repeat("x", MaxSpawnUntrustedBytes)},
	})
	many := make([]tools.UntrustedInput, MaxSpawnUntrustedEntries)
	for i := range many {
		many[i] = tools.UntrustedInput{Kind: "untrusted", Text: "x"}
	}
	manyCtx := tools.WithRunUntrusted(context.Background(), many)
	for name, tc := range map[string]struct {
		ctx         context.Context
		input, want string
	}{
		"bytes, spawn":          {bigCtx, `{"name":"judge","prompt":"p","untrusted":[{"ref":"input"},"y"]}`, "bytes of text is over the limit"},
		"bytes, parallel_spawn": {bigCtx, `{"op":"parallel_spawn","spawns":[{"name":"ok","prompt":"p"},{"name":"judge","prompt":"p","untrusted":[{"ref":"input"},"y"]}]}`, "bytes of text is over the limit"},
		"bytes, open":           {bigCtx, `{"op":"open","name":"judge","prompt":"p","untrusted":[{"ref":"input"},"y"]}`, "bytes of text is over the limit"},
		"entries, spawn":        {manyCtx, `{"name":"judge","prompt":"p","untrusted":[{"ref":"input"},"y"]}`, "entries is over the limit"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &untrustedRunner{}
			a := r.tool()
			a.OpenChild = func(context.Context, string, string, string, int, int) (string, string, string, error) {
				t.Error("a resident child was opened despite the refusal")
				return "", "", "", nil
			}
			res := execJSON(t, a, tc.ctx, tc.input)
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Errorf("want a refusal containing %q, got IsError=%v %q", tc.want, res.IsError, res.Text)
			}
			if len(r.got) != 0 {
				t.Errorf("children started despite the refusal: %v", r.got)
			}
		})
	}
	// Exactly at the limit is accepted, as a literal of that size is.
	r := &untrustedRunner{}
	if res := execJSON(t, r.tool(), bigCtx, `{"name":"judge","prompt":"p","untrusted":[{"ref":"input"}]}`); res.IsError {
		t.Errorf("a reference resolving to exactly the limit was refused: %q", res.Text)
	}
}

// Each fan-out entry resolves its own reference; a sibling without one gets
// nothing.
func TestAgentTool_ParallelSpawnResolvesARefPerEntry(t *testing.T) {
	r := &untrustedRunner{}
	res := execJSON(t, r.tool(), ownInput(context.Background()), `{"op":"parallel_spawn","spawns":[
		{"name":"a","prompt":"p","untrusted":[{"ref":"input"}]},
		{"name":"b","prompt":"p"},
		{"name":"c","prompt":"p","untrusted":["own",{"ref":"input"}]}]}`)
	if res.IsError {
		t.Fatalf("parallel_spawn: %s", res.Text)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sameInputs(t, "a", r.got["a"], ownInputForwarded)
	sameInputs(t, "b", r.got["b"], nil)
	sameInputs(t, "c", r.got["c"], append([]tools.UntrustedInput{{Text: "own"}}, ownInputForwarded...))
}

// Poll mode starts its children after the call has returned, from stored
// entries: the reference has to be resolved before then, against the caller.
func TestAgentTool_PollModeChildrenGetAResolvedRef(t *testing.T) {
	r := &untrustedRunner{}
	a := r.tool()
	ctx, _ := pollCtx()
	ctx = ownInput(ctx)
	if res := execJSON(t, a, ctx, `{"op":"parallel_spawn","mode":"poll","spawns":[
		{"name":"a","prompt":"p","untrusted":[{"ref":"input"}]},{"name":"b","prompt":"p"}]}`); res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	if p := decodePoll(t, execJSON(t, a, ctx, `{"op":"poll","wait":"all","wait_ms":5000}`)); p.Pending != 0 {
		t.Fatalf("children still pending: %+v", p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sameInputs(t, "a", r.got["a"], ownInputForwarded)
	sameInputs(t, "b", r.got["b"], nil)
}

func TestAgentTool_OpenResolvesARef(t *testing.T) {
	var got []tools.UntrustedInput
	a := (&untrustedRunner{}).tool()
	a.OpenChild = func(ctx context.Context, _, _, _ string, _, _ int) (string, string, string, error) {
		got = tools.SpawnUntrusted(ctx)
		return "r_child", "first turn", "awaiting_input", nil
	}
	if res := execJSON(t, a, ownInput(context.Background()), `{"op":"open","name":"judge","prompt":"p","untrusted":[{"ref":"input"}]}`); res.IsError {
		t.Fatalf("open: %s", res.Text)
	}
	sameInputs(t, "the resident child", got, ownInputForwarded)
}

// send takes no untrusted text, and a reference is no way around that.
func TestAgentTool_SendRefusesARef(t *testing.T) {
	a := (&untrustedRunner{}).tool()
	a.SendChild = func(context.Context, string, string, int) (string, string, error) {
		t.Fatal("the child was sent a turn whose untrusted reference was dropped")
		return "", "", nil
	}
	res := execJSON(t, a, context.Background(), `{"op":"send","child_run_id":"r_1","prompt":"next","untrusted":[{"ref":"input"}]}`)
	if !res.IsError || !strings.Contains(res.Text, "untrusted") {
		t.Errorf("want a refusal naming untrusted, got IsError=%v %q", res.IsError, res.Text)
	}
}

// The schema is what a model is shown: it has to offer the entry, and it must
// still be valid JSON after the description gained quoted text.
func TestAgentTool_SchemaDocumentsTheRefEntry(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string
			Items       struct {
				Properties map[string]json.RawMessage
				Required   []string
			}
		}
	}
	if err := json.Unmarshal((&AgentTool{}).InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	u := schema.Properties["untrusted"]
	if _, ok := u.Items.Properties["ref"]; !ok {
		t.Error(`untrusted items do not declare "ref"`)
	}
	if len(u.Items.Required) != 0 {
		t.Errorf("untrusted items still require %v, which a {ref} entry cannot satisfy", u.Items.Required)
	}
	if !strings.Contains(u.Description, `{"ref": "input"}`) {
		t.Errorf("the description does not show the entry: %q", u.Description)
	}
}
