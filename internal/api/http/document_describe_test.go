package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// visionProvider is a stub that records the request it was handed, so a test can
// assert the IMAGE actually reached the provider rather than only that a
// description came back.
type visionProvider struct {
	mu      sync.Mutex
	reqs    []providers.Request
	reply   string
	canSee  bool
	failErr string
	// stopReason overrides the terminal EventDone reason, so a test can reproduce a
	// turn that ran out of token budget before saying anything.
	stopReason string
}

func (v *visionProvider) ID() string                    { return "vis" }
func (v *visionProvider) Probe(_ context.Context) error { return nil }
func (v *visionProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"vm"}, nil
}
func (v *visionProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsVision: v.canSee}
}
func (v *visionProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	v.mu.Lock()
	v.reqs = append(v.reqs, req)
	v.mu.Unlock()
	ch := make(chan providers.Event, 2)
	if v.failErr != "" {
		ch <- providers.Event{Type: providers.EventError, Error: v.failErr}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: v.reply}
	}
	stop := v.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: stop}
	close(ch)
	return ch, nil
}
func (v *visionProvider) calls() []providers.Request {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]providers.Request(nil), v.reqs...)
}

// describeFixture stands up a server whose `middle` tier resolves to the given
// provider, plus a document scope holding one captioned image chunk.
func describeFixture(t *testing.T, prov *visionProvider) (*Server, *builtin.Document, context.Context, string) {
	t.Helper()
	cfg := &config.Config{
		ProviderPriority: []string{"vis"},
		Tiers:            map[string][]config.TierCandidate{"middle": {{Provider: "vis", Model: "vm"}}},
		Concurrency:      config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "desc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	res := resolve.NewResolver([]string{"vis"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "vis", Model: "vm"}},
	})
	res.SetReachable("vis", true, []string{"vm"}, "")
	srv.SetResolver(res)
	srv.sqlMem = mgr

	// Author one captioned image chunk with real bytes, through the tool, so the
	// fixture exercises the same write path an agent would.
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1"})
	doc := &builtin.Document{Store: st, SqlMem: mgr}

	res1, err := doc.Execute(ctx, json.RawMessage(`{"op":"create_document","scope":"user","title":"Shots"}`))
	if err != nil || res1.IsError {
		t.Fatalf("create_document: %v %s", err, res1.Text)
	}
	var cd map[string]any
	_ = json.Unmarshal([]byte(res1.Text), &cd)
	docID, _ := cd["document_id"].(string)

	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "scope": "user", "document_id": docID,
		"title": "Login", "type": "image", "body": "the login screen",
	})
	res2, err := doc.Execute(ctx, body)
	if err != nil || res2.IsError {
		t.Fatalf("create_chunk: %v %s", err, res2.Text)
	}
	var cc map[string]any
	_ = json.Unmarshal([]byte(res2.Text), &cc)
	chunkID, _ := cc["id"].(string)

	png, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")
	sa, _ := json.Marshal(map[string]any{
		"op": "set_asset", "scope": "user", "id": chunkID,
		"media_type": "image/png", "data": base64.StdEncoding.EncodeToString(png),
	})
	if r3, err := doc.Execute(ctx, sa); err != nil || r3.IsError {
		t.Fatalf("set_asset: %v %s", err, r3.Text)
	}
	return srv, doc, ctx, chunkID
}

func postDescribe(t *testing.T, srv *Server, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/_document/describe_images?scope=user&scope_id=u1"+query, nil)
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestDescribeImages_DryRunMakesNoVisionCall — the default must be a preview. A
// bare POST that swept a thousand images would be a bill nobody approved, which is
// the same reason the phase-2 backfill defaults to dry_run.
func TestDescribeImages_DryRunMakesNoVisionCall(t *testing.T) {
	prov := &visionProvider{reply: "a form with two fields", canSee: true}
	srv, _, _, _ := describeFixture(t, prov)

	code, out := postDescribe(t, srv, "")
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}
	if out["dry_run"] != true {
		t.Errorf("dry_run should default TRUE, got %v", out["dry_run"])
	}
	if n, _ := out["candidates"].(float64); n != 1 {
		t.Errorf("candidates = %v, want 1", out["candidates"])
	}
	if len(prov.calls()) != 0 {
		t.Errorf("a dry run made %d vision call(s)", len(prov.calls()))
	}
}

