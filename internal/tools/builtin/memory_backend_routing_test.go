package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// routingFixture builds a Memory tool backed by a SQLite store and a
// config carrying a static memory_backends map. ctx is pre-populated with
// a run identity + full memory scopes; tests set MemoryPolicy.Backend per
// case to drive routing. RFC I MR-3b.
func routingFixture(t *testing.T) (*Memory, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	tool := &Memory{
		Store:             s,
		MaxValueBytes:     65536,
		DefaultQuotaBytes: 1 << 20,
		Cfg: &config.Config{
			MemoryBackends: map[string]config.MemoryBackend{
				"local-store": {Name: "local-store", Kind: "inprocess"},
				"team-mem9":   {Name: "team-mem9", Kind: "mem9"},
			},
		},
	}
	ctx := tools.WithAgentName(context.Background(), "qa-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	return tool, ctx, func() { _ = s.Close() }
}

// withBackend returns ctx carrying a memory policy whose resolved backend
// NAME is set (mirrors what the HTTP server stamps from agent config).
func withBackend(ctx context.Context, name string) context.Context {
	return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
		Backend:       name,
	})
}

// roundTrip drives a set then get through the tool and asserts the stored
// value survives — the functional proof that whichever backend was routed
// to actually works.
func roundTrip(t *testing.T, tool *Memory, ctx context.Context) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"k","value":{"v":1}}`))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.IsError {
		t.Fatalf("set is_error: %s", res.Text)
	}
	res, err = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"k"}`))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if res.IsError {
		t.Fatalf("get is_error: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"v":1`) {
		t.Errorf("get missing stored value: %s", res.Text)
	}
}

func TestMemoryBackend_NamedInprocessDefRoutesAndWorks(t *testing.T) {
	tool, ctx, cleanup := routingFixture(t)
	defer cleanup()
	roundTrip(t, tool, withBackend(ctx, "local-store"))
}

func TestMemoryBackend_Mem9DefFallsBackToInprocess(t *testing.T) {
	tool, ctx, cleanup := routingFixture(t)
	defer cleanup()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// mem9 is not wired yet (MR-4) — the op must still succeed via the
	// in-process fallback, and the fallback must be logged.
	roundTrip(t, tool, withBackend(ctx, "team-mem9"))

	if !strings.Contains(buf.String(), "kind=mem9 not yet wired") {
		t.Errorf("expected mem9 fallback log, got: %q", buf.String())
	}
}

func TestMemoryBackend_UnknownNameFallsBackToDefault(t *testing.T) {
	tool, ctx, cleanup := routingFixture(t)
	defer cleanup()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// A name absent from both the static map and the substrate must NOT
	// fail — it degrades to the operator-default backend with a log.
	roundTrip(t, tool, withBackend(ctx, "does-not-exist"))

	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("expected not-found fallback log, got: %q", buf.String())
	}
}

func TestMemoryBackend_EmptyNameUsesDefaultPath(t *testing.T) {
	tool, ctx, cleanup := routingFixture(t)
	defer cleanup()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// Empty backend name is the pre-MR-3b path: it must never hit the
	// resolver and must never log a routing decision.
	roundTrip(t, tool, withBackend(ctx, ""))

	if buf.Len() != 0 {
		t.Errorf("empty backend name must not log a routing decision, got: %q", buf.String())
	}
}
