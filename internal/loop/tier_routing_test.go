package loop

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// textProvider returns a plain end_turn text reply and reports a configurable
// Local capability. If the stateful loop were (wrongly) entered it would ask this
// provider for an emit_state tool call and get none → a clear error, which is how
// the routing tests tell the recap/append path from the stateful path.
type textProvider struct{ local bool }

func (p *textProvider) ID() string                                   { return "text-test" }
func (p *textProvider) Probe(context.Context) error                  { return nil }
func (p *textProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *textProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, Local: p.local}
}
func (p *textProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func autoCtx() *config.Context {
	m := config.ContextModeAuto
	return &config.Context{Mode: &m}
}

func TestResolveAutoContextMode(t *testing.T) {
	cases := []struct {
		local, interactive bool
		want               string
	}{
		{local: true, interactive: false, want: config.ContextModeRecap},     // local → recap
		{local: false, interactive: false, want: config.ContextModeStateful}, // frontier → stateful
		{local: false, interactive: true, want: config.ContextModeRecap},     // interactive never stateful
		{local: true, interactive: true, want: config.ContextModeRecap},      // local + interactive → recap
	}
	for _, c := range cases {
		got := resolveAutoContextMode(autoCtx(), c.local, c.interactive)
		if got.Mode == nil || *got.Mode != c.want {
			t.Errorf("resolve(local=%v, interactive=%v) = %v, want %s", c.local, c.interactive, got.Mode, c.want)
		}
	}
	// It must NOT mutate the input (the shared agent def).
	in := autoCtx()
	_ = resolveAutoContextMode(in, false, false)
	if in.Mode == nil || *in.Mode != config.ContextModeAuto {
		t.Errorf("resolveAutoContextMode mutated its input: %v", in.Mode)
	}
}

// mode=auto on a FRONTIER provider resolves to stateful: the scripted emit_state
// run drives to completion (proving the stateful loop was entered).
func TestRun_Auto_FrontierResolvesToStateful(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{
		`{"patch":{"count":1},"done":true,"final":"stateful ran"}`,
	}} // Local defaults to false → frontier
	res, err := Run(context.Background(), RunOptions{
		Provider: prov, Model: "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    autoCtx(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "stateful ran" || res.State == nil {
		t.Errorf("auto+frontier did not take the stateful path: final=%q state=%v", res.FinalText, res.State)
	}
}

// mode=auto on a LOCAL provider resolves to recap (the append/recap path): a
// plain text provider completes normally. If it had wrongly taken the stateful
// path it would error asking for emit_state.
func TestRun_Auto_LocalResolvesToRecap(t *testing.T) {
	res, err := Run(context.Background(), RunOptions{
		Provider:   &textProvider{local: true},
		Model:      "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    autoCtx(),
	})
	if err != nil {
		t.Fatalf("auto+local should take the recap/append path and succeed: %v", err)
	}
	if res.FinalText != "ok" {
		t.Errorf("final = %q, want ok (the text path)", res.FinalText)
	}
	if res.State != nil {
		t.Errorf("auto+local must NOT run stateful (State should be nil): %v", res.State)
	}
}

// Control: the SAME plain-text provider marked frontier (Local=false) DOES take
// the stateful path under auto, and there errors because it never calls
// emit_state — confirming the routing hinges on Local, not the provider itself.
func TestRun_Auto_FrontierTextProviderTakesStateful(t *testing.T) {
	_, err := Run(context.Background(), RunOptions{
		Provider:   &textProvider{local: false},
		Model:      "x",
		Dispatcher: tools.NewDispatcher(nil),
		Segments:   statefulTaskSegs(),
		Context:    autoCtx(),
	})
	if err == nil {
		t.Fatal("auto+frontier with a non-emit_state provider must error (it entered the stateful loop)")
	}
}
