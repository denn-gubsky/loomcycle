package builtin

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type stubRerankModel struct {
	reply string
	calls int
}

func (s *stubRerankModel) Complete(context.Context, string) (string, error) {
	s.calls++
	return s.reply, nil
}

func rerankOn() *config.MemoryRerank {
	on := true
	return &config.MemoryRerank{Enabled: &on}
}

// rerankMemoryFixture is a Memory tool holding three embedded rows, run as an
// agent whose memory_rerank is `policy` (nil = the agent never mentions it).
func rerankMemoryFixture(t *testing.T, rr memrank.RerankModel, policy *config.MemoryRerank, backend string) (*Memory, context.Context, func()) {
	t.Helper()
	tool, _, ctx, cleanup := vectorMemoryFixture(t)
	tool.Reranker = rr
	for _, row := range []struct{ key, text string }{
		{"r1", "alice go rust"},
		{"r2", "alice go"},
		{"r3", "alice"},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"`+row.key+
			`","value":"`+row.text+`","embed":true,"embed_text":"`+row.text+`"}`))
		if res.IsError {
			t.Fatalf("set %s: %s", row.key, res.Text)
		}
	}
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"}, Rerank: policy, Backend: backend,
	})
	return tool, ctx, cleanup
}

func searchKeys(t *testing.T, tool *Memory, ctx context.Context) (string, map[string]any) {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"search","scope":"agent","query":"alice go rust","top_k":3}`))
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	var ks []string
	for _, e := range out["entries"].([]any) {
		ks = append(ks, e.(map[string]any)["key"].(string))
	}
	return strings.Join(ks, ","), out
}

// TestMemoryRerank_ReportedOnlyForAnAgentThatEnabledIt — an agent that never
// mentions memory_rerank gets search's order and the response shape it always
// had; one that enables it gets the reranked order and `reranked: true`.
func TestMemoryRerank_ReportedOnlyForAnAgentThatEnabledIt(t *testing.T) {
	rr := &stubRerankModel{reply: "[3, 2]"}
	tool, ctx, cleanup := rerankMemoryFixture(t, rr, nil, "")
	defer cleanup()
	order, out := searchKeys(t, tool, ctx)
	if order != "r1,r2,r3" {
		t.Fatalf("precondition: search order %s", order)
	}
	if _, has := out["reranked"]; has || rr.calls != 0 {
		t.Errorf("an agent without memory_rerank: reranked key present=%v, %d model calls", has, rr.calls)
	}

	tool, ctx, cleanup2 := rerankMemoryFixture(t, rr, rerankOn(), "")
	defer cleanup2()
	order, out = searchKeys(t, tool, ctx)
	if order != "r3,r2,r1" {
		t.Errorf("reranked order = %s, want r3,r2,r1", order)
	}
	if out["reranked"] != true {
		t.Errorf("reranked = %v, want true", out["reranked"])
	}
	if _, has := out["rerank_reason"]; has {
		t.Error("a successful rerank carries no reason")
	}
}

// TestMemoryRerank_AnUnconfiguredServerSaysSo — enabled on the agent, no
// memory.reranker on the server: search's own order, and the reason.
func TestMemoryRerank_AnUnconfiguredServerSaysSo(t *testing.T) {
	tool, ctx, cleanup := rerankMemoryFixture(t, nil, rerankOn(), "")
	defer cleanup()
	order, out := searchKeys(t, tool, ctx)
	if order != "r1,r2,r3" || out["reranked"] != false || out["rerank_reason"] != memrank.RerankNotConfigured {
		t.Errorf("order %s, reranked %v, reason %v", order, out["reranked"], out["rerank_reason"])
	}
}

// TestMemoryRerank_ANamedInprocessBackendCarriesTheReranker — an agent routed
// through a named in-process memory_backend gets a backend built per call; it
// must be built WITH the reranker, or the agent reports not_configured on a
// server that has one.
func TestMemoryRerank_ANamedInprocessBackendCarriesTheReranker(t *testing.T) {
	rr := &stubRerankModel{reply: "[3]"}
	tool, ctx, cleanup := rerankMemoryFixture(t, rr, rerankOn(), "local")
	defer cleanup()
	tool.Cfg = &config.Config{MemoryBackends: map[string]config.MemoryBackend{"local": {Kind: "inprocess"}}}
	order, out := searchKeys(t, tool, ctx)
	if out["reranked"] != true || !strings.HasPrefix(order, "r3") || rr.calls != 1 {
		t.Errorf("named backend: order %s, reranked %v (%v), %d calls", order, out["reranked"], out["rerank_reason"], rr.calls)
	}
}

// TestMemoryRerank_RecallReportsItToo — recall carries the report; its default
// (the agent's own facts and notes) is not a document search.
func TestMemoryRerank_RecallReportsItToo(t *testing.T) {
	rr := &stubRerankModel{reply: "[1]"}
	tool, ctx, cleanup := rerankMemoryFixture(t, rr, rerankOn(), "")
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"recall","scope":"agent","query":"alice"}`))
	if res.IsError {
		t.Fatalf("recall: %s", res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	if out["reranked"] != false || out["rerank_reason"] != memrank.RerankNotDocumentSearch || rr.calls != 0 {
		t.Errorf("recall: reranked %v reason %v, %d calls", out["reranked"], out["rerank_reason"], rr.calls)
	}
}

// TestMemoryRerank_HasNoToolParameter — the rerank is a model call per search, so
// neither the model nor a tool caller may request it, size it or switch it off.
// Asserting the ABSENCE means adding a parameter later has to be argued for.
func TestMemoryRerank_HasNoToolParameter(t *testing.T) {
	for name, schema := range map[string]string{"Memory": memoryInputSchema, "Document": documentInputSchema} {
		if strings.Contains(strings.ToLower(schema), "rerank") {
			t.Errorf("the %s input schema mentions a rerank — it must stay operator-only", name)
		}
	}
}

// TestMemoryRerank_IsCarriedByEveryPolicyConstructionSite — a STRUCT-LITERAL
// sweep, driven off the sibling grant's own count (a hand-written list of sites is
// the second copy that drifts). A missed site resolves to nil there: the agent's
// rerank silently off on exactly the path nobody tested.
func TestMemoryRerank_IsCarriedByEveryPolicyConstructionSite(t *testing.T) {
	rerankField := regexp.MustCompile(`(?m)^\s+Rerank:\s`)
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
		if mine := len(rerankField.FindAllString(src, -1)); mine != sibling {
			t.Errorf("%s builds MemoryPolicyValue %d times carrying RecallIncludeTurns but %d carrying Rerank", f, sibling, mine)
		}
	}
}