// TestDescribeImages_PersistsDescribesAndSendsTheImage is the phase-4b happy path.
// It asserts the image REACHED the provider, because a describe pass that sent only
// the caption would still return a plausible description.
func TestDescribeImages_PersistsDescribesAndSendsTheImage(t *testing.T) {
	prov := &visionProvider{reply: "a form with two text fields", canSee: true}
	srv, doc, ctx, chunkID := describeFixture(t, prov)

	code, out := postDescribe(t, srv, "&dry_run=false")
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}
	if n, _ := out["described"].(float64); n != 1 {
		t.Fatalf("described = %v, want 1 (failed=%v, first_failure=%v)",
			out["described"], out["failed"], out["first_failure"])
	}

	calls := prov.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 vision call, got %d", len(calls))
	}
	if !providers.RequestHasImage(calls[0]) {
		t.Error("the request carried NO image block — the model was asked to describe nothing")
	}
	// The caption is handed to the model so it can add what the author omitted.
	var sawInstruction bool
	for _, c := range calls[0].Messages[0].Content {
		if c.Type == "text" && strings.Contains(c.Text, "the login screen") {
			sawInstruction = true
		}
	}
	if !sawInstruction {
		t.Error("the caption was not given to the model, so it will tend to restate it")
	}

	// Persisted AND reported back through get_asset.
	ga, _ := json.Marshal(map[string]any{"op": "get_asset", "scope": "user", "id": chunkID})
	res, err := doc.Execute(ctx, ga)
	if err != nil || res.IsError {
		t.Fatalf("get_asset: %v %s", err, res.Text)
	}
	var meta map[string]any
	_ = json.Unmarshal([]byte(res.Text), &meta)
	if meta["description"] != "a form with two text fields" {
		t.Errorf("description not persisted: %v", meta["description"])
	}
	if meta["described"] != true {
		t.Errorf("described flag not set: %v", meta["described"])
	}
}

// TestDescribeImages_SecondPassSkipsDescribedImages — resumability. described_at
// drops the asset out of the candidate set, so re-invoking resumes rather than
// re-paying for work already done.
func TestDescribeImages_SecondPassSkipsDescribedImages(t *testing.T) {
	prov := &visionProvider{reply: "a form", canSee: true}
	srv, _, _, _ := describeFixture(t, prov)

	if code, out := postDescribe(t, srv, "&dry_run=false"); code != 200 {
		t.Fatalf("first pass: %d %v", code, out)
	}
	code, out := postDescribe(t, srv, "&dry_run=false")
	if code != 200 {
		t.Fatalf("second pass: %d %v", code, out)
	}
	if n, _ := out["candidates"].(float64); n != 0 {
		t.Errorf("candidates = %v after describing everything, want 0", out["candidates"])
	}
	if len(prov.calls()) != 1 {
		t.Errorf("the second pass re-described an image: %d total calls", len(prov.calls()))
	}
}

// TestDescribeImages_RefusesATextOnlyModelBeforeCalling — the RFC AT gate. A
// text-only model handed an image returns an opaque provider 400, and a sweep that
// reported that per image would read as a network fault rather than a misconfigured
// tier. So refuse once, up front, and name the tier.
func TestDescribeImages_RefusesATextOnlyModelBeforeCalling(t *testing.T) {
	prov := &visionProvider{reply: "should never be asked", canSee: false}
	srv, _, _, _ := describeFixture(t, prov)

	// 400, not 503: the caller CAN fix this by naming another tier, so it is not a
	// retry-unchanged condition. A deployment with no resolver at all is the 503.
	code, out := postDescribe(t, srv, "&dry_run=false")
	if code != 400 {
		t.Fatalf("status %d, want 400: %v", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "cannot accept images") || !strings.Contains(msg, "middle") {
		t.Errorf("refusal should name the problem and the tier, got %q", msg)
	}
	if len(prov.calls()) != 0 {
		t.Errorf("refused but still made %d call(s)", len(prov.calls()))
	}
}

// TestDescribeImages_FailedCallLeavesTheImageRetryable is the distinction the
// described_at column exists for. A transient model failure must NOT stamp
// described_at, or the image is silently retired from every future pass — the exact
// shape of bug that looks correct from every surface an operator would check.
func TestDescribeImages_FailedCallLeavesTheImageRetryable(t *testing.T) {
	prov := &visionProvider{failErr: "model cold: unexpected EOF", canSee: true}
	srv, _, _, _ := describeFixture(t, prov)

	code, out := postDescribe(t, srv, "&dry_run=false")
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}
	if n, _ := out["failed"].(float64); n != 1 {
		t.Errorf("failed = %v, want 1", out["failed"])
	}
	if ff, _ := out["first_failure"].(string); !strings.Contains(ff, "unexpected EOF") {
		t.Errorf("first_failure should carry the diagnostic, got %q", ff)
	}
	// Still a candidate: the next pass retries it.
	prov.failErr, prov.reply = "", "described on the retry"
	_, out2 := postDescribe(t, srv, "&dry_run=false")
	if n, _ := out2["candidates"].(float64); n != 1 {
		t.Fatalf("a FAILED image was retired from the candidate set (candidates=%v)", out2["candidates"])
	}
	if n, _ := out2["described"].(float64); n != 1 {
		t.Errorf("the retry did not describe it: %v", out2)
	}
}

