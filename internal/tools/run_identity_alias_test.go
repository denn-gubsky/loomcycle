package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/runident"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestRunIdentity_OneContextKeyAcrossToolsAndRunident pins that the identity a
// run attaches is ONE context value, whichever package names it.
//
// Most of the runtime attaches the identity through WithRunIdentity, while the
// memory backends and the remote document client read it through runident so
// they need not import this package. If the two ever kept separate context
// keys, those readers would see an empty identity — the tenant would read ""
// and a remote memory or document call would go out under the operator's
// default key instead of the tenant's — with no error anywhere.
func TestRunIdentity_OneContextKeyAcrossToolsAndRunident(t *testing.T) {
	ident := RunIdentityValue{
		UserID:    "u1",
		AgentID:   "a1",
		SessionID: "s1",
		TenantID:  "t1",
		// A *store.ParentContext fits the field directly: store aliases the
		// runident type rather than defining its own.
		ParentContext: &store.ParentContext{RootAgentRunID: "root-1", WaveIndex: 2},
	}

	t.Run("set through tools, read through runident", func(t *testing.T) {
		ctx := WithRunID(WithRunIdentity(context.Background(), ident), "run-1")
		if !runident.Has(ctx) {
			t.Fatal("runident.Has = false on a ctx WithRunIdentity attached an identity to")
		}
		if got := runident.From(ctx); !reflect.DeepEqual(got, ident) {
			t.Errorf("runident.From = %+v, want %+v", got, ident)
		}
		if got := runident.RunID(ctx); got != "run-1" {
			t.Errorf("runident.RunID = %q, want run-1", got)
		}
	})

	t.Run("set through runident, read through tools", func(t *testing.T) {
		ctx := runident.WithRunID(runident.With(context.Background(), ident), "run-2")
		if !HasRunIdentity(ctx) {
			t.Fatal("HasRunIdentity = false on a ctx runident.With attached an identity to")
		}
		if got := RunIdentity(ctx); !reflect.DeepEqual(got, ident) {
			t.Errorf("RunIdentity = %+v, want %+v", got, ident)
		}
		if got := RunID(ctx); got != "run-2" {
			t.Errorf("RunID = %q, want run-2", got)
		}
	})

	t.Run("a bare ctx has no identity through either", func(t *testing.T) {
		ctx := context.Background()
		if HasRunIdentity(ctx) || runident.Has(ctx) {
			t.Error("a ctx with nothing attached reports an identity")
		}
		if RunID(ctx) != "" || runident.RunID(ctx) != "" {
			t.Error("a ctx with nothing attached reports a run id")
		}
	})
}
