package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeLayerBackend implements BOTH memrank.Backend (minimally) and
// memrank.MemoryLayer + memrank.Capable, so the Memory tool routes add/recall
// to it. Only the layer methods are exercised; the KV methods are stubs that
// fail loudly if the layer test accidentally hits a KV path.
type fakeLayerBackend struct {
	addCalls    []fakeAdd
	recallQuery memrank.RecallQuery
	recallOut   []memrank.RecallFact
}

type fakeAdd struct {
	scope   store.MemoryScope
	scopeID string
	msgs    []memrank.LayerMessage
	opts    memrank.AddOptions
}

func (f *fakeLayerBackend) Capabilities() memrank.Capabilities {
	return memrank.Capabilities{KV: true, VectorSearch: true, Stats: true, MemoryLayer: true}
}

func (f *fakeLayerBackend) Add(_ context.Context, scope store.MemoryScope, scopeID string, msgs []memrank.LayerMessage, opts memrank.AddOptions) (memrank.AddResult, error) {
	f.addCalls = append(f.addCalls, fakeAdd{scope, scopeID, msgs, opts})
	return memrank.AddResult{Status: memrank.AddPending, EventID: "evt-123"}, nil
}

func (f *fakeLayerBackend) Recall(_ context.Context, _ store.MemoryScope, _ string, q memrank.RecallQuery) (memrank.RecallResult, error) {
	f.recallQuery = q
	return memrank.RecallResult{Facts: f.recallOut}, nil
}

// KV stubs — present so fakeLayerBackend satisfies memrank.Backend; a layer
// test must never reach these.
func (f *fakeLayerBackend) Get(context.Context, store.MemoryScope, string, string) (store.MemoryEntry, error) {
	panic("fakeLayerBackend.Get must not be called in a layer test")
}
func (f *fakeLayerBackend) Set(context.Context, store.MemoryScope, string, string, json.RawMessage, memrank.SetOptions) (memrank.SetResult, error) {
	panic("fakeLayerBackend.Set must not be called in a layer test")
}
func (f *fakeLayerBackend) Delete(context.Context, store.MemoryScope, string, string) (bool, error) {
	panic("fakeLayerBackend.Delete must not be called in a layer test")
}
func (f *fakeLayerBackend) List(context.Context, store.MemoryScope, string, string, int) ([]store.MemoryEntry, bool, error) {
	panic("fakeLayerBackend.List must not be called in a layer test")
}
func (f *fakeLayerBackend) Search(context.Context, store.MemoryScope, string, memrank.SearchQuery, memrank.RankConfig, memrank.DedupConfig) (memrank.SearchResult, error) {
	panic("fakeLayerBackend.Search must not be called in a layer test")
}
func (f *fakeLayerBackend) Stats(context.Context, store.MemoryScope) (store.MemoryEmbedStats, error) {
	panic("fakeLayerBackend.Stats must not be called in a layer test")
}

// The default in-process backend is NOT a memory layer, so add/recall must
// refuse with capability_unsupported — the fail-closed posture RFC K
// mandates (never a panic, never a silent no-op).
func TestMemoryTool_AddRecall_RefuseOnNonLayerBackend(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t) // default in-process backend
	defer cleanup()

	for _, in := range []string{
		`{"op":"add","scope":"user","messages":[{"role":"user","content":"hi"}]}`,
		`{"op":"recall","scope":"user","query":"anything"}`,
	} {
		res, err := tool.Execute(ctx, json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s: unexpected go error: %v", in, err)
		}
		if !res.IsError {
			t.Fatalf("%s: expected is_error refusal on a non-layer backend; got %s", in, res.Text)
		}
		if res.Text != store.ErrCapabilityUnsupported.Msg {
			t.Errorf("%s: refusal should be the typed capability_unsupported message %q; got %s", in, store.ErrCapabilityUnsupported.Msg, res.Text)
		}
	}
}

