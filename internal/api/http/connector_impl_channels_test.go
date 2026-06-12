package http

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// channelGetErrStore embeds a real store but forces ChannelGet to fail with a
// non-NotFound error — to exercise the I5 error-propagation path.
type channelGetErrStore struct {
	store.Store
	err error
}

func (s channelGetErrStore) ChannelGet(context.Context, string) (store.ChannelRow, error) {
	return store.ChannelRow{}, s.err
}

// TestRequireChannelDeclared_PropagatesStoreError pins exp7 I5: a genuine store
// fault on the declared-check must surface as that error, NOT be swallowed into
// a spurious channel_not_declared denial (the old code did `if err == nil`).
// FAIL-BEFORE: with the swallow, the returned error wraps ErrChannelNotDeclared.
func TestRequireChannelDeclared_PropagatesStoreError(t *testing.T) {
	real, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })

	srv := &Server{
		cfg:   &config.Config{}, // no yaml channels → the store path is taken
		store: channelGetErrStore{Store: real, err: errors.New("db-connection-lost")},
	}

	_, gotErr := srv.requireChannelDeclared(context.Background(), "anything")
	if gotErr == nil {
		t.Fatal("expected an error from a failing ChannelGet, got nil")
	}
	if errors.Is(gotErr, connector.ErrChannelNotDeclared) {
		t.Errorf("store fault was masked as channel_not_declared: %v", gotErr)
	}
	if !strings.Contains(gotErr.Error(), "db-connection-lost") {
		t.Errorf("store error not propagated; got %v", gotErr)
	}
}

// TestRequireChannelDeclared_PointLookupResolvesAndNotFound covers the happy
// paths: a runtime channel resolves via the point lookup (carrying its
// MaxMessages/DefaultTTL), a yaml channel resolves without touching the store,
// and an unknown name reports ErrChannelNotDeclared.
func TestRequireChannelDeclared_PointLookupResolvesAndNotFound(t *testing.T) {
	real, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })

	ctx := context.Background()
	if err := real.ChannelsCreate(ctx, store.ChannelRow{
		Name: "runtime-q", Scope: "global", Semantic: "queue", MaxMessages: 7, DefaultTTL: 60,
	}); err != nil {
		t.Fatalf("ChannelsCreate: %v", err)
	}

	srv := &Server{
		cfg: &config.Config{Channels: map[string]config.Channel{
			"yaml-q": {Scope: "global", Semantic: "queue", MaxMessages: 3, DefaultTTL: 30},
		}},
		store: real,
	}

	// Runtime channel resolves via the point lookup with its own fields.
	got, err := srv.requireChannelDeclared(ctx, "runtime-q")
	if err != nil {
		t.Fatalf("runtime-q: unexpected error %v", err)
	}
	if got.MaxMessages != 7 || got.DefaultTTL != 60 {
		t.Errorf("runtime-q def = %+v, want MaxMessages=7 DefaultTTL=60", got)
	}

	// yaml channel resolves without the store.
	got, err = srv.requireChannelDeclared(ctx, "yaml-q")
	if err != nil {
		t.Fatalf("yaml-q: unexpected error %v", err)
	}
	if got.MaxMessages != 3 || got.DefaultTTL != 30 {
		t.Errorf("yaml-q def = %+v, want MaxMessages=3 DefaultTTL=30", got)
	}

	// Unknown name → not declared (a clean NotFound, not a propagated error).
	if _, err := srv.requireChannelDeclared(ctx, "ghost"); !errors.Is(err, connector.ErrChannelNotDeclared) {
		t.Errorf("ghost: err = %v, want ErrChannelNotDeclared", err)
	}
}
