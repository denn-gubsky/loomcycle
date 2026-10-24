package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

type eraseSink struct{ events []audit.Event }

func (s *eraseSink) Record(ev audit.Event) error {
	s.events = append(s.events, ev)
	return nil
}

// TestErasureTool_WritesTheEraseAuditPair drives a live `erasure` op=execute
// against the REAL HTTP server as the connector, and asserts the same two
// records an HTTP erasure writes. Before the audit moved into the connector
// path, an MCP erasure deleted the subject and recorded nothing.
func TestErasureTool_WritesTheEraseAuditPair(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "mcp-erasure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4}}
	httpSrv := lchttp.New(cfg, nil, nil, concurrency.New(4, 4, 1000), st)
	sink := &eraseSink{}
	httpSrv.SetEraseAudit(sink)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant},
	})

	args, _ := json.Marshal(map[string]any{"op": "execute", "subject": "alice", "dry_run": false, "confirm": "alice"})
	res, err := handleErasure(ctx, &handlerEnv{connector: httpSrv}, args)
	if err != nil || res.IsError {
		t.Fatalf("erasure: %v %+v", err, res)
	}
	if len(sink.events) != 2 || sink.events[0].Action != "erase_intent" || sink.events[1].Action != "erase_result" {
		t.Fatalf("audit = %+v, want erase_intent then erase_result", sink.events)
	}
	for i, ev := range sink.events {
		if ev.TargetTenant != "acme" || ev.TargetSubject != "alice" || ev.ActorSubject != "ops" {
			t.Errorf("record %d does not name the target and the actor: %+v", i, ev)
		}
	}
}
