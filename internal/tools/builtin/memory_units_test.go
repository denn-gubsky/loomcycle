package builtin

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestMemoryUnits_IsCarriedByEveryPolicyConstructionSite — a STRUCT-LITERAL sweep
// driven off a sibling's count. A missed site leaves Units nil there, so an agent
// that opted out would get units on exactly the path nobody tested.
func TestMemoryUnits_IsCarriedByEveryPolicyConstructionSite(t *testing.T) {
	unitsField := regexp.MustCompile(`(?m)^\s+Units:\s`)
	for _, f := range []string{"../../api/http/server.go", "../../api/http/resume.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		sibling := strings.Count(src, "RecallIncludeTurns: ")
		if sibling == 0 {
			t.Fatalf("%s carries no RecallIncludeTurns — this guard would assert nothing", f)
		}
		if mine := len(unitsField.FindAllString(src, -1)); mine != sibling {
			t.Errorf("%s builds MemoryPolicyValue %d times carrying RecallIncludeTurns but %d carrying Units", f, sibling, mine)
		}
	}
}

// TestMemoryUnits_RoundTripsTheAgentDefinition — counted as CARRIES (a struct
// literal field or an assignment), the same guard memory_rerank has.
func TestMemoryUnits_RoundTripsTheAgentDefinition(t *testing.T) {
	for _, f := range []string{"../../agents/loader.go", "../../agents/sign.go", "../../lookup/agent.go", "agentdef.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		carries := func(name string) int {
			n := 0
			for _, m := range regexp.MustCompile(`(?m)(^\s+`+name+`:\s|\.`+name+` = ).*$`).FindAllString(src, -1) {
				if !strings.HasSuffix(strings.TrimSpace(m), "= nil") {
					n++
				}
			}
			return n
		}
		sib, mine := carries("RecallAttachTraces"), carries("MemoryUnits")
		if sib == 0 {
			t.Fatalf("%s carries RecallAttachTraces nowhere — the guard is vacuous", f)
		}
		if mine < sib {
			t.Errorf("%s carries RecallAttachTraces %d times but MemoryUnits only %d", f, sib, mine)
		}
	}
}

// TestMemoryUnits_OverlayRoundTripsAndIsHashed — authored as false, it survives
// create → lookup, a fork that does not mention it keeps it, and it changes the
// content hash.
func TestMemoryUnits_OverlayRoundTripsAndIsHashed(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"plain","overlay":{"system_prompt":"read"}}`))
	plainSHA, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reader","overlay":{"system_prompt":"read","memory_units":false}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	sha, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	if sha == "" || sha == plainSHA {
		t.Errorf("memory_units:false did not change the content hash")
	}
	if r, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"reader","promote":true,"overlay":{"description":"x"}}`)); r.IsError {
		t.Fatalf("fork: %s", r.Text)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "reader")
	if !ok || def.MemoryUnits == nil || *def.MemoryUnits {
		t.Errorf("after a fork that did not mention it, memory_units = %v", def.MemoryUnits)
	}
}

// TestMemorySearch_ReportsTheUnitThatFoundAChunk — the Memory tool renders
// matched_unit, and an agent with memory_units:false never sees one.
func TestMemorySearch_ReportsTheUnitThatFoundAChunk(t *testing.T) {
	d, _, ctx, ids := unitsDocFixture(t)
	if _, err := d.ReplaceUnits(ctx, "user", ids["install"], []DerivedUnit{{Kind: memrank.UnitQuestion, Text: "how often to reboot"}}, "m", 1); err != nil {
		t.Fatal(err)
	}
	m := &Memory{Store: d.Store, Embedder: d.Embedder, SqlMem: d.SqlMem, MaxValueBytes: 1 << 16, DefaultQuotaBytes: 1 << 20}
	search := func(policy tools.MemoryPolicyValue) string {
		t.Helper()
		policy.AllowedScopes = []string{"user"}
		res, _ := m.Execute(tools.WithMemoryPolicy(ctx, policy), json.RawMessage(`{"op":"search","scope":"user","query":"reboot","top_k":3}`))
		if res.IsError {
			t.Fatalf("search: %s", res.Text)
		}
		return res.Text
	}
	if out := search(tools.MemoryPolicyValue{}); !strings.Contains(out, `"matched_unit":{"kind":"question","text":"how often to reboot"}`) ||
		strings.Contains(out, `"key":"doc.unit:`) {
		t.Errorf("default search: %s", out)
	}
	off := false
	if out := search(tools.MemoryPolicyValue{Units: &off}); strings.Contains(out, "matched_unit") || strings.Contains(out, "doc.unit:") {
		t.Errorf("memory_units:false search: %s", out)
	}
}