// add must bound its ingest by MaxValueBytes, exactly as set does for a value.
// Without the cap an agent could POST an arbitrarily large messages array to
// the (possibly remote) layer backend — unbounded egress, and the bytes are
// never charged to the per-scope quota (async-extracted facts, RFC §8). The
// oversized add must refuse BEFORE the backend is called.
func TestMemoryTool_Add_RefusesOversizedIngest(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t) // MaxValueBytes = 65536
	defer cleanup()
	fake := &fakeLayerBackend{}
	tool.Backend = fake

	big := strings.Repeat("x", tool.MaxValueBytes+1)
	in := `{"op":"add","scope":"user","messages":[{"role":"user","content":"` + big + `"}]}`
	res, err := tool.Execute(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("oversized add should be refused; got non-error result %s", res.Text)
	}
	if !strings.Contains(res.Text, "exceeds max") {
		t.Errorf("refusal should name the byte cap; got %s", res.Text)
	}
	if len(fake.addCalls) != 0 {
		t.Errorf("backend.Add must NOT be called for an oversized ingest; got %d calls", len(fake.addCalls))
	}
}

// With a MemoryLayer-capable backend wired, add routes through to it: the
// messages + infer-default (true) reach the backend and the async pending
// status + event id surface in the tool result.
func TestMemoryTool_Add_RoutesToLayerBackend(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	fake := &fakeLayerBackend{}
	tool.Backend = fake // operator-default backend is the layer

	res, err := tool.Execute(ctx, json.RawMessage(
		`{"op":"add","scope":"user","messages":[{"role":"user","content":"I prefer dark mode"},{"role":"assistant","content":"noted"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("add is_error: %s", res.Text)
	}
	if len(fake.addCalls) != 1 {
		t.Fatalf("backend.Add called %d times, want 1", len(fake.addCalls))
	}
	call := fake.addCalls[0]
	if len(call.msgs) != 2 || call.msgs[0].Content != "I prefer dark mode" {
		t.Errorf("messages not threaded through: %+v", call.msgs)
	}
	if !call.opts.Infer {
		t.Error("infer should default to true (the memory-layer paradigm)")
	}
	if !strings.Contains(res.Text, `"status":"pending"`) || !strings.Contains(res.Text, "evt-123") {
		t.Errorf("add result should surface pending status + event id; got %s", res.Text)
	}
}

// infer:false opts into verbatim storage — the flag must reach the backend.
func TestMemoryTool_Add_InferFalseThreadsThrough(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	fake := &fakeLayerBackend{}
	tool.Backend = fake

	if res, err := tool.Execute(ctx, json.RawMessage(
		`{"op":"add","scope":"agent","infer":false,"messages":[{"role":"user","content":"raw note"}]}`)); err != nil || res.IsError {
		t.Fatalf("add infer:false failed: err=%v res=%+v", err, res)
	}
	if fake.addCalls[0].opts.Infer {
		t.Error("infer:false should disable extraction, but Infer was true at the backend")
	}
}

// recall threads query + top_k + threshold and renders the returned facts.
func TestMemoryTool_Recall_RoutesAndRenders(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	fake := &fakeLayerBackend{recallOut: []memrank.RecallFact{
		{ID: "uuid-1", Memory: "user prefers dark mode", Score: 0.91},
		{ID: "uuid-2", Memory: "user is in Berlin", Score: 0.77},
	}}
	tool.Backend = fake

	res, err := tool.Execute(ctx, json.RawMessage(
		`{"op":"recall","scope":"user","query":"ui preferences","top_k":5,"threshold":0.5}`))
	if err != nil || res.IsError {
		t.Fatalf("recall failed: err=%v res=%+v", err, res)
	}
	if fake.recallQuery.Query != "ui preferences" || fake.recallQuery.TopK != 5 || fake.recallQuery.Threshold != 0.5 {
		t.Errorf("recall query not threaded through: %+v", fake.recallQuery)
	}
	if !strings.Contains(res.Text, "uuid-1") || !strings.Contains(res.Text, "user prefers dark mode") {
		t.Errorf("recall result missing facts: %s", res.Text)
	}
}

// add with no messages is a request error (not a capability error) once the
// backend IS a layer.
func TestMemoryTool_Add_RejectsEmptyMessages(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	tool.Backend = &fakeLayerBackend{}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"add","scope":"user","messages":[]}`))
	if !res.IsError || !strings.Contains(res.Text, "messages") {
		t.Errorf("empty messages should be refused with a messages error; got %s", res.Text)
	}
}