// TestDescribeImages_EmptyAnswerIsMarkedExamined — the other half of that
// distinction. A model that LOOKED and produced nothing must not be re-asked
// forever, so it is stamped even though the description is empty.
func TestDescribeImages_EmptyAnswerIsMarkedExamined(t *testing.T) {
	prov := &visionProvider{reply: "   ", canSee: true}
	srv, _, _, _ := describeFixture(t, prov)

	_, out := postDescribe(t, srv, "&dry_run=false")
	if n, _ := out["empty"].(float64); n != 1 {
		t.Fatalf("empty = %v, want 1: %v", out["empty"], out)
	}
	_, out2 := postDescribe(t, srv, "&dry_run=false")
	if n, _ := out2["candidates"].(float64); n != 0 {
		t.Errorf("an examined-but-empty image stayed a candidate (%v) — the pass would "+
			"keep paying for it forever", out2["candidates"])
	}
}

// TestDescribeImages_DescriptionReachesTheSearchIndex — the re-embed. Without it the
// description sits in the database, get_asset reports the image as described, and it
// is still unsearchable: correct-looking from every surface, and useless.
func TestDescribeImages_DescriptionReachesTheSearchIndex(t *testing.T) {
	prov := &visionProvider{reply: "two text fields", canSee: true}
	srv, _, _, chunkID := describeFixture(t, prov)

	emb := &recordingHTTPEmbedder{}
	srv.embedder = emb

	if code, out := postDescribe(t, srv, "&dry_run=false"); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	var found string
	for _, txt := range emb.seen() {
		if strings.Contains(txt, "two text fields") {
			found = txt
		}
	}
	if found == "" {
		t.Fatalf("the description was never embedded, so the image is still unsearchable; "+
			"embedder saw %q", emb.seen())
	}
	if !strings.Contains(found, "the login screen") {
		t.Errorf("the re-embed dropped the caption: %q", found)
	}
	_ = chunkID
}

// recordingHTTPEmbedder captures the texts handed to the embedder.
type recordingHTTPEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (r *recordingHTTPEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.texts = append(r.texts, texts...)
	r.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}
func (r *recordingHTTPEmbedder) Provider() string { return "fake" }
func (r *recordingHTTPEmbedder) Model() string    { return "fm" }
func (r *recordingHTTPEmbedder) Dimension() int   { return 3 }
func (r *recordingHTTPEmbedder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

// TestDescribeImages_TruncatedAnswerIsRetryableNotEmpty is the regression test for a
// bug found by running the pass against a live deployment.
//
// describeMaxTokens was 300 and qwen3.6 is a thinking model: it emits its reasoning
// trace FIRST, which consumed the entire budget (done_reason=length, eval_count=300)
// and produced ZERO characters of description. The pass recorded that as an
// answered-empty image and stamped described_at — so a CODE bug became a permanent
// fact about the data, and no re-run would ever revisit it.
//
// An empty answer that stopped at the ceiling is a configuration problem the operator
// can fix, so it must be reported as a FAILURE and left retryable.
func TestDescribeImages_TruncatedAnswerIsRetryableNotEmpty(t *testing.T) {
	prov := &visionProvider{reply: "", canSee: true, stopReason: "max_tokens"}
	srv, _, _, _ := describeFixture(t, prov)

	_, out := postDescribe(t, srv, "&dry_run=false")
	if n, _ := out["empty"].(float64); n != 0 {
		t.Errorf("empty = %v, want 0 — a truncated turn is not an answered-empty image, "+
			"and marking it examined retires it forever", out["empty"])
	}
	if n, _ := out["failed"].(float64); n != 1 {
		t.Fatalf("failed = %v, want 1: %v", out["failed"], out)
	}
	if ff, _ := out["first_failure"].(string); !strings.Contains(ff, "ceiling") {
		t.Errorf("first_failure should explain the token ceiling, got %q", ff)
	}
	// Still a candidate: the next pass retries it.
	prov.stopReason, prov.reply = "", "a described image"
	_, out2 := postDescribe(t, srv, "&dry_run=false")
	if n, _ := out2["candidates"].(float64); n != 1 {
		t.Fatalf("a TRUNCATED image was retired from the candidate set (candidates=%v)",
			out2["candidates"])
	}
	if n, _ := out2["described"].(float64); n != 1 {
		t.Errorf("the retry did not describe it: %v", out2)
	}
}

// TestDescribeImages_BudgetLeavesRoomForAThinkingTrace pins the ceiling itself. A
// thinking model's trace measured ~1281 characters (~320 tokens) BEFORE any content,
// so a budget near that is the bug this constant exists to avoid.
func TestDescribeImages_BudgetLeavesRoomForAThinkingTrace(t *testing.T) {
	if describeMaxTokens < 1000 {
		t.Errorf("describeMaxTokens = %d — a thinking model emits its reasoning trace "+
			"first (~320 tokens measured) and will consume the whole budget before "+
			"producing any description", describeMaxTokens)
	}
}
