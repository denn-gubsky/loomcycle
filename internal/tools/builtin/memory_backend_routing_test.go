package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// TestMemoryBackend_Mem9KindDegradesGracefully is the upgrade-safety
// regression for the removal of the external `mem9` backend kind.
//
// A deployment that ran an older build may hold a PERSISTED MemoryBackendDef
// row whose definition says `kind: mem9`. That kind no longer has a case in
// Memory.backend's switch, so it must land on the `default:` arm and serve
// from the in-process backend — logged, never a crash and never a failed
// agent run. The row is written straight to the store (not through the
// MemoryBackendDef tool, whose validator now REFUSES the kind — see
// TestMemoryBackendDefTool_CreateRefusesMem9Kind) precisely because that is
// the only way the state can still arise: authored by a previous version.
func TestMemoryBackend_Mem9KindDegradesGracefully(t *testing.T) {
	tool, ctx, cleanup := routingFixture(t)
	defer cleanup()

	// Persist a def exactly as an older build would have written it.
	legacy := store.MemoryBackendDefRow{
		DefID: "mb_legacy_mem9",
		Name:  "team-remote",
		Definition: json.RawMessage(
			`{"name":"team-remote","kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"LOOMCYCLE_M_KEY"},"fallback_on_error":"inprocess"}`),
	}
	if _, err := tool.Store.MemoryBackendDefCreate(context.Background(), legacy); err != nil {
		t.Fatalf("seed legacy def: %v", err)
	}
	if err := tool.Store.MemoryBackendDefSetActive(context.Background(), "", "team-remote", "mb_legacy_mem9", "a_test"); err != nil {
		t.Fatalf("promote legacy def: %v", err)
	}

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// The agent's Memory ops must still work end-to-end via the fallback.
	roundTrip(t, tool, withBackend(ctx, "team-remote"))

	if !strings.Contains(buf.String(), "unknown kind") {
		t.Errorf("expected an unknown-kind degradation log for a persisted kind:mem9 def, got: %q", buf.String())
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
