package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An agent that never captures content takes its spawn tree with it: a
// sub-run's input is what its never parent wrote, so a child under it
// captures nothing, whatever its own agent and its tenant say.
func TestContentCapture_ASubRunOfANeverRunCapturesNothing(t *testing.T) {
	s, _, _, _, _, _ := contentCaptureFixture(t, true)
	never := s.cfg().Agents["allowed"]
	never.Telemetry = &config.AgentTelemetry{ContentCapture: config.AgentContentCaptureNever}
	capturing := s.cfg().Agents["allowed"]
	base := lcotel.WithTenant(context.Background(), "acme")

	if lcotel.ContentCaptureFrom(s.withContentCapture(base, "acme", capturing, false)) == nil {
		t.Fatal("a capturing agent's run captures nothing: this test cannot see a leak")
	}
	parent := s.withContentCapture(base, "acme", never, false)
	if lcotel.ContentCaptureFrom(parent) != nil {
		t.Fatal("a never agent's run captures")
	}
	child := s.withContentCapture(parent, "acme", capturing, false)
	if lcotel.ContentCaptureFrom(child) != nil {
		t.Error("a sub-run of a never run captures its input")
	}
	grandchild := s.withContentCapture(child, "acme", capturing, false)
	if lcotel.ContentCaptureFrom(grandchild) != nil {
		t.Error("a sub-run two levels under a never run captures")
	}
}

// A never child's answer comes back as its parent's tool result: the
// spawning tool call is marked, so neither its span nor the parent's next
// model call captures the answer. Nothing stored changes.
func TestPrepareSubRun_ANeverChildMarksItsAnswerNotCaptured(t *testing.T) {
	for _, never := range []bool{false, true} {
		cfg := noTraceConfig()
		if never {
			c := cfg.Agents["child"]
			c.Telemetry = &config.AgentTelemetry{ContentCapture: config.AgentContentCaptureNever}
			cfg.Agents["child"] = c
		}
		srv, _ := makeServer(t, &childRouter{}, cfg)
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_parent", UserID: "alice"})
		ctx, mark := tools.WithContentMark(ctx)
		if _, err := srv.prepareSubRun(ctx, "child", "", "hi", "", false, func(providers.Event) {}); err != nil {
			t.Fatalf("never=%v: %v", never, err)
		}
		if mark.NotCaptured() != never {
			t.Errorf("never=%v: the spawning call is marked not captured = %v", never, mark.NotCaptured())
		}
		if mark.NotRetained() {
			t.Errorf("never=%v: the child's answer is marked not retained: its parent would store nothing of it", never)
		}
	}
}
