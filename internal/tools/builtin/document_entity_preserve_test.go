package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

// The regression suite for the sidecar-preservation bug, found by driving the
// shipped ops against a live deployment rather than by any test here.
//
// One mechanism, three failures. writeChunkMeta rebuilt the sidecar row from
// defaults on every upsert, so a re-observation of an existing fact silently
// discarded whatever it was not handed — while the chunk half of the SAME operation
// carefully preserved a title the caller never mentioned. Two halves, opposite
// semantics.
//
// A consolidator upserts by natural key on every pass, so none of this needed an
// unusual caller: re-observation is the normal case.

// TestUpsert_ReObservationKeepsAFactRetired is the severe one.
//
// A superseded fact came back as CURRENT beside the fact that replaced it, while the
// `supersedes` edge still recorded that it had been replaced. Two contradictory facts
// live at once in a store other agents read as ground truth — the exact failure
// supersede-not-delete exists to prevent, reachable by simply seeing the old fact
// again.
func TestUpsert_ReObservationKeepsAFactRetired(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	stale := upsert(t, d, ctx, docID, "office", "office location", "The office is in Berlin.", "")
	fresh := upsert(t, d, ctx, docID, "office-v2", "office location (corrected)", "The office moved to Hamburg.", "")
	supersede(t, d, ctx, fresh, stale)

	before := metaOf(t, d, ctx, stale)
	if before.InvalidAt == nil || before.ExpiredAt == nil {
		t.Fatalf("fixture: supersede should have stamped both end-timestamps, got %+v", before)
	}

	// The re-observation: same natural key, a fresher wording, nothing else said.
	upsert(t, d, ctx, docID, "office", "", "The office is in Berlin. (seen again)", "")

	after := metaOf(t, d, ctx, stale)
	if after.InvalidAt == nil {
		t.Error("a re-observation un-retired the fact in world time (invalid_at cleared)")
	}
	if after.ExpiredAt == nil {
		t.Error("a re-observation un-retired the fact in system time (expired_at cleared)")
	}
	if after.InvalidAt != nil && *after.InvalidAt != *before.InvalidAt {
		t.Errorf("invalid_at moved: %d → %d", *before.InvalidAt, *after.InvalidAt)
	}
	if after.ExpiredAt != nil && *after.ExpiredAt != *before.ExpiredAt {
		t.Errorf("expired_at moved: %d → %d", *before.ExpiredAt, *after.ExpiredAt)
	}
}

// TestUpsert_ReObservationKeepsTheEvidentialClass: `evidential` is the retention
// exemption that keeps source-of-truth material from being aged out. Silently
// dropping it back to `derived` hands the prune the one thing it must never take —
// and the prune is off by default, so nobody would notice until it was on.
func TestUpsert_ReObservationKeepsTheEvidentialClass(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	id := upsert(t, d, ctx, docID, "evidence", "evidence", "Raw source material.", "evidential")
	if got := metaOf(t, d, ctx, id).Class; got != "evidential" {
		t.Fatalf("fixture: class = %q, want evidential", got)
	}

	upsert(t, d, ctx, docID, "evidence", "", "Raw source material, seen again.", "")

	if got := metaOf(t, d, ctx, id).Class; got != "evidential" {
		t.Errorf("a re-observation downgraded the class to %q — the retention exemption is lost", got)
	}
}

// TestUpsert_ReObservationKeepsTheFirstBeliefTime: created_at is SYSTEM time — when
// the store began believing the fact. Resetting it on every write turns it into
// "when we last wrote this", which is what updated_at already is, and erases the
// axis that makes the store bi-temporal at all.
func TestUpsert_ReObservationKeepsTheFirstBeliefTime(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	id := upsert(t, d, ctx, docID, "rotation", "rotation", "Weekly, on Mondays.", "")
	first := metaOf(t, d, ctx, id)
	if first.CreatedAt == nil || first.ValidAt == nil {
		t.Fatalf("fixture: a fresh row should carry both start-timestamps, got %+v", first)
	}

	upsert(t, d, ctx, docID, "rotation", "", "Weekly, on Mondays at 10:00 UTC.", "")

	after := metaOf(t, d, ctx, id)
	if after.CreatedAt == nil || *after.CreatedAt != *first.CreatedAt {
		t.Errorf("created_at moved on re-observation: %d → %d", *first.CreatedAt, deref(after.CreatedAt))
	}
	// valid_at is WORLD time: when the fact was true. Seeing it again is not new
	// information about when it became true.
	if after.ValidAt == nil || *after.ValidAt != *first.ValidAt {
		t.Errorf("valid_at moved on re-observation: %d → %d", *first.ValidAt, deref(after.ValidAt))
	}
}

