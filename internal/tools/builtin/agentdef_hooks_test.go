package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func seedHookDef(t *testing.T, st store.Store, ctx context.Context, name string, event hooks.Phase) {
	t.Helper()
	def, _ := json.Marshal(hooks.Def{Event: event, Body: hooks.DefBody{Kind: hooks.BodyKindHTTP, URL: "https://h.example"}})
	row, err := st.HookDefCreate(ctx, store.HookDefRow{DefID: "hdf_" + name, Name: name, Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.HookDefSetActive(ctx, "", name, row.DefID, ""); err != nil {
		t.Fatal(err)
	}
}

func agentDefCall(t *testing.T, tool *AgentDef, ctx context.Context, in string) (tools.Result, map[string]any) {
	t.Helper()
	ctx = tools.WithAgentTools(ctx, []string{"*"}) // the operator's own call
	res, err := tool.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		return res, nil
	}
	return res, decodeResult(t, res.Text)
}

// A tool entry {name, hooks} is stored as the tool's name plus its hooks, and
// the hooks are content: part of the definition's hash.
func TestAgentDef_AToolEntryCarriesThatToolsHooks(t *testing.T) {
	tool, ctx, done := agentDefFixture(t)
	defer done()
	seedHookDef(t, tool.Store, ctx, "deny-internal", hooks.PhasePre)
	res, out := agentDefCall(t, tool, ctx, `{"op":"create","name":"scout","overlay":{"system_prompt":"x",
	  "tools":["Read",{"name":"WebFetch","hooks":{"pre":["deny-internal",{"name":"url-gate","url":"https://app.example/g","fail_mode":"closed"}]}}],
	  "hooks":{"agent_stop":[{"name":"check","url":"https://app.example/c"}]}}}`)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	row, err := tool.Store.AgentDefGet(ctx, out["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var def mergedDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	if strings.Join(def.Tools, ",") != "Read,WebFetch" {
		t.Fatalf("tools = %v; want the names", def.Tools)
	}
	pre := def.ToolHooks["WebFetch"][hooks.PhasePre]
	if len(pre) != 2 || pre[0].Ref != "deny-internal" || pre[1].Inline == nil || pre[1].Inline.FailMode != hooks.FailClosed {
		t.Fatalf("WebFetch hooks = %+v", pre)
	}
	if len(def.Hooks[hooks.PhaseAgentStop]) != 1 {
		t.Fatalf("agent hooks = %+v", def.Hooks)
	}
	plain := signFromMergedDef("scout", mergedDef{SystemPrompt: def.SystemPrompt, Tools: def.Tools})
	if row.ContentSHA256 == plain {
		t.Fatalf("the hooks are not part of the content hash")
	}
}

func TestAgentDef_RefusesHooksARunCouldNotFire(t *testing.T) {
	tool, ctx, done := agentDefFixture(t)
	defer done()
	seedHookDef(t, tool.Store, ctx, "stop-check", hooks.PhaseAgentStop)
	cases := map[string]string{
		"missing HookDef":         `{"tools":["Read"],"hooks":{"pre":["nope"]}}`,
		"event mismatch":          `{"tools":["Read"],"hooks":{"pre":["stop-check"]}}`,
		"hooks on a missing tool": `{"tools":["Read"],"tool_hooks":{"WebFetch":{"pre":[{"name":"g","url":"https://x"}]}}}`,
		"run event under a tool":  `{"tools":[{"name":"Read","hooks":{"agent_stop":["stop-check"]}}]}`,
		"inline without a name":   `{"tools":["Read"],"hooks":{"pre":[{"url":"https://x"}]}}`,
		"unknown entry field":     `{"tools":["Read"],"hooks":{"pre":[{"name":"g","url":"https://x","owner":"app"}]}}`,
	}
	for name, overlay := range cases {
		t.Run(name, func(t *testing.T) {
			res, _ := agentDefCall(t, tool, ctx, `{"op":"create","name":"scout","overlay":`+overlay+`}`)
			if !res.IsError {
				t.Fatalf("created; want refused")
			}
		})
	}
}

func TestLiftToolEntries_KeepsPlainNamesAndAppendsToToolHooks(t *testing.T) {
	in := `{"tools":["Read",{"name":"WebFetch","hooks":{"pre":["a"]}}],"tool_hooks":{"WebFetch":{"pre":["b"]}},"model":"m"}`
	out, err := hooks.LiftToolEntries(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools     []string        `json:"tools"`
		ToolHooks hooks.ToolHooks `json:"tool_hooks"`
		Model     string          `json:"model"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	pre := got.ToolHooks["WebFetch"][hooks.PhasePre]
	if strings.Join(got.Tools, ",") != "Read,WebFetch" || len(pre) != 2 || pre[0].Ref != "b" || pre[1].Ref != "a" || got.Model != "m" {
		t.Fatalf("lifted = %s", out)
	}
	plain := json.RawMessage(`{"tools":["Read"]}`)
	if same, _ := hooks.LiftToolEntries(plain); string(same) != string(plain) {
		t.Fatalf("a definition with no tool entries was rewritten: %s", same)
	}
}
