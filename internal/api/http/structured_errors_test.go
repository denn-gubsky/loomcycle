package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// quotaTool returns the store's typed quota error, as the Memory tool does.
type quotaTool struct{}

func (quotaTool) Name() string                 { return "Memory" }
func (quotaTool) Description() string          { return "stores things" }
func (quotaTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (quotaTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{}, fmt.Errorf("memory set: %w", store.ErrMemoryQuotaExceeded)
}

// The dispatcher every run gets classifies a tool's typed Go error, so the model
// reads the same category and next step the transports give that error. The
// classifier is injected by the server; this pins that it is.
func TestNewDispatcher_ClassifiesAToolsTypedError(t *testing.T) {
	srv := New(&config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100}},
		&stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	res := srv.newDispatcher([]tools.Tool{quotaTool{}}).Execute(context.Background(), "Memory", json.RawMessage(`{"op":"set"}`))
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryBusiness || res.Error.Retryable {
		t.Errorf("result = %+v (error %+v); want the quota error classified as business, not retryable", res, res.Error)
	}
}

// A builtin called from outside a run goes through the same dispatcher a call
// inside a run does, so its failure carries the same structure: this crosses
// the whole seam — execBuiltin, the connector result, the HTTP refusal
// envelope — with the real Context help. It used to call the tool directly,
// so an unknown argument ran silently and a failure was bare text.
func TestConnectorBuiltin_ARefusalCarriesItsStructureOverHTTP(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100}}
	cfg.Env.AuthToken = "test-token"
	srv := New(cfg, &stubResolver{}, []tools.Tool{&builtin.Document{}, &builtin.Context{Help: set}},
		concurrency.New(1, 1, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp := postAdmin(t, ts, "/v1/_document", `{"op":"create_chunk","document_id":"d1","title":"T","text":"hello"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 422 {
		t.Fatalf("status = %d, want 422; body=%s", resp.StatusCode, raw)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if env["code"] != "tool_refused" || env["errorCategory"] != "validation" || env["isRetryable"] != false {
		t.Errorf("envelope = %s; want a non-retryable validation refusal", raw)
	}
	if env["description"] == nil {
		t.Errorf("no next step in the envelope: %s", raw)
	}
	cf, ok := env["correctCallFormat"].(map[string]any)
	if !ok || cf["tool"] != "Document" || cf["op"] != "create_chunk" || cf["example"] == nil {
		t.Errorf("correctCallFormat = %v; want the create_chunk example", env["correctCallFormat"])
	}
	// A caller outside a run has no run history to gate a help hint on, so its
	// dispatcher attaches none.
	if strings.Contains(string(raw), "You have not read the help") {
		t.Errorf("an off-run refusal carries a help hint: %s", raw)
	}
}

// The crossing for help hints: the dispatcher the SERVER builds for a run, the
// real Context, and the real help corpus. A run that skipped Document's help
// gets the create_chunk article with its first shape failure — here the
// measured case, `parent` for `parent_id` — and none once it reads the help.
func TestRunDispatcher_AShapeFailureCarriesTheSkippedArticle(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	art, ok := set.Get("Document/create_chunk")
	if !ok {
		t.Fatal("no Document/create_chunk article")
	}
	srv := New(&config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100}},
		&stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	d := srv.newDispatcher([]tools.Tool{&builtin.Document{}, &builtin.Context{Help: set}})
	ctx := context.Background()

	res := d.Execute(ctx, "Document", json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"T","parent":"r"}`))
	if !res.IsError || res.Error == nil {
		t.Fatalf("want a refusal, got %+v", res)
	}
	if !strings.Contains(res.Error.Hint, strings.TrimSpace(art.Content)) {
		t.Errorf("the refusal does not carry the create_chunk article; hint = %q", res.Error.Hint)
	}

	if r := d.Execute(ctx, "Context", json.RawMessage(`{"op":"help","topic":"Document"}`)); r.IsError {
		t.Fatalf("help call: %s", r.Text)
	}
	res = d.Execute(ctx, "Document", json.RawMessage(`{"op":"get_document","parent":"r"}`))
	if !res.IsError || res.Error == nil || res.Error.Hint != "" {
		t.Errorf("after reading Document's help the failure still carries a hint: %+v", res.Error)
	}
}