// TestUpsert_ExplicitFieldsStillWin: preservation must not become immutability. The
// caller remains able to correct any of these — including reviving a retired fact,
// which is now something you have to ask for rather than something a routine write
// does behind your back.
func TestUpsert_ExplicitFieldsStillWin(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	id := upsert(t, d, ctx, docID, "thing", "thing", "A fact.", "evidential")

	// Correct the class, the world-time validity and the confidence in one write.
	validAt := int64(1_700_000_000_000_000_000)
	conf := 0.42
	res, err := d.Execute(ctx, entityJSON(map[string]any{
		"op": "upsert_chunk", "scope": "user", "document_id": docID,
		"natural_key": "thing", "class": "derived",
		"valid_at": validAt, "confidence": conf,
	}))
	if err != nil || res.IsError {
		t.Fatalf("upsert: %v %s", err, res.Text)
	}
	got := metaOf(t, d, ctx, id)
	if got.Class != "derived" {
		t.Errorf("an explicit class must win: %q", got.Class)
	}
	if got.ValidAt == nil || *got.ValidAt != validAt {
		t.Errorf("an explicit valid_at must win: %d", deref(got.ValidAt))
	}
	if got.Confidence == nil || *got.Confidence != conf {
		t.Errorf("an explicit confidence must win: %v", got.Confidence)
	}

	// And an explicit revival: clearing a retirement is allowed, just not implicit.
	other := upsert(t, d, ctx, docID, "other", "other", "Replacement.", "")
	supersede(t, d, ctx, other, id)
	if metaOf(t, d, ctx, id).InvalidAt == nil {
		t.Fatal("fixture: supersede did not retire")
	}
	res, err = d.Execute(ctx, entityJSON(map[string]any{
		"op": "upsert_chunk", "scope": "user", "document_id": docID,
		"natural_key": "thing", "invalid_at": 0,
	}))
	if err != nil || res.IsError {
		t.Fatalf("revive: %v %s", err, res.Text)
	}
	if iv := metaOf(t, d, ctx, id).InvalidAt; iv == nil || *iv != 0 {
		t.Errorf("an explicit invalid_at must win, got %d", deref(iv))
	}
}

// TestUpsert_CreateIsUnchanged pins the create path: preservation reads a row that
// does not exist yet, so a first write must still land today's defaults rather than
// inheriting anything or refusing.
func TestUpsert_CreateIsUnchanged(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	id := upsert(t, d, ctx, docID, "fresh", "fresh", "First sighting.", "")
	got := metaOf(t, d, ctx, id)
	if got.Class != "derived" {
		t.Errorf("a fresh row defaults to derived, got %q", got.Class)
	}
	if got.ValidAt == nil || got.CreatedAt == nil {
		t.Errorf("a fresh row stamps both start-timestamps, got %+v", got)
	}
	if got.InvalidAt != nil || got.ExpiredAt != nil {
		t.Errorf("a fresh row is not retired, got invalid_at=%d expired_at=%d",
			deref(got.InvalidAt), deref(got.ExpiredAt))
	}
	if got.NaturalKey != "fresh" {
		t.Errorf("natural_key = %q", got.NaturalKey)
	}
}

// ---- helpers ----

func newEntityDoc(t *testing.T, d *Document, ctx context.Context) string {
	t.Helper()
	res, err := d.Execute(ctx, entityJSON(map[string]any{
		"op": "create_document", "scope": "user", "title": "entities", "path": "/t/entities",
	}))
	if err != nil || res.IsError {
		t.Fatalf("create_document: %v %s", err, res.Text)
	}
	var out struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.DocumentID
}

// upsert returns the chunk id. A blank title is omitted entirely, which is how a
// re-observation that only carries a body reaches the tool.
func upsert(t *testing.T, d *Document, ctx context.Context, docID, key, title, body, class string) string {
	t.Helper()
	in := map[string]any{
		"op": "upsert_chunk", "scope": "user", "document_id": docID,
		"natural_key": key, "body": body,
	}
	if title != "" {
		in["title"] = title
	}
	if class != "" {
		in["class"] = class
	}
	res, err := d.Execute(ctx, entityJSON(in))
	if err != nil || res.IsError {
		t.Fatalf("upsert_chunk %q: %v %s", key, err, res.Text)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.ID
}

func supersede(t *testing.T, d *Document, ctx context.Context, newID, oldID string) {
	t.Helper()
	res, err := d.Execute(ctx, entityJSON(map[string]any{
		"op": "supersede_chunk", "scope": "user", "id": newID, "supersedes_id": oldID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("supersede_chunk: %v %s", err, res.Text)
	}
}

// metaOf reads the sidecar THROUGH the tool's own reader, so the test cannot pass
// against a shape the tool does not actually produce.
func metaOf(t *testing.T, d *Document, ctx context.Context, chunkID string) chunkMetaRow {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	row, found, err := d.readChunkMeta(ctx, key, chunkID)
	if err != nil {
		t.Fatalf("readChunkMeta: %v", err)
	}
	if !found {
		t.Fatalf("no sidecar row for chunk %s", chunkID)
	}
	return row
}

// entityJSON marshals a tool input. Named for this file: the package already has a
// mustJSON with a different signature.
func entityJSON(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// deref prints a nullable timestamp as a number (-1 for NULL). A %v on the pointer
// prints its ADDRESS, which is what the first run of these tests reported.
func deref(v *int64) int64 {
	if v == nil {
		return -1
	}
	return *v
}
