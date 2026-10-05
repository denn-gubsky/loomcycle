package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// teamChannelSource is a store holding team sdlc in tenant acme with two
// messages on its own channel "inbox" (one acked, so a cursor too) and one
// message on the ordinary channel "news". Returns its snapshot.
func teamChannelSource(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	t.Cleanup(srcClose)
	row, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_sdlc", TenantID: "acme", Name: "sdlc", Definition: json.RawMessage(`{"entry":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.TeamDefSetActive(ctx, "acme", "sdlc", row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatal(err)
	}
	inbox := store.TeamChannelName("sdlc", "inbox")
	cursors := publishChannelBatch(t, src, "acme", inbox, store.MemoryScopeUser, "alice", 2)
	if err := src.ChannelAck(ctx, "acme", inbox, store.MemoryScopeUser, "alice", cursors[0]); err != nil {
		t.Fatal(err)
	}
	publishChannelBatch(t, src, "acme", "news", store.MemoryScopeUser, "alice", 1)
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A team's own channel rows whose team does not land in their tenant are not
// written: nothing could reach them, and a team created later under the name
// would inherit them. A same-named team in ANOTHER tenant does not admit them.
// Other channels restore as before.
func TestRestore_TeamChannelRowsWithoutTheirTeamAreSkipped(t *testing.T) {
	ctx := context.Background()
	raw := teamChannelSource(t)
	dst, dstClose := newTestStore(t)
	defer dstClose()
	if _, err := dst.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_other", TenantID: "globex", Name: "sdlc", Definition: json.RawMessage(`{"entry":"a"}`)}); err != nil {
		t.Fatal(err)
	}

	// The team itself does not land: what an author on this host could not
	// have created.
	res, err := Restore(ctx, dst, raw, RestoreOptions{Validators: map[string]func(json.RawMessage) error{
		migrations.SectionTeamDefs: func(json.RawMessage) error { return errors.New("refused") },
	}})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.TeamDefsRestored != 0 {
		t.Fatalf("setup: team restored %d, want 0", res.TeamDefsRestored)
	}
	if res.TeamChannelRowsSkipped != 3 {
		t.Errorf("team channel rows skipped = %d, want 3 (2 messages + 1 cursor)", res.TeamChannelRowsSkipped)
	}
	if res.ChannelMessagesRestored != 1 || res.ChannelCursorsRestored != 0 {
		t.Errorf("restored %d messages, %d cursors; want only news's 1 message", res.ChannelMessagesRestored, res.ChannelCursorsRestored)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "team acme/sdlc's own channels not restored") {
		t.Errorf("no warning names the team; warnings: %v", res.Warnings)
	}
	inbox := store.TeamChannelName("sdlc", "inbox")
	if msgs, _, err := dst.ChannelSubscribe(ctx, "acme", inbox, store.MemoryScopeUser, "alice", "", 10); err != nil || len(msgs) != 0 {
		t.Errorf("acme's %s holds %d messages (err %v), want none", inbox, len(msgs), err)
	}
	if cur, err := dst.ChannelCommittedCursor(ctx, "acme", inbox, store.MemoryScopeUser, "alice"); err != nil || cur != "" {
		t.Errorf("acme's %s cursor = %q (err %v), want none", inbox, cur, err)
	}
}

// A restored team gets its own channels' messages and cursors back.
func TestRestore_RestoredTeamGetsItsOwnChannelRowsBack(t *testing.T) {
	ctx := context.Background()
	raw := teamChannelSource(t)
	dst, dstClose := newTestStore(t)
	defer dstClose()

	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.TeamChannelRowsSkipped != 0 {
		t.Errorf("team channel rows skipped = %d, want 0 (warnings: %v)", res.TeamChannelRowsSkipped, res.Warnings)
	}
	if res.ChannelMessagesRestored != 3 || res.ChannelCursorsRestored != 1 {
		t.Errorf("restored %d messages, %d cursors; want 3 and 1", res.ChannelMessagesRestored, res.ChannelCursorsRestored)
	}
	inbox := store.TeamChannelName("sdlc", "inbox")
	cur, err := dst.ChannelCommittedCursor(ctx, "acme", inbox, store.MemoryScopeUser, "alice")
	if err != nil || cur == "" {
		t.Fatalf("acme's %s cursor = %q (err %v), want the captured one", inbox, cur, err)
	}
	if msgs, _, err := dst.ChannelSubscribe(ctx, "acme", inbox, store.MemoryScopeUser, "alice", cur, 10); err != nil || len(msgs) != 1 {
		t.Errorf("acme's %s resumed with %d messages (err %v), want the 1 unread", inbox, len(msgs), err)
	}
}
