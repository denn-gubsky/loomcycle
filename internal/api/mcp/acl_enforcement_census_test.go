package mcp

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// The meta-tool gate asks exactly the pairs the enforcement table credits to
// it (RFC FY Part E).
func TestACLEnforcement_MetaToolsAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	for name := range handlersByName {
		for _, s := range mcpToolQuestions(name) {
			asked[s.Action+" "+s.Type] = true
		}
	}
	if len(asked) == 0 {
		t.Fatal("no meta-tool asked anything")
	}
	table := map[string]bool{}
	for _, p := range authz.PairsAskedAt(authz.SiteMCPTool) {
		table[p] = true
		if !asked[p] {
			t.Errorf("the enforcement table credits %q to the meta-tools, which never ask it", p)
		}
	}
	var missing []string
	for p := range asked {
		if !table[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("a meta-tool asks %q, which the enforcement table does not credit to the meta-tools", p)
	}
}

// Every meta-tool names its HTTP twin or why it has none, and a twin names a
// governed route: a meta-tool cannot land asking a weaker question than its
// route by omission.
func TestMCPToolTwins_EveryToolIsListed(t *testing.T) {
	for name := range handlersByName {
		tw, twinned := mcpToolTwins[name]
		_, without := mcpToolsWithoutTwin[name]
		if twinned == without {
			t.Errorf("%s: twinned=%v without=%v, want exactly one", name, twinned, without)
			continue
		}
		if twinned && tw.Ops == nil {
			if _, governed := authz.HTTPSurface(tw.Method, tw.Path); !governed {
				t.Errorf("%s: twin %s %s is not a governed route", name, tw.Method, tw.Path)
			}
		}
		for op, ot := range tw.Ops {
			if _, governed := authz.HTTPSurface(ot.Method, ot.Path); !governed {
				t.Errorf("%s op %s: twin %s %s is not a governed route", name, op, ot.Method, ot.Path)
			}
		}
	}
	for name := range mcpToolTwins {
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("mcpToolTwins lists %s, which is not a meta-tool", name)
		}
	}
	for name := range mcpToolsWithoutTwin {
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("mcpToolsWithoutTwin lists %s, which is not a meta-tool", name)
		}
	}
}

// Every op a meta-tool's schema offers is classified (RFC GD P1): a per-op
// tool maps exactly its schema's ops, each to a governed route, and refuses
// any other; a tool that takes several ops through one question says why in
// mcpOpsInBody. A new op on a per-op tool, or a new multi-op tool, fails here
// until it is classified, so it cannot reach its handler on another op's
// question.
func TestMCPToolTwins_EveryOpIsClassified(t *testing.T) {
	seen := 0
	for _, d := range toolDescriptors() {
		var sch struct {
			Properties struct {
				Op struct {
					Enum []string `json:"enum"`
				} `json:"op"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(d.InputSchema, &sch); err != nil {
			t.Fatalf("%s: schema: %v", d.Name, err)
		}
		ops := sch.Properties.Op.Enum
		if len(ops) == 0 {
			continue
		}
		seen++
		tw, twinned := mcpToolTwins[d.Name]
		_, inBody := mcpOpsInBody[d.Name]
		_, without := mcpToolsWithoutTwin[d.Name]
		perOp := twinned && tw.Ops != nil
		n := 0
		for _, b := range []bool{perOp, inBody, without} {
			if b {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s (ops %v): per-op=%v in-body=%v without-twin=%v, want exactly one", d.Name, ops, perOp, inBody, without)
			continue
		}
		if !perOp {
			continue
		}
		want := map[string]bool{}
		for _, op := range ops {
			want[op] = true
			if _, ok := tw.Ops[op]; !ok {
				t.Errorf("%s: op %s has no twin route", d.Name, op)
			}
		}
		for op := range tw.Ops {
			if !want[op] {
				t.Errorf("%s: the twin table maps %s, which the schema does not offer", d.Name, op)
			}
		}
		if tw.DefaultOp != "" && !want[tw.DefaultOp] {
			t.Errorf("%s: default op %s is not one of its ops", d.Name, tw.DefaultOp)
		}
		if _, _, err := mcpTwinSurface(d.Name, json.RawMessage(`{"op":"no_such_op"}`)); err == nil {
			t.Errorf("%s: an op the table does not map was let through", d.Name)
		}
	}
	if seen == 0 {
		t.Fatal("no meta-tool offers an op: the schema scan is broken")
	}
	for name := range mcpOpsInBody {
		if tw, ok := mcpToolTwins[name]; !ok || tw.Ops != nil {
			t.Errorf("mcpOpsInBody lists %s, which is not a one-route twinned meta-tool", name)
		}
	}
}

// Each op of a per-op meta-tool asks its own route's question, the instance
// taken from the call: before, channeldef asked subscribe channel:* for every
// op and retention_override read runtime:retention.
func TestMCPTwinSurface_AsksEachOpItsOwnRoutesQuestion(t *testing.T) {
	for _, c := range []struct {
		tool, args, want string
	}{
		{"channeldef", `{"op":"update","name":"ops"}`, "manage channel:ops"},
		{"channeldef", `{"op":"delete","name":"ops"}`, "manage channel:ops"},
		{"channeldef", `{"op":"purge","name":"ops"}`, "manage channel:ops"},
		{"channeldef", `{"op":"create","name":"ops"}`, "manage channel:ops"},
		{"retention_override", `{}`, "read runtime:retention"},
		{"retention_override", `{"op":"set"}`, "manage runtime:retention"},
		{"retention_override", `{"op":"clear"}`, "manage runtime:retention"},
		{"erasure", `{}`, "read runtime:erasure"},
		{"erasure", `{"op":"execute"}`, "manage runtime:erasure"},
		{"get_run", `{}`, "read run:*"},
	} {
		q, ok, err := mcpTwinSurface(c.tool, json.RawMessage(c.args))
		if err != nil || !ok {
			t.Errorf("%s %s: %v %v", c.tool, c.args, ok, err)
			continue
		}
		if got := q.Action + " " + q.Type + ":" + q.Name; got != c.want {
			t.Errorf("%s %s asks %q, want %q", c.tool, c.args, got, c.want)
		}
	}
}
