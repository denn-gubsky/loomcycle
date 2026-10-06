package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

type teamDefSchemaShape struct {
	Properties map[string]struct {
		Enum        []string `json:"enum"`
		Description string   `json:"description"`
	} `json:"properties"`
}

func parseTeamDefSchema(t *testing.T, raw json.RawMessage) teamDefSchemaShape {
	t.Helper()
	var s teamDefSchemaShape
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return s
}

func advertisedTool(t *testing.T, name string) (desc string, schema json.RawMessage) {
	t.Helper()
	for _, td := range toolDescriptors() {
		if td.Name == name {
			return td.Description, td.InputSchema
		}
	}
	t.Fatalf("no %q tool in the MCP surface", name)
	return "", nil
}

// The MCP teamdef tool advertises the in-run tool's ops minus the poll-mode
// ones an MCP session (which has no calling run) can never complete, keeps the
// rest of the in-run schema, and lists every op it advertises in its
// description — the description is the MCP client's documentation.
func TestTeamDefMCP_SchemaAdvertisesOnlyOffRunOps(t *testing.T) {
	raw, _ := builtin.MCPWrapperInputSchema("teamdef")
	inRun := parseTeamDefSchema(t, raw)
	if len(inRun.Properties["op"].Enum) < 10 || !slices.Contains(inRun.Properties["mode"].Enum, "poll") {
		t.Fatalf("in-run teamdef schema changed shape (op=%v mode=%v) — update this test",
			inRun.Properties["op"].Enum, inRun.Properties["mode"].Enum)
	}
	var wantOps []string
	for _, op := range inRun.Properties["op"].Enum {
		if op != "poll" && op != "cancel" {
			wantOps = append(wantOps, op)
		}
	}

	desc, schema := advertisedTool(t, "teamdef")
	got := parseTeamDefSchema(t, schema)
	if !slices.Equal(got.Properties["op"].Enum, wantOps) {
		t.Errorf("MCP teamdef op enum = %v, want %v (the in-run ops minus poll and cancel)", got.Properties["op"].Enum, wantOps)
	}
	if m := got.Properties["mode"].Enum; !slices.Equal(m, []string{"detach"}) {
		t.Errorf("MCP teamdef mode enum = %v, want [detach]", m)
	}
	if strings.Contains(got.Properties["mode"].Description, `"poll"`) {
		t.Errorf("MCP teamdef mode description still offers poll: %q", got.Properties["mode"].Description)
	}
	for _, p := range []string{"notify", "on_parent_end", "run_ids", "wait", "wait_ms"} {
		if _, ok := got.Properties[p]; ok {
			t.Errorf("MCP teamdef still advertises the poll-only property %q", p)
		}
	}
	// Everything else is the in-run schema, untouched.
	for p := range inRun.Properties {
		if _, ok := got.Properties[p]; !ok && !slices.Contains([]string{"notify", "on_parent_end", "run_ids", "wait", "wait_ms"}, p) {
			t.Errorf("MCP teamdef dropped %q, which works off-run", p)
		}
	}

	m := regexp.MustCompile(`Ops: ([a-z_, ]+)\.`).FindStringSubmatch(desc)
	if m == nil {
		t.Fatalf("teamdef description has no \"Ops: …\" list: %q", desc)
	}
	listed := strings.Split(m[1], ", ")
	slices.Sort(listed)
	sortedWant := slices.Clone(wantOps)
	slices.Sort(sortedWant)
	if !slices.Equal(listed, sortedWant) {
		t.Errorf("teamdef description lists ops %v, want exactly the advertised %v", listed, sortedWant)
	}
}

type teamDefCountingConnector struct {
	*mockConnector
	calls atomic.Int32
}

func (c *teamDefCountingConnector) TeamDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	c.calls.Add(1)
	return connector.ToolResult{Text: `{"ok":true}`}, nil
}

// A client that ignores the schema and asks for poll mode anyway is refused
// before the tool, with a validation error that names the transport's limit
// and what to do instead; every other call still reaches the tool.
func TestTeamDefMCP_PollModeRefusedBeforeTheTool(t *testing.T) {
	for _, body := range []string{
		`{"op":"poll"}`,
		`{"op":"cancel","run_ids":["r1"]}`,
		`{"op":"run","name":"t","input":"go","mode":"poll"}`,
	} {
		cc := &teamDefCountingConnector{mockConnector: &mockConnector{}}
		res, err := handlersByName["teamdef"](context.Background(), &handlerEnv{connector: cc}, json.RawMessage(body))
		if err != nil {
			t.Fatalf("%s: handler error %v", body, err)
		}
		if !res.IsError || !strings.Contains(res.Content[0].Text, "MCP session") {
			t.Errorf("%s: want a refusal naming the MCP session, got isError=%v %q", body, res.IsError, res.Content[0].Text)
		}
		if s := decodeStructured(t, res); s["errorCategory"] != "validation" {
			t.Errorf("%s: errorCategory = %v, want validation", body, s["errorCategory"])
		}
		if n := cc.calls.Load(); n != 0 {
			t.Errorf("%s: reached the tool %d time(s); want refused before it", body, n)
		}
	}
	for _, body := range []string{
		`{"op":"run","name":"t","input":"go","mode":"detach"}`,
		`{"op":"run","name":"t","input":"go"}`,
		`{"op":"list","name":"t"}`,
	} {
		cc := &teamDefCountingConnector{mockConnector: &mockConnector{}}
		res, err := handlersByName["teamdef"](context.Background(), &handlerEnv{connector: cc}, json.RawMessage(body))
		if err != nil || res.IsError || cc.calls.Load() != 1 {
			t.Errorf("%s: want forwarded to the tool once, got err=%v isError=%v calls=%d", body, err, res != nil && res.IsError, cc.calls.Load())
		}
	}
}
