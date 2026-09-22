package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func schemaWith(classes map[string]string) map[string]any {
	props := map[string]any{}
	for k, c := range classes {
		p := map[string]any{"type": "string"}
		if c != "" {
			p[retentionKeyword] = c
		}
		props[k] = p
	}
	return map[string]any{"type": "object", "properties": props}
}

// ⚠️ THE DEFAULT IS CORE, and it is the decision that keeps this opt-in.
//
// An agent whose schema predates this feature — or whose author simply did not
// think about retention — must not silently start losing state on upgrade. The
// cost is that such a run gets no relief from Σ growth and reports exhaustion
// instead, which is the honest failure rather than a quiet one.
func TestRetentionOf_UndeclaredIsCore(t *testing.T) {
	s := schemaWith(map[string]string{
		"spine": "", "notes": RetentionScratch, "calc": RetentionDerived, "bogus": "nonsense",
	})
	for key, want := range map[string]string{
		"spine":   RetentionCore, // undeclared
		"notes":   RetentionScratch,
		"calc":    RetentionDerived,
		"bogus":   RetentionCore, // unrecognised value is not a licence to drop
		"missing": RetentionCore, // not in the schema at all
	} {
		if got := retentionOf(s, key); got != want {
			t.Errorf("retentionOf(%q) = %q, want %q", key, got, want)
		}
	}
	// And with no schema at all, nothing is evictable.
	if got := retentionOf(nil, "anything"); got != RetentionCore {
		t.Errorf("with no schema, retentionOf = %q, want core", got)
	}
}

// Scratch goes before derived, and core never goes. The class is the operator's
// statement of what matters; recency only orders equals within it.
func TestSigmaEvictionPlan_ClassBeforeRecency(t *testing.T) {
	big := strings.Repeat("x", 400)
	sigma := map[string]any{
		"spine":  big, // core
		"calc":   big, // derived, written most recently
		"notes":  big, // scratch, written earliest
		"notes2": big, // scratch, written later
	}
	schema := schemaWith(map[string]string{
		"spine": RetentionCore, "calc": RetentionDerived,
		"notes": RetentionScratch, "notes2": RetentionScratch,
	})
	lastWritten := map[string]int{"notes": 1, "notes2": 5, "calc": 9, "spine": 2}

	// A budget that forces two evictions.
	plan := sigmaEvictionPlan(sigma, schema, lastWritten, sigmaTokens(sigma)/2)
	if len(plan) < 2 {
		t.Fatalf("plan = %v, want at least two evictions", plan)
	}
	// Both scratch keys must go before the derived one is touched...
	if plan[0] != "notes" {
		t.Errorf("first eviction = %q, want notes — scratch before derived, and "+
			"least-recently-written first within the class", plan[0])
	}
	if plan[1] != "notes2" {
		t.Errorf("second eviction = %q, want notes2", plan[1])
	}
	// ...and core must never appear.
	for _, k := range plan {
		if k == "spine" {
			t.Error("core was evicted — it is the task's spine and is never droppable")
		}
	}
}

// It stops as soon as Σ fits. Evicting more than necessary throws away context
// the run may still need; the cheapest eviction is the one not performed.
func TestSigmaEvictionPlan_StopsWhenItFits(t *testing.T) {
	big := strings.Repeat("x", 400)
	sigma := map[string]any{"a": big, "b": big, "c": big, "d": big}
	schema := schemaWith(map[string]string{
		"a": RetentionScratch, "b": RetentionScratch,
		"c": RetentionScratch, "d": RetentionScratch,
	})
	lw := map[string]int{"a": 1, "b": 2, "c": 3, "d": 4}

	// Budget fits three of the four → exactly one eviction.
	budget := sigmaTokens(sigma) * 3 / 4
	plan := sigmaEvictionPlan(sigma, schema, lw, budget)
	if len(plan) != 1 {
		t.Errorf("plan = %v, want exactly one eviction for a budget that fits three", plan)
	}
}

// An all-core Σ yields no plan — which is what makes the run report exhaustion
// instead of quietly doing nothing.
func TestSigmaEvictionPlan_AllCoreYieldsNothing(t *testing.T) {
	big := strings.Repeat("x", 400)
	sigma := map[string]any{"a": big, "b": big}
	schema := schemaWith(map[string]string{"a": RetentionCore, "b": ""})
	if plan := sigmaEvictionPlan(sigma, schema, map[string]int{}, 1); len(plan) != 0 {
		t.Errorf("plan = %v, want none — nothing is declared evictable", plan)
	}
}

// Already under budget → no plan at all, whatever the classes say.
func TestSigmaEvictionPlan_UnderBudgetEvictsNothing(t *testing.T) {
	sigma := map[string]any{"notes": "small"}
	schema := schemaWith(map[string]string{"notes": RetentionScratch})
	if plan := sigmaEvictionPlan(sigma, schema, map[string]int{}, 1_000_000); len(plan) != 0 {
		t.Errorf("plan = %v, want none — Σ already fits", plan)
	}
}

