package sqlite

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

func TestWorkspaceContract(t *testing.T) {
	storetest.RunWorkspaces(t, func(t *testing.T) (store.Store, func()) {
		s := newTestStore(t)
		return s, func() { _ = s.Close() }
	})
}
