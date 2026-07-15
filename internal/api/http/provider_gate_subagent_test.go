package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// multiResolver maps a resolved provider id to a DISTINCT provider instance, so
// a test can give a parent and its sub-agents different scripted providers while
// each still resolves to its own gate key. (stubResolver returns one instance for
// every id — fine when the gate key is all that matters, but the sub-agent tests
// need the parent + children to behave differently per call.)
type multiResolver struct{ byID map[string]providers.Provider }

func (r *multiResolver) Get(id string) (providers.Provider, error) {
	if p, ok := r.byID[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no provider %q", id)
}

// --- P2c ctx held-set primitives (pure unit tests) ---

// TestWithHeldProviderSlot_CopyOnAddIndependentSupersets locks the copy-on-add
// invariant that makes parallel_spawn safe: two siblings augmenting the SAME
// parent ctx each get an independent superset (a sibling never sees the other's
// provider), the parent's set is never mutated, and a grandchild sees its whole
// ancestry (nested depth). A shared-map implementation would fail every one of
// these.
func TestWithHeldProviderSlot_CopyOnAddIndependentSupersets(t *testing.T) {
	base := context.Background()
	if holdsProviderSlot(base, "P") {
		t.Fatal("empty ctx must hold no provider")
	}

	parent := withHeldProviderSlot(base, "P")
	if !holdsProviderSlot(parent, "P") {
		t.Fatal("parent ctx should hold P")
	}

	// Two siblings each add their OWN provider off the same parent ctx.
	childA := withHeldProviderSlot(parent, "A")
	childB := withHeldProviderSlot(parent, "B")

	if !holdsProviderSlot(childA, "P") || !holdsProviderSlot(childA, "A") {
		t.Error("childA should hold P and A")
	}
	if holdsProviderSlot(childA, "B") {
		t.Error("childA must NOT see sibling B's provider — copy-on-add leaked a shared map")
	}
	if !holdsProviderSlot(childB, "P") || !holdsProviderSlot(childB, "B") {
		t.Error("childB should hold P and B")
	}
	if holdsProviderSlot(childB, "A") {
		t.Error("childB must NOT see sibling A's provider — copy-on-add leaked a shared map")
	}

	// The parent's set must be untouched by either child's add.
	if holdsProviderSlot(parent, "A") || holdsProviderSlot(parent, "B") {
		t.Error("a child's add mutated the parent's held-set (copy-on-add violated)")
	}

	// Grandchild sees the full ancestry of its branch, not the sibling branch.
	grand := withHeldProviderSlot(childA, "G")
	for _, id := range []string{"P", "A", "G"} {
		if !holdsProviderSlot(grand, id) {
			t.Errorf("grandchild should hold ancestor %q", id)
		}
	}
	if holdsProviderSlot(grand, "B") {
		t.Error("grandchild must not see the sibling branch's provider B")
	}
}

// TestHeldSlotCtx_StampsOnlyRealCappedSlot proves the zero-overhead guarantee:
// heldSlotCtx enters a provider into the held-set ONLY for a real capped holder.
// An uncapped provider, a carve-out (noop) holder, and a nil holder all leave the
// ctx (and thus the held-set) untouched.
func TestHeldSlotCtx_StampsOnlyRealCappedSlot(t *testing.T) {
	gates := concurrency.NewProviderGates(map[string]int{"capped": 2}, 4, time.Second)
	s := &Server{providerGates: gates}
	base := context.Background()

	// Uncapped provider → not gated → nothing to skip → ctx unchanged.
	if got := s.heldSlotCtx(base, &providerSlot{release: func() {}, providerID: "uncapped"}); holdsProviderSlot(got, "uncapped") {
		t.Error("uncapped provider must NOT enter the held-set (zero-overhead)")
	}
	// Carve-out (noop) holder → holds no fresh gate → ctx unchanged.
	if got := s.heldSlotCtx(base, &providerSlot{noop: true, providerID: "capped"}); got != base {
		t.Error("carve-out holder must return ctx unchanged")
	}
	// nil holder → ctx unchanged.
	if got := s.heldSlotCtx(base, nil); got != base {
		t.Error("nil slot must return ctx unchanged")
	}
	// Real capped slot → provider held so descendants take the carve-out.
	if got := s.heldSlotCtx(base, &providerSlot{release: func() {}, providerID: "capped"}); !holdsProviderSlot(got, "capped") {
		t.Error("a real capped slot must enter the held-set")
	}
}

// TestAcquireProviderSlot_CarveOutWhenAncestorHolds proves the deadlock carve-out
// at the acquire seam: when the ctx already marks provider P as ancestor-held,
// acquireProviderSlot returns a noop holder WITHOUT touching P's gate (so a
// parent never awaits a child queued behind its own slot), and that holder is
// inert on both swap (must not acquire a fallback target) and release. Also
// covers the sub-agent fallback path: a REAL sub-agent slot swaps P->Q with no
// leak.
//
// Fail-before: drop the holdsProviderSlot check in acquireProviderSlot and the
// carve-out branch instead occupies P's gate (Active=1) — this test's Active==0
// assertion fails.
func TestAcquireProviderSlot_CarveOutWhenAncestorHolds(t *testing.T) {
	gates := concurrency.NewProviderGates(map[string]int{"P": 1, "Q": 1}, 2, time.Second)
	s := &Server{providerGates: gates}

	// Ancestor holds P → the child's acquire takes the carve-out.
	ctx := withHeldProviderSlot(context.Background(), "P")
	slot, err := s.acquireProviderSlot(ctx, "P")
	if err != nil {
		t.Fatalf("acquire under ancestor-held P: %v", err)
	}
	if !slot.noop {
		t.Fatal("expected a carve-out noop holder when an ancestor holds P")
	}
	if g := gates.Stats()["P"]; g.Active != 0 {
		t.Errorf("carve-out must not occupy P's gate; active=%d want 0", g.Active)
	}
	// A carve-out holder must stay ungated across a fallback — swap is inert.
	slot.swap(context.Background(), gates, "Q")
	if g := gates.Stats()["Q"]; g.Active != 0 {
		t.Errorf("carve-out swap acquired Q's gate; active=%d want 0 (must stay ungated)", g.Active)
	}
	slot.releaseCurrent() // inert; must not panic
	if g := gates.Stats()["P"]; g.Active != 0 {
		t.Errorf("P active=%d after carve-out release, want 0", g.Active)
	}

	// Sub-agent fallback: a REAL (non-carve-out) slot swaps P->Q with no leak.
	real, err := s.acquireProviderSlot(context.Background(), "P")
	if err != nil {
		t.Fatalf("acquire real P: %v", err)
	}
	real.swap(context.Background(), gates, "Q")
	if gP, gQ := gates.Stats()["P"], gates.Stats()["Q"]; gP.Active != 0 || gQ.Active != 1 {
		t.Fatalf("after sub-agent fallback P->Q: P active=%d (want 0) Q active=%d (want 1)", gP.Active, gQ.Active)
	}
	real.releaseCurrent()
	if gP, gQ := gates.Stats()["P"], gates.Stats()["Q"]; gP.Active != 0 || gQ.Active != 0 {
		t.Errorf("after release: P active=%d Q active=%d, want 0/0 (no leak)", gP.Active, gQ.Active)
	}
}

// --- End-to-end sub-agent gating through the full admission + spawn path ---

// TestProviderGate_SubAgentFanoutBatchesToCap is THE ats-filter goal (RFC BF §4):
// a code-js-style PARENT on an UNCAPPED provider fans out N same-provider workers
// resolving to a CAPPED provider; P2c gates the SUB-AGENTS so exactly `cap` of
// them call the provider at once, the rest queue and drain, the parent holds NO
// worker-provider slot, and every child completes with no deadlock.
//
// Fail-before: revert runSubAgent to P2b (sub-agents ungated) and the workers all
// run at once → peakInFlight == workers, not cap.
func TestProviderGate_SubAgentFanoutBatchesToCap(t *testing.T) {
	const cap = 2
	const workers = 5

	// Parent (uncapped provider): iter1 fans out N workers; iter2 wraps up.
	spawnEntries := make([]string, workers)
	for i := range spawnEntries {
		spawnEntries[i] = fmt.Sprintf(`{"name":"worker","prompt":"task-%d"}`, i)
	}
	parallelSpawnInput := `{"op":"parallel_spawn","spawns":[` + strings.Join(spawnEntries, ",") + `]}`
	parentProv := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_fanout", Name: "Agent", Input: json.RawMessage(parallelSpawnInput)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
			{
				{Type: providers.EventText, Text: "ats-filter done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
		},
	}
	// Workers (capped provider): each a single-iteration run; countingProvider
	// records the peak concurrent Call so we can assert it equals the cap exactly.
	workerProv := &countingProvider{delay: 40 * time.Millisecond}

	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		// max_concurrent_children high so the Agent tool's own fan-out semaphore
		// (default 4) is NOT the limiter — the PROVIDER gate must be the only cap.
		"ats-filter": {Provider: "codejs-prov", Model: "m", Tools: []string{"Agent"}, MaxConcurrentChildren: workers + 5},
		"worker":     {Provider: "ollama-prov", Model: "m", Tools: []string{}},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "fanout_gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(16, 16, 5*time.Second) // global never gates first
	gates := concurrency.NewProviderGates(map[string]int{"ollama-prov": cap}, workers, 5*time.Second)
	resolver := &multiResolver{byID: map[string]providers.Provider{"codejs-prov": parentProv, "ollama-prov": workerProv}}
	srv := New(cfg, resolver, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"ats-filter","agent_id":"ats-parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	// THE ASSERTION: at most `cap` workers ever called the provider at once.
	if peak := workerProv.peakInFlight(); peak != cap {
		t.Errorf("peak concurrent worker provider.Call = %d, want exactly cap=%d (P2c must gate sub-agents; P2b left them ungated → peak would be %d)", peak, cap, workers)
	}
	// The parent holds NO worker-provider slot, and the gate drains to zero.
	if g := gates.Stats()["ollama-prov"]; g.Active != 0 || g.Queued != 0 {
		t.Errorf("final ollama-prov gate state active=%d queued=%d, want 0/0", g.Active, g.Queued)
	}
	// All N workers actually ran (the rest queued behind the cap, then drained) —
	// each is a persisted sub-run under the parent.
	childRuns, err := st.ListRunsByParentAgentID(context.Background(), "ats-parent")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(childRuns) != workers {
		t.Errorf("expected %d worker sub-runs (all queued then drained), got %d", workers, len(childRuns))
	}
}

// TestProviderGate_SameProviderParentChildTakesCarveOut is the deadlock carve-out
// end-to-end (RFC BF §4): a PARENT resolving to provider P (cap 1) holds P's only
// slot, then spawns a child ALSO resolving to P. Without the carve-out the child
// would queue behind the parent's own slot and time out — the parent awaiting its
// own child is a self-deadlock. With the carve-out the child runs UNGATED and the
// parent completes.
//
// Fail-before (revert the carve-out): the child's acquire blocks then times out
// on P's cap BEFORE its sub-run row is created, so ListRunsByParentAgentID
// returns 0 children and the parent's tool_result carries the exhaustion error.
func TestProviderGate_SameProviderParentChildTakesCarveOut(t *testing.T) {
	// One provider instance serves parent + child (they share provider id "P", the
	// gate key). op=spawn is synchronous, so the three Calls arrive in a fixed
	// order: parent-iter1 (spawn) -> child-iter1 (text) -> parent-iter2 (final).
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_spawn", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go"}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
			{
				{Type: providers.EventText, Text: "child ran despite the cap"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
		},
	}

	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Provider: "P", Model: "m", Tools: []string{"Agent"}, SystemPrompt: "parent"},
		"child":  {Provider: "P", Model: "m", Tools: []string{}, SystemPrompt: "child"},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "carveout.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(8, 8, 5*time.Second)
	// cap 1: the parent takes the only slot. Short timeout so the FAIL-BEFORE path
	// (child gated) times out fast instead of hanging the test.
	gates := concurrency.NewProviderGates(map[string]int{"P": 1}, 1, 500*time.Millisecond)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"parent-carveout","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	bodyStr := string(body)

	// No deadlock: the parent completed its final turn.
	if !strings.Contains(bodyStr, "parent done") {
		t.Errorf("parent did not complete (deadlock?); body=\n%s", bodyStr)
	}
	// The child must NOT have been gated-and-timed-out.
	if strings.Contains(bodyStr, "provider concurrency exhausted") || strings.Contains(bodyStr, "provider gate") {
		t.Errorf("child was gated on the parent-held provider (carve-out missing); body=\n%s", bodyStr)
	}

	// The child actually ran: its sub-run row exists and its transcript carries
	// its output. In the fail-before path the acquire fails BEFORE the sub-run is
	// created, so this returns zero children.
	childRuns, err := st.ListRunsByParentAgentID(context.Background(), "parent-carveout")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(childRuns) != 1 {
		t.Fatalf("expected exactly 1 child run (carve-out let it run); got %d — the child was gated behind the parent's own slot", len(childRuns))
	}
	childTranscript, err := st.GetTranscript(context.Background(), childRuns[0].SessionID)
	if err != nil {
		t.Fatalf("GetTranscript(child): %v", err)
	}
	var sawChildText bool
	for _, ev := range childTranscript {
		if ev.Type == "text" && strings.Contains(string(ev.Payload), "child ran despite the cap") {
			sawChildText = true
		}
	}
	if !sawChildText {
		t.Errorf("child transcript missing its output — it did not run to completion")
	}

	// The parent's slot is released at run end; nothing leaked on the gate.
	if g := gates.Stats()["P"]; g.Active != 0 || g.Queued != 0 {
		t.Errorf("final P gate state active=%d queued=%d, want 0/0", g.Active, g.Queued)
	}
}