// The estimate must measure the SERIALISED form: that is what is put in front
// of the model, and a map's Go footprint is not what the provider bills.
func TestSigmaTokens_MeasuresTheSerialisedForm(t *testing.T) {
	small := map[string]any{"k": "v"}
	large := map[string]any{"k": strings.Repeat("x", 4000)}
	if sigmaTokens(large) <= sigmaTokens(small) {
		t.Error("a Σ with 4000 characters of value does not measure larger than a tiny one")
	}
	if got := sigmaTokens(large); got < 900 {
		t.Errorf("sigmaTokens = %d for ~4000 chars; the estimate is not reading the value", got)
	}
}

// growingStateProvider drives a stateful run whose Σ grows every step, and
// reports a footprint over the backstop so eviction is due.
type growingStateProvider struct {
	mu     sync.Mutex
	step   int
	inTok  int
	maxCtx int
}

func (p *growingStateProvider) ID() string                                   { return "growing-state" }
func (p *growingStateProvider) Probe(context.Context) error                  { return nil }
func (p *growingStateProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *growingStateProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: p.maxCtx}
}
func (p *growingStateProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	s := p.step
	p.step++
	p.mu.Unlock()

	// Each step writes one more scratch note plus keeps the spine.
	// Each note must be large enough that Σ actually crosses its share of the
	// window — the budget is half a 10000-token window, so Σ needs to exceed
	// ~20000 characters before eviction is due. A fixture below that exercises
	// the plumbing and proves nothing about the bound.
	patch := fmt.Sprintf(`{"spine":%q,"note%d":%q}`,
		"the task's spine", s, strings.Repeat("n", 9000))
	// ⚠️ An action is REQUIRED to continue: the loop treats a missing action as
	// terminal, so a fake without one runs exactly one step and Σ never grows.
	// That is how the first version of this test "proved" eviction does not
	// happen — it never reached a second step.
	out := fmt.Sprintf(
		`{"reasoning":"step %d","patch":%s,"action":{"tool":"Noop","input":{}},"done":false}`,
		s, patch)
	if s >= 4 {
		out = fmt.Sprintf(`{"reasoning":"step %d","patch":%s,"done":true,"final":"fin"}`, s, patch)
	}

	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
		ID: fmt.Sprintf("t%d", s), Name: emitStateToolName, Input: json.RawMessage(out)}}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use",
		Usage: &providers.Usage{InputTokens: p.inTok, MaxContextTokens: p.maxCtx}}
	close(ch)
	return ch, nil
}

// ⚠️ C2 END TO END — the mode that had no bound at all.
//
// The transcript is rebuilt from (Σ, observation) each step so it cannot
// accumulate, which is why the gate was never wired here. But Σ accumulates,
// and the whole of it is serialised into the prompt every step. Nothing
// measured it and nothing bounded it.
func TestRunStateful_EvictsScratchAndKeepsCore(t *testing.T) {
	// A footprint over the 80% backstop on every step.
	prov := &growingStateProvider{inTok: 9000, maxCtx: 10000}
	m := config.ContextModeStateful
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"spine": map[string]any{"type": "string", retentionKeyword: RetentionCore},
			"note0": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note1": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note2": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note3": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note4": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
		},
	}
	var mu sync.Mutex
	var evictions [][]string
	var finalState map[string]any
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		Context:    &config.Context{Mode: &m, StateSchema: schema},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextState && ev.ContextState != nil {
				mu.Lock()
				if len(ev.ContextState.Evicted) > 0 {
					evictions = append(evictions, ev.ContextState.Evicted)
				}
				finalState = ev.ContextState.State
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()

	if len(evictions) == 0 {
		t.Fatal("Σ grew past the backstop and nothing was evicted — stateful still " +
			"has no bound in the dimension that actually grows")
	}
	// The spine must survive every eviction.
	for _, batch := range evictions {
		for _, k := range batch {
			if k == "spine" {
				t.Error("the core key was evicted — core is never droppable")
			}
			if !strings.HasPrefix(k, "note") {
				t.Errorf("evicted %q, which is not declared scratch", k)
			}
		}
	}
	if finalState != nil {
		if _, ok := finalState["spine"]; !ok {
			t.Error("the run finished without its core key — eviction removed the task's spine")
		}
	}
	if res.State == nil {
		t.Error("the result carries no state")
	}
}

// The other direction: a run comfortably inside its window must not evict.
// Losing state for no reason is the failure this introduces rather than fixes.
func TestRunStateful_NoEvictionBelowTheBackstop(t *testing.T) {
	prov := &growingStateProvider{inTok: 100, maxCtx: 200000} // 0.05%
	m := config.ContextModeStateful
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"spine": map[string]any{"type": "string", retentionKeyword: RetentionCore},
			"note0": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note1": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note2": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note3": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
			"note4": map[string]any{"type": "string", retentionKeyword: RetentionScratch},
		},
	}
	var mu sync.Mutex
	evicted := 0
	if _, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		Context:    &config.Context{Mode: &m, StateSchema: schema},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextState && ev.ContextState != nil {
				mu.Lock()
				evicted += len(ev.ContextState.Evicted)
				mu.Unlock()
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if evicted != 0 {
		t.Errorf("evicted %d key(s) at 0.05%% of the window — state was lost for nothing", evicted)
	}
}