// TestMemoryRerank_RoundTripsTheAgentDefinition — declared in yaml, frontmatter or
// an AgentDef overlay, the block must survive every adapter its sibling does.
func TestMemoryRerank_RoundTripsTheAgentDefinition(t *testing.T) {
	for _, f := range []string{"../../agents/loader.go", "../../agents/sign.go", "../../lookup/agent.go", "agentdef.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		// Counted as CARRIES — a struct-literal field ("Name: …") or an assignment
		// ("x.Name = …") — not as mentions: a comment, a type or a helper named
		// after the field must not stand in for a carry that is missing.
		carries := func(name string) int {
			n := 0
			for _, m := range regexp.MustCompile(`(?m)(^\s+`+name+`:\s|\.`+name+` = ).*$`).FindAllString(src, -1) {
				if !strings.HasSuffix(strings.TrimSpace(m), "= nil") { // normalising a field away is not a carry
					n++
				}
			}
			return n
		}
		sib, mine := carries("RecallAttachTraces"), carries("MemoryRerank")
		if sib == 0 {
			t.Fatalf("%s carries RecallAttachTraces nowhere — the guard is vacuous", f)
		}
		if mine < sib {
			t.Errorf("%s carries RecallAttachTraces %d times but MemoryRerank only %d", f, sib, mine)
		}
	}
}

// TestMemoryRerank_OverlayRoundTripsAndForksPerField — authored over the
// AgentDef substrate, the block survives create → persist → lookup, and a fork
// that changes only `candidates` keeps the parent's switch. It is
// content-identifying: that fork mints a different content_sha256.
func TestMemoryRerank_OverlayRoundTripsAndForksPerField(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reader","overlay":{`+
		`"system_prompt":"read","memory_rerank":{"enabled":true,"max_chars":900}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	parentSHA, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "reader")
	if !ok || !def.MemoryRerank.On() || def.MemoryRerank.MaxChars != 900 {
		t.Fatalf("create round-trip: ok=%v memory_rerank=%+v", ok, def.MemoryRerank)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"reader","promote":true,`+
		`"overlay":{"memory_rerank":{"candidates":30}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	forkSHA, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	def, _ = lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "reader")
	if !def.MemoryRerank.On() || def.MemoryRerank.Candidates != 30 || def.MemoryRerank.MaxChars != 900 {
		t.Errorf("fork merged to %+v, want enabled kept, candidates 30, max_chars 900 kept", def.MemoryRerank)
	}
	if parentSHA == "" || parentSHA == forkSHA {
		t.Errorf("a fork changing only memory_rerank kept content_sha256 %q — the block is not hashed", parentSHA)
	}
}

// TestMemoryRerank_OverlayIsValidatedLikeYaml — the bounds operator yaml is held
// to apply to a def authored over the substrate too.
func TestMemoryRerank_OverlayIsValidatedLikeYaml(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"reader","overlay":{`+
		`"system_prompt":"read","memory_rerank":{"enabled":true,"candidates":500}}}`))
	if !res.IsError || !strings.Contains(res.Text, "memory_rerank.candidates") {
		t.Errorf("an out-of-range candidates must be refused, got: %s", res.Text)
	}
}

// noReportBackend serves searches but never reports a rerank, as a remote memory
// layer does.
type noReportBackend struct{ memrank.Backend }

func (n noReportBackend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memrank.SearchQuery,
	rank memrank.RankConfig, dedup memrank.DedupConfig) (memrank.SearchResult, error) {
	res, err := n.Backend.Search(ctx, scope, scopeID, q, rank, dedup)
	res.Rerank = nil
	return res, err
}

// TestMemoryRerank_ABackendThatDoesNotRerankSaysSo — an agent configured for the
// rerank, served by a backend that returns no report, must read reranked:false
// with the reason, not an absent key it could take for "reranked".
func TestMemoryRerank_ABackendThatDoesNotRerankSaysSo(t *testing.T) {
	tool, ctx, cleanup := rerankMemoryFixture(t, &stubRerankModel{reply: "[3]"}, rerankOn(), "")
	defer cleanup()
	tool.Backend = noReportBackend{tool.newInprocess()}
	_, out := searchKeys(t, tool, ctx)
	if out["reranked"] != false || out["rerank_reason"] != memrank.RerankBackendUnsupported {
		t.Errorf("reranked %v, reason %v; want false, %q", out["reranked"], out["rerank_reason"], memrank.RerankBackendUnsupported)
	}
}
