package credential

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The store the runtime hands NewEngine satisfies Store. Asserted here, not in
// internal/store: these tests open a real store, so the store importing this
// package would be an import cycle.
var _ Store = store.Store(nil)

func TestIsNotFound_MatchesOnlyTheStoresMissWrappedOrNot(t *testing.T) {
	miss := &store.ErrNotFound{Kind: "credential_def", ID: "gh"}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"bare miss":          {miss, true},
		"wrapped miss":       {fmt.Errorf("lookup: %w", miss), true},
		"joined miss":        {errors.Join(errors.New("other"), miss), true},
		"typed-nil miss":     {(*store.ErrNotFound)(nil), true},
		"nil":                {nil, false},
		"plain error":        {errors.New("credential_def not found: gh"), false},
		"other store error":  {&store.SubstrateError{Code: "not_found", Msg: "gone"}, false},
		"wrapped other kind": {fmt.Errorf("lookup: %w", ErrUnopenable), false},
	} {
		if got := isNotFound(c.err); got != c.want {
			t.Errorf("%s: isNotFound(%v) = %v, want %v", name, c.err, got, c.want)
		}
	}
}

// The real store's miss is recognised: a by-name read of an absent row is
// "no such credential", not a store failure.
func TestIsNotFound_MatchesTheSQLiteStoresMiss(t *testing.T) {
	e := newEngine(t, testKey(1))
	_, err := e.store.CredentialDefGet(context.Background(), "t", "tenant", "", "absent")
	if err == nil || !isNotFound(err) {
		t.Fatalf("CredentialDefGet of an absent row: err = %v, want a miss isNotFound recognises", err)
	}
}