// ⚠️ EVICTED Σ MUST BE BANKED BEFORE IT IS DROPPED.
//
// Σ is the run's working memory. Dropping it to save prompt space and not
// banking it trades a context problem for a data-loss one — and the recap path
// already learned this expensively: its harvest sat ABOVE the measurement, so
// declined distillations banked spans they then kept, while a successful one
// could drop content nothing had recorded.
//
// The ordering is what the test pins: the bank must see the value, which is
// only possible before the delete.
func TestRunStateful_EvictedStateIsBankedBeforeItIsDropped(t *testing.T) {
	prov := &growingStateProvider{inTok: 9000, maxCtx: 10000}
	m := config.ContextModeStateful
	props := map[string]any{
		"spine": map[string]any{"type": "string", retentionKeyword: RetentionCore},
	}
	for i := 0; i < 5; i++ {
		props[fmt.Sprintf("note%d", i)] = map[string]any{
			"type": "string", retentionKeyword: RetentionScratch}
	}
	var mu sync.Mutex
	var banked []string
	_, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		Context: &config.Context{Mode: &m, HarvestToMemory: cptr(true),
			StateSchema: map[string]any{"type": "object", "properties": props}},
		BankCompactedSpan: func(_ context.Context, msgs []providers.Message) (string, error) {
			mu.Lock()
			for _, msg := range msgs {
				for _, c := range msg.Content {
					banked = append(banked, c.Text)
				}
			}
			mu.Unlock()
			return "mp_state", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()

	var stateBanks []string
	for _, b := range banked {
		if strings.HasPrefix(b, "Evicted state:") {
			stateBanks = append(stateBanks, b)
		}
	}
	if len(stateBanks) == 0 {
		t.Fatal("Σ entries were evicted and NOTHING was banked — the run silently lost " +
			"its own working memory to save prompt space")
	}
	// The banked payload must carry the VALUE, not just the key name: a later
	// recall has to be able to return what was dropped.
	joined := strings.Join(stateBanks, "\n")
	if !strings.Contains(joined, "nnnn") {
		t.Errorf("the bank recorded key names but not their content, so nothing can be "+
			"recovered: %.200q", joined)
	}
}

// oneBigWriteProvider reports a small footprint on every call, and writes one
// large scratch note on step 0 — the growth only the merge can see.
type oneBigWriteProvider struct {
	mu   sync.Mutex
	step int
}

func (p *oneBigWriteProvider) ID() string                                   { return "one-big-write" }
func (p *oneBigWriteProvider) Probe(context.Context) error                  { return nil }
func (p *oneBigWriteProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *oneBigWriteProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: 10000}
}
func (p *oneBigWriteProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	s := p.step
	p.step++
	p.mu.Unlock()
	out := fmt.Sprintf(`{"patch":{"spine":"task","note":%q},"action":{"tool":"Noop","input":{}}}`, strings.Repeat("n", 40000))
	if s > 0 {
		out = `{"patch":{},"done":true,"final":"fin"}`
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
		ID: fmt.Sprintf("t%d", s), Name: emitStateToolName, Input: json.RawMessage(out)}}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use",
		Usage: &providers.Usage{InputTokens: 100, MaxContextTokens: 10000}}
	close(ch)
	return ch, nil
}

// ⚠️ EVICTION WEIGHED THE REQUEST BEFORE THIS STEP'S PATCH. The footprint came
// from the call that produced the patch, so a single large write — here ~10000
// tokens into a 10000-token window — was never counted before the next
// request went out.
func TestRunStateful_ALargePatchIsWeighedBeforeTheNextRequest(t *testing.T) {
	m := config.ContextModeStateful
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"spine": map[string]any{"type": "string", retentionKeyword: RetentionCore},
		"note":  map[string]any{"type": "string", retentionKeyword: RetentionScratch},
	}}
	var mu sync.Mutex
	var evicted []string
	if _, err := Run(context.Background(), RunOptions{
		Provider: &oneBigWriteProvider{}, Model: "x",
		Tools:      []tools.Tool{noopTool{}},
		Dispatcher: tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:   steerSegs(),
		Context:    &config.Context{Mode: &m, StateSchema: schema},
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventContextState && ev.ContextState != nil {
				mu.Lock()
				evicted = append(evicted, ev.ContextState.Evicted...)
				mu.Unlock()
			}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != "note" {
		t.Errorf("evicted %v; a patch that fills the window was sent on unweighed", evicted)
	}
}
