package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// tenantDrainCtx grants the linker the tenant scope (the operator overlay a tenant
// drain needs) and the channels exactly as cfg declares them.
func tenantDrainCtx(ctx context.Context, cfg *config.Config) context.Context {
	defs := map[string]tools.ChannelDef{}
	for name, c := range cfg.Channels {
		defs[name] = tools.ChannelDef{Name: name, Scope: c.Scope, DefaultTTL: c.DefaultTTL, MaxMessages: c.MaxMessages, Semantic: c.Semantic, Publisher: c.Publisher}
	}
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.Channel = tools.ChannelPolicyValue{Subscribe: cfg.Agents["kb/linker"].Channels.Subscribe, Channels: defs}
	})
}

// runRealDrain runs the shipped kb/linker body with the real Document and Channel
// tools and returns its report, whatever its status.
func runRealDrain(t *testing.T, d *builtin.Document, ctx context.Context, cfg *config.Config, input string) map[string]any {
	t.Helper()
	set := []tools.Tool{d, &kbFakeAgent{f: newKBFixture()}, &builtin.Channel{Store: d.Store, Bus: d.Bus}}
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/linker",
		CodeBody:   cfg.Agents["kb/linker"].Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: input}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(res.FinalText), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, res.FinalText)
	}
	t.Logf("report: %s", res.FinalText)
	return rep
}

// TestKBLinker_TenantDocumentDrainAgainstTheRealChannelTool: the crossing between
// the runtime's import and a tenant drain. The real Document tool imports into the
// tenant scope and publishes documents/ingested there; on a deployment that
// declares the channel tenant-scoped, a {"scope":"tenant","queue":"documents"}
// drain reads it through the real Channel tool, runs the DOCUMENT layers (not the
// subject queue's L6 alone) on the tenant document — the terms layer files its
// statements there — and its ack commits the tenant queue's cursor.
func TestKBLinker_TenantDocumentDrainAgainstTheRealChannelTool(t *testing.T) {
	d, base := realDocument(t)
	cfg := knowledgeBaseTenantIngestConfig(t)
	// The ontology is a tenant import too, so it is the queue's first message.
	ctx := tenantDrainCtx(withRealOntology(t, d, base, kbRealOntologyMD), cfg)
	md, _ := json.Marshal("# Закон про послуги\n\n## Стаття 2. Сфера дії\n\n" +
		"Цей Закон регулює відносини, що виникають, коли оператор укладає договір з клієнтом на надання послуг.\n")
	doc := realDoc(t, d, ctx, `{"op":"import_md","scope":"tenant","path":"/kb/act","markdown":`+string(md)+`}`)["document_id"].(string)

	rep := runRealDrain(t, d, ctx, cfg, `{"mode":"drain","scope":"tenant","queue":"documents","write":"propose","terms_scheme":"topics"}`)
	if rep["status"] != "complete" {
		t.Fatalf("status = %v, failure %v", rep["status"], rep["failure"])
	}
	if q, _ := rep["queue"].(map[string]any); q["channel"] != "documents/ingested" {
		t.Errorf("queue = %v, want documents/ingested", rep["queue"])
	}
	msgs, _ := rep["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want the ontology and the act read", rep["messages"])
	}
	for _, raw := range msgs {
		if m := raw.(map[string]any); m["reason"] != "imported" || m["outcome"] != "complete" || m["acked"] != true {
			t.Errorf("message = %v, want each tenant import passed and acked", m)
		}
	}
	if m := msgs[1].(map[string]any); m["document_id"] != doc {
		t.Errorf("second message names %v, want the act %s", m["document_id"], doc)
	}
	list := realDoc(t, d, ctx, `{"op":"list_statements","scope":"tenant","status":"proposed","document_id":"`+doc+`"}`)
	if st, _ := list["statements"].([]any); len(st) == 0 {
		t.Errorf("no statement was filed on the tenant act: %v", list)
	}
	layers, _ := rep["layers"].(map[string]any)
	for _, l := range []string{"L0", "L1", "L2", "L3", "L6"} {
		if _, ok := layers[l]; !ok {
			t.Errorf("layers = %v, want the document layers (missing %s)", layers, l)
		}
	}
	cur, err := d.Store.ChannelCommittedCursor(ctx, "tnt", "documents/ingested", store.MemoryScopeTenant, "")
	if err != nil || cur == "" {
		t.Errorf("tenant queue cursor = %q, %v — the ack did not commit it", cur, err)
	}
}

// TestKBLinker_TenantDocumentDrainRefusesAnotherScopesQueue: on a deployment that
// kept the bundle's user declaration, a tenant documents drain reads the run
// user's USER queue. Every document there is missing from the tenant store, so
// without a guard the drain acks each one as document_missing and silently
// consumes another queue's messages. The runtime names its scope on the message;
// the drain stops at the first one that is not its own, acks nothing, and says
// how to fix the declaration — and the message is still there for its own drain.
func TestKBLinker_TenantDocumentDrainRefusesAnotherScopesQueue(t *testing.T) {
	d, base := realDocument(t)
	cfg := knowledgeBaseConfig(t)
	if cfg.Channels["documents/ingested"].Scope != "user" {
		t.Fatalf("the bundle declares documents/ingested at %q, want user", cfg.Channels["documents/ingested"].Scope)
	}
	ctx := tenantDrainCtx(base, cfg)
	doc := realDoc(t, d, ctx, `{"op":"import_md","scope":"user","path":"/notes/a","markdown":"# Notes\n\nA user's own notes on the incident register."}`)["document_id"].(string)

	rep := runRealDrain(t, d, ctx, cfg, `{"mode":"drain","scope":"tenant","queue":"documents","write":"propose"}`)
	if rep["status"] != "failed" {
		t.Fatalf("status = %v, want failed", rep["status"])
	}
	if f, _ := rep["failure"].(string); !strings.Contains(f, "holds user-scope messages") || !strings.Contains(f, "declare the channel at scope tenant") {
		t.Errorf("failure = %q, want it to name the queue's scope and the declaration to fix", f)
	}
	msgs, _ := rep["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the drain stopped at the first message", rep["messages"])
	}
	if m := msgs[0].(map[string]any); m["document_id"] != doc || m["outcome"] != "scope_mismatch" || m["acked"] != false {
		t.Errorf("message = %v, want it refused as scope_mismatch and left unacked", m)
	}
	if q, _ := rep["queue"].(map[string]any); q["acked"] != float64(0) {
		t.Errorf("queue = %v, want nothing acked", q)
	}
	if docs, _ := rep["documents"].([]any); len(docs) != 0 {
		t.Errorf("documents = %v, want no pass over a document of another scope", docs)
	}

	// The next read from the committed cursor still finds it.
	res, err := (&builtin.Channel{Store: d.Store, Bus: d.Bus}).Execute(ctx, json.RawMessage(
		`{"op":"await","channels":["documents/ingested"],"mode":"any","max_messages":1,"wait_ms":0}`))
	if err != nil || res.IsError {
		t.Fatalf("await: %v %s", err, res.Text)
	}
	var aw struct {
		Results map[string]struct {
			Messages []struct {
				Value map[string]any `json:"value"`
			} `json:"messages"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &aw); err != nil {
		t.Fatalf("await result: %v\n%s", err, res.Text)
	}
	left := aw.Results["documents/ingested"].Messages
	if len(left) != 1 || left[0].Value["document_id"] != doc {
		t.Errorf("the user queue holds %v, want the unacked message for %s", left, doc)
	}
}
