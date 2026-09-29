package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestSnapshotSections_ReaderKnowsEverySectionItWrites: every section Capture
// writes is one Restore recognises; otherwise this very binary would warn
// that its own snapshot holds a section it does not understand.
func TestSnapshotSections_ReaderKnowsEverySectionItWrites(t *testing.T) {
	for k := range envelopeSectionKeys() {
		if !migrations.KnownSection(k) {
			t.Errorf("Capture writes section %q but the migrations registry does not know it", k)
		}
	}
	// The other direction: probe a name that is certainly not a section, so
	// KnownSection is shown to answer false at all.
	if migrations.KnownSection("no_such_section") {
		t.Fatal("KnownSection accepts an arbitrary name; the unknown-section warning can never fire")
	}
}

// TestRestore_UnknownSectionIsWarned: a section a newer writer added is named
// in the warnings, and the known sections still restore.
func TestRestore_UnknownSectionIsWarned(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	if err := src.MemorySet(ctx, "", store.MemoryScopeUser, "u1", "k", json.RawMessage(`"v"`), 0); err != nil {
		t.Fatal(err)
	}
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var secs map[string]json.RawMessage
	if err := json.Unmarshal(doc["sections"], &secs); err != nil {
		t.Fatal(err)
	}
	secs["from_the_future"] = json.RawMessage(`{"version":"1.0","entries":[{"x":1}]}`)
	sb, err := json.Marshal(secs)
	if err != nil {
		t.Fatal(err)
	}
	doc["sections"] = sb
	delete(doc, "checksum") // the edit invalidates it; an unchecksummed snapshot still restores
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	var hit int
	for _, w := range res.Warnings {
		if strings.Contains(w, "from_the_future") {
			hit++
			if !strings.Contains(w, "not understood by this reader") {
				t.Errorf("warning %q names the section but not what happened to it", w)
			}
		}
	}
	if hit != 1 {
		t.Errorf("warnings %q: want exactly one naming section from_the_future", res.Warnings)
	}
	if res.MemoryRestored != 1 {
		t.Errorf("MemoryRestored = %d, want 1: an unknown section must not stop the known ones", res.MemoryRestored)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "not understood") && !strings.Contains(w, "from_the_future") {
			t.Errorf("a section this reader knows was reported as not understood: %q", w)
		}
	}
}
