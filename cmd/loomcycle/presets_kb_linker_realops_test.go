package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The fixture tests pin the linker to the ops' contracts as written. This one
// runs its maintain pass against the REAL Document tool, so a contract the
// fixture got wrong — a field name, a refusal, the lease gate, the grant —
// fails here: R1 (the method grant), R2 (maintain_statement), R3
// (list_statements filters) and R6 (the lease), end to end on sqlite.

func realDocument(t *testing.T) (*builtin.Document, context.Context) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), StatementTimeoutMS: 30000, MaxRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := tools.WithAgentName(context.Background(), "kb/linker")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	ctx = tools.WithRunID(ctx, "run-linker-1")
	ctx = tools.WithRunPolicy(ctx, tools.RunPolicy{
		Tools:            []string{"Document", "Agent", "Channel"},
		StatementMethods: []string{"rule", "extractor"},
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"user"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}},
	})
	return &builtin.Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}, ctx
}

func realDoc(t *testing.T, d *builtin.Document, ctx context.Context, body string) map[string]any {
	t.Helper()
	res, err := d.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

func TestKBLinker_MaintainPassAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	target := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Target"}`)["document_id"].(string)
	law := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Law"}`)["document_id"].(string)
	chunk := func(doc, title, body string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, doc, title, body))["id"].(string)
	}
	c1 := chunk(target, "Clause 1", kbBodyClaim)
	x1 := chunk(law, "Article 1", kbBodyLaw)
	x2 := chunk(law, "Article 2", kbBodyRule)
	propose := func(to, quote string) string {
		return realDoc(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation","evidence":[{"chunk_id":%q,"quote":%q}]}`,
			c1, to, c1, quote))["id"].(string)
	}
	healthy := propose(x1, "report a material incident")
	stale := propose(x2, "within 24 hours")
	// The second statement's quote leaves the passage.
	got := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, c1))
	rev := got["revision"]
	realDoc(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%v,"body":%q}`,
		c1, rev, "BODYALPHA Every operator shall report a material incident to the regulator without delay once it is detected."))

	f := newKBFixture()
	agent := knowledgeBaseConfig(t).Agents["kb/linker"]
	set := []tools.Tool{d, &kbFakeAgent{f: f}, &kbFakeChannel{f: f}}
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/linker",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text",
			Text: fmt.Sprintf(`{"mode":"maintain","document_id":%q,"write":"propose"}`, target)}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(res.FinalText), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, res.FinalText)
	}
	t.Logf("report: %s", res.FinalText)
	if rep["status"] != "complete" {
		t.Fatalf("status = %v, failure %v\n%s", rep["status"], rep["failure"], res.FinalText)
	}
	lease, _ := rep["lease"].(map[string]any)
	if lease["acquired"] != true || lease["released"] != true {
		t.Errorf("lease = %v, want acquired and released through R6 under the R1 grant", lease)
	}
	if refusals, _ := rep["refusals"].(map[string]any); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none from the real ops\n%s", refusals, res.FinalText)
	}

	edge := func(id string) map[string]any {
		s, _ := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, id))["statement"].(map[string]any)
		return s
	}
	if h := edge(healthy); h["verified_at"] == nil {
		t.Errorf("healthy statement = %v, want verified", h)
	}
	s := edge(stale)
	ev, _ := s["evidence"].([]any)
	if len(ev) != 1 || ev[0].(map[string]any)["stale"] != true {
		t.Errorf("stale statement evidence = %v, want its one row marked stale", ev)
	}
	if s["verified_at"] != nil {
		t.Errorf("stale statement = %v, want it not verified", s)
	}
}
