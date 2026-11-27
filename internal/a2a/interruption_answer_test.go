package a2a

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestStoreInterruptResolver_ResolveRefusesAnAnswerOutsideTheOptions pins RFC
// HG D5: a peer's resume answer takes the shared answer check. Outside the
// options it is refused — nothing recorded, the run not woken — unless the
// question allows other answers. The resolver checked nothing before.
func TestStoreInterruptResolver_ResolveRefusesAnAnswerOutsideTheOptions(t *testing.T) {
	opts := []byte(`["Yes","No"]`)
	fs := &fakeInterruptStore{row: store.InterruptRow{InterruptID: "intr-1", Options: opts}}
	var notified []string
	r := NewInterruptResolver(fs, func(k string) { notified = append(notified, k) })

	err := r.Resolve(context.Background(), "intr-1", "Maybe")
	if !errors.Is(err, connector.ErrInterruptInvalidAnswer) {
		t.Fatalf("Resolve = %v, want ErrInterruptInvalidAnswer", err)
	}
	if fs.resolvedID != "" || len(notified) != 0 {
		t.Fatalf("refused answer was recorded (%q) or woke the run (%v)", fs.resolvedID, notified)
	}

	fs.row.AllowOther = true
	if err := r.Resolve(context.Background(), "intr-1", "Maybe"); err != nil {
		t.Fatalf("allow_other answer refused: %v", err)
	}
	if fs.resolvedAns != "Maybe" || len(notified) != 1 {
		t.Fatalf("store resolve = %q, notify = %v; want Maybe and one wake", fs.resolvedAns, notified)
	}
}
