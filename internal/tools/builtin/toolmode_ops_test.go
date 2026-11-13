package builtin

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// schemaOps reads the op enum from a tool's own input schema, so the list the
// tables are checked against is the one the model is shown, not a copy.
func schemaOps(t *testing.T, tool tools.Tool) []string {
	t.Helper()
	var s struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema(), &s); err != nil {
		t.Fatalf("%s schema: %v", tool.Name(), err)
	}
	if len(s.Properties.Op.Enum) == 0 {
		t.Fatalf("%s schema has no op enum", tool.Name())
	}
	return s.Properties.Op.Enum
}

// modalTools is each tool a tool_modes entry may name, with its op table.
func modalTools() map[string]struct {
	tool  tools.Tool
	table map[string]opRead
} {
	return map[string]struct {
		tool  tools.Tool
		table map[string]opRead
	}{
		"Document": {&Document{}, documentOpModes},
		"Path":     {&Path{}, pathOpModes},
	}
}

// Every op a modal tool offers is classified, and the table names no op the
// tool does not offer: a new op must be judged read or write before it ships,
// and a stale entry is a classification nobody checks any more.
func TestToolModes_EveryOpOfAModalToolIsClassified(t *testing.T) {
	for name, m := range modalTools() {
		ops := schemaOps(t, m.tool)
		for _, op := range ops {
			if _, ok := m.table[op]; !ok {
				t.Errorf("%s op %q is not classified read or write in its tool_modes table", name, op)
			}
		}
		for op := range m.table {
			if !slices.Contains(ops, op) {
				t.Errorf("%s tool_modes table classifies %q, which the tool's schema does not offer", name, op)
			}
		}
	}
}

// The tools config accepts a mode for are exactly the tools that classify
// their calls: a mode the loader accepts on a tool that cannot classify would
// refuse every call to it.
func TestToolModes_ConfigToolsAreTheModalTools(t *testing.T) {
	var want, got []string
	for n := range config.ToolModeTools {
		want = append(want, n)
	}
	for n, m := range modalTools() {
		if m.tool.Name() != n {
			t.Errorf("modalTools key %q names tool %q", n, m.tool.Name())
		}
		if _, ok := m.tool.(tools.ModalTool); !ok {
			t.Errorf("%s is not a tools.ModalTool", n)
		}
		got = append(got, n)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(want, got) {
		t.Fatalf("config.ToolModeTools = %v, modal tools = %v", want, got)
	}
}

// config.ToolDeclared restates the runtime's tool-grant rule because config may
// not import the policy package; it must give the same answer, or the loader
// would refuse (or accept) a mode on a tool the run would (or would not) hold.
func TestToolModes_ConfigToolDeclaredAgreesWithTheGrantRule(t *testing.T) {
	for _, declared := range [][]string{nil, {"Document"}, {"Doc*"}, {"*"}, {"Path", "mcp__x__*"}, {"Documents"}, {"Document*"}} {
		for _, tool := range []string{"Document", "Path", "Doc", "mcp__x__y"} {
			if got, want := config.ToolDeclared(tool, declared), grantsTool(declared, tool); got != want {
				t.Errorf("ToolDeclared(%q, %v) = %v, the runtime grant rule says %v", tool, declared, got, want)
			}
		}
	}
}

func TestToolModes_CallIsReadJudgesTheArguments(t *testing.T) {
	d, p := &Document{}, &Path{}
	for _, tc := range []struct {
		tool tools.ModalTool
		in   string
		read bool
	}{
		{d, `{"op":"search","query":"x"}`, true},
		{d, `{"op":"get_document","id":"d1"}`, true},
		{d, `{"op":"update_chunk","id":"c1"}`, false},
		{d, `{"op":"import_md","markdown":"# x"}`, false},
		{d, `{"op":"classify","id":"d1","write":"report"}`, true},
		{d, `{"op":"classify","id":"d1"}`, false}, // the default, propose, writes
		{d, `{"op":"classify","id":"d1","write":"propose"}`, false},
		{d, `{"op":"find_duplicates"}`, true},
		{d, `{"op":"find_duplicates","propose":true}`, false},
		{d, `{"op":"resolve_anchor","work_key":"k"}`, true},
		{d, `{"op":"resolve_anchor","work_key":"k","create_placeholder":true}`, false},
		// The last of two keys wins in the tool's decode, so here too.
		{d, `{"op":"resolve_anchor","create_placeholder":false,"create_placeholder":true}`, false},
		{d, `{"op":"copy_documents","dry_run":true}`, false},
		{d, `{"op":"no_such_op"}`, false},
		{d, `{}`, false},
		{d, `not json`, false},
		{d, `{"op":"search","propose":"yes"}`, false}, // undecodable as the tool decodes it
		{p, `{"op":"ls","path":"/"}`, true},
		{p, `{"op":"stat","path":"/a"}`, true},
		{p, `{"op":"resolve","path":"/a"}`, true},
		{p, `{"op":"mkdir","path":"/a"}`, false},
		{p, `{"op":"mv","path":"/a","to":"/b"}`, false},
		{p, `{"op":"rm","path":"/a"}`, false},
	} {
		if _, got := tc.tool.CallIsRead(json.RawMessage(tc.in)); got != tc.read {
			t.Errorf("%T %s: read = %v, want %v", tc.tool, tc.in, got, tc.read)
		}
	}
}