// TestProviderGate_SubAgentZeroOverheadWhenUnconfigured proves the whole P2c path
// is inert when no provider is capped: a parent fans out N workers, all run
// concurrently (peak > 1, bounded only by the Agent tool's fan-out cap), and no
// gates exist at any level.
func TestProviderGate_SubAgentZeroOverheadWhenUnconfigured(t *testing.T) {
	const workers = 4

	spawnEntries := make([]string, workers)
	for i := range spawnEntries {
		spawnEntries[i] = fmt.Sprintf(`{"name":"worker","prompt":"task-%d"}`, i)
	}
	parentProv := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_fanout", Name: "Agent", Input: json.RawMessage(`{"op":"parallel_spawn","spawns":[` + strings.Join(spawnEntries, ",") + `]}`)}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
			{
				{Type: providers.EventText, Text: "done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
			},
		},
	}
	workerProv := &countingProvider{delay: 30 * time.Millisecond}

	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Provider: "codejs-prov", Model: "m", Tools: []string{"Agent"}, MaxConcurrentChildren: workers},
		"worker": {Provider: "ollama-prov", Model: "m", Tools: []string{}},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "sub_zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(16, 16, 5*time.Second)
	// The default deployment: no caps → zero gates built.
	gates := concurrency.NewProviderGates(map[string]int{}, 16, time.Second)
	if gates.Len() != 0 {
		t.Fatalf("expected 0 gates, got %d", gates.Len())
	}
	resolver := &multiResolver{byID: map[string]providers.Provider{"codejs-prov": parentProv, "ollama-prov": workerProv}}
	srv := New(cfg, resolver, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// No gate anywhere: the workers overlap (peak > 1 proves they weren't
	// serialised) and no gate stats exist.
	if peak := workerProv.peakInFlight(); peak <= 1 {
		t.Errorf("peak = %d; with no gate all %d workers should overlap (peak > 1)", peak, workers)
	}
	if gates.Stats() != nil {
		t.Errorf("gates.Stats() = %v, want nil (no gates configured)", gates.Stats())
	}
}
