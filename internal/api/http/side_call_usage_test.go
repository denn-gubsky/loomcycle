package http

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestRecordRunSideCallUsage_ChargesTheRunOnTheContext — a model call a tool makes
// on a run's behalf (the search rerank) lands in that run's token_usage ledger,
// attributed to its tenant, user and session; with no run on the context there is
// nothing to charge and nothing is written.
func TestRecordRunSideCallUsage_ChargesTheRunOnTheContext(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(&config.Config{}, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)

	ctx := tools.WithRunID(context.Background(), "run-1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t1", UserID: "u1", AgentID: "reader", SessionID: "s1"})
	srv.RecordRunSideCallUsage(ctx, &providers.Usage{InputTokens: 6000, OutputTokens: 12, Provider: "ollama-local", Model: "qwen3.6:latest"})
	srv.RecordRunSideCallUsage(context.Background(), &providers.Usage{InputTokens: 1})

	rows, err := st.TokenUsageForRun(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.TenantID != "t1" || r.UserID != "u1" || r.SessionID != "s1" || r.Provider != "ollama-local" ||
		r.Model != "qwen3.6:latest" || r.InputTokens != 6000 || r.CredentialSource != "operator" {
		t.Errorf("row = %+v", r)
	}
}
