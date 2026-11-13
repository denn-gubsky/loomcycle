package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// tool_modes on AgentDef create and fork (RFC GL): the overlay is validated as
// operator yaml is, round-trips through the stored definition, may be added by
// a fork and not removed, and is held to the author's own modes.

func execModeDef(t *testing.T, tool *AgentDef, ctx context.Context, call string) (string, bool) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(call))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text, res.IsError
}

func TestAgentDefTool_RefusesAMalformedToolMode(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)
	for _, tc := range []struct {
		name, overlay, want string
	}{
		{"a tool that takes no mode", `"tools":["Read"],"tool_modes":{"Read":"read"}`, `"Read" cannot be given a mode`},
		{"an unknown mode", `"tools":["Document"],"tool_modes":{"Document":"ro"}`, `unknown mode "ro"`},
		{"a tool not in tools", `"tools":["Read"],"tool_modes":{"Document":"read"}`, "Document is not in tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := execModeDef(t, tool, ctx, `{"op":"create","name":"bad","overlay":{"system_prompt":"x",`+tc.overlay+`}}`)
			if !isErr || !strings.Contains(text, tc.want) {
				t.Fatalf("create = %q (error %v), want refused with %q", text, isErr, tc.want)
			}
		})
	}
	// A fork is judged the same way.
	if _, isErr := execModeDef(t, tool, ctx, `{"op":"create","name":"reader","overlay":{"system_prompt":"x","tools":["Document"]}}`); isErr {
		t.Fatal("create reader refused")
	}
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"reader","overlay":{"tool_modes":{"Document":"write"}}}`); !isErr || !strings.Contains(text, `unknown mode "write"`) {
		t.Fatalf("fork with a bad mode = %q (error %v), want refused", text, isErr)
	}
}

func TestAgentDefTool_ToolModesRoundTripThroughTheStoredDefinition(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"create","name":"reader","overlay":{"system_prompt":"x",`+
		`"tools":["Document","Path"],"tool_modes":{"Document":"read","Path":"read"}}}`); isErr {
		t.Fatalf("create: %s", text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "reader")
	if !ok {
		t.Fatal("reader did not resolve")
	}
	if def.ToolModes["Document"] != config.ToolModeRead || def.ToolModes["Path"] != config.ToolModeRead {
		t.Fatalf("resolved tool_modes = %v, want Document and Path read", def.ToolModes)
	}
	// A fork that sends nothing about modes keeps them.
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"reader","promote":true,"overlay":{"system_prompt":"y"}}`); isErr {
		t.Fatalf("fork: %s", text)
	}
	def, _ = lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "reader")
	if def.ToolModes["Document"] != config.ToolModeRead || def.ToolModes["Path"] != config.ToolModeRead {
		t.Fatalf("forked tool_modes = %v, want the parent's kept", def.ToolModes)
	}
}

// A fork may add a read mode and may not remove one, whether it sends a map
// without the tool or an empty one: removing it widens what every later run
// of the lineage may change.
func TestAgentDefTool_ForkCanAddAReadModeButNotRemoveOne(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"create","name":"reader","overlay":{"system_prompt":"x","tools":["Document","Path"]}}`); isErr {
		t.Fatalf("create: %s", text)
	}
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"reader","promote":true,"overlay":{"tool_modes":{"Document":"read"}}}`); isErr {
		t.Fatalf("fork adding a read mode: %s", text)
	}
	for _, overlay := range []string{`{"tool_modes":{}}`, `{"tool_modes":{"Path":"read"}}`} {
		text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"reader","overlay":`+overlay+`}`)
		if !isErr || !strings.Contains(text, "cannot remove the read mode of Document") {
			t.Errorf("fork %s = %q (error %v), want refused", overlay, text, isErr)
		}
	}
	// Dropping the tool narrows further and is allowed; the mode is kept, so
	// a later fork that grants the tool again holds it in read mode.
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"reader","promote":true,"overlay":{"tools":["Path"]}}`); isErr {
		t.Fatalf("fork dropping Document: %s", text)
	}
	def, _ := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "reader")
	if def.ToolModes["Document"] != config.ToolModeRead {
		t.Errorf("tool_modes after dropping Document = %v, want Document still read", def.ToolModes)
	}
}

// A fork of a static agent keeps the static definition's read mode.
func TestAgentDefTool_ForkOfAStaticReadOnlyAgentStaysReadOnly(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)
	tool.Cfg.Agents["static-reader"] = config.AgentDef{SystemPrompt: "s", Tools: []string{"Document"},
		ToolModes: map[string]string{"Document": config.ToolModeRead}}
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"static-reader","overlay":{"tool_modes":{}}}`); !isErr || !strings.Contains(text, "cannot remove the read mode") {
		t.Fatalf("fork removing the static read mode = %q (error %v), want refused", text, isErr)
	}
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"fork","name":"static-reader","promote":true,"overlay":{"system_prompt":"t"}}`); isErr {
		t.Fatalf("fork: %s", text)
	}
	def, _ := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "static-reader")
	if def.ToolModes["Document"] != config.ToolModeRead {
		t.Errorf("forked static agent's tool_modes = %v, want Document read", def.ToolModes)
	}
}

// An author holding Document in read mode cannot mint an agent holding it
// whole: the capability ceiling every other authority field is held to.
func TestAgentDefTool_AReadModeAuthorCannotCreateAWriter(t *testing.T) {
	tool, ctx := ceilingFixture(t)
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Tools = []string{"Read", "Document"}
		rp.ToolModes = map[string]string{"Document": config.ToolModeRead}
	})
	text, isErr := execModeDef(t, tool, ctx, `{"op":"create","name":"sdlc/writer","overlay":{"system_prompt":"w","tier":"middle","tools":["Document"],"volumes":[]}}`)
	if !isErr || !strings.Contains(text, "tool_modes") || !strings.Contains(text, "Document") {
		t.Fatalf("create = %q (error %v), want refused by the tool_modes ceiling", text, isErr)
	}
	if text, isErr := execModeDef(t, tool, ctx, `{"op":"create","name":"sdlc/reader","overlay":{"system_prompt":"r","tier":"middle","tools":["Document"],"volumes":[],"tool_modes":{"Document":"read"}}}`); isErr {
		t.Fatalf("create with the author's own read mode refused: %s", text)
	}
}
