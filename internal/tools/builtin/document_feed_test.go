package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestIsDocumentFeedChannel_MatchesOnlyADocumentsChangeFeed(t *testing.T) {
	for name, want := range map[string]bool{
		"documents/0101a323877fa6c3c57ab25245bac110/chunks": true,
		"documents/ingested":         false,
		"documents//chunks":          false,
		"documents/a/b/chunks":       false,
		"memory/subjects":            false,
		"documentsX/abc/chunks":      false,
		"documents/abc/chunks/extra": false,
	} {
		if got := IsDocumentFeedChannel(name); got != want {
			t.Errorf("IsDocumentFeedChannel(%q) = %v, want %v", name, got, want)
		}
	}
}

// feedCount reads how many messages a document's change feed holds for the
// fixture's tenant, from the store's own stats.
func feedCount(t *testing.T, st store.Store, tenant, docID string) int64 {
	t.Helper()
	stats, err := st.ChannelStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stats {
		if s.TenantID == tenant && s.Channel == documentFeedPrefix+docID+documentFeedSuffix {
			return s.MessageCount
		}
	}
	return 0
}

// Deleting a document takes its change feed with it, and leaves another
// document's feed alone.
func TestDeleteDocument_PurgesItsChangeFeed(t *testing.T) {
	d, ctx, st := documentFixture(t)
	mk := func(title string) string {
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q}`, title))
		if r.IsError {
			t.Fatalf("create_document: %s", r.Text)
		}
		id := out["document_id"].(string)
		req, _ := json.Marshal(map[string]any{"op": "create_chunk", "scope": "user", "document_id": id, "parent_id": out["root_chunk_id"], "title": "part", "body": "text"})
		if _, r := docExec(t, d, ctx, string(req)); r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
		return id
	}
	gone, kept := mk("Gone"), mk("Kept")
	if feedCount(t, st, "tnt", gone) == 0 || feedCount(t, st, "tnt", kept) == 0 {
		t.Fatal("the fixture's documents have no change events to purge")
	}

	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_document","scope":"user","id":%q}`, gone)); r.IsError {
		t.Fatalf("delete_document: %s", r.Text)
	}
	if n := feedCount(t, st, "tnt", gone); n != 0 {
		t.Errorf("the deleted document's feed still holds %d messages", n)
	}
	if feedCount(t, st, "tnt", kept) == 0 {
		t.Error("another document's feed was purged too")
	}
}

// The sweep purges a feed quiet for longer than the idle period, keeps a recent
// one, and never touches a channel that is not a document's feed.
func TestSweepDocumentFeeds_PurgesOnlyIdleFeeds(t *testing.T) {
	_, _, st := documentFixture(t)
	ctx := context.Background()
	publish := func(tenant, channel string) {
		t.Helper()
		if _, _, err := st.ChannelPublish(ctx, store.ChannelMessage{
			Channel: channel, TenantID: tenant, Scope: store.MemoryScopeTenant, Payload: []byte(`{}`),
		}, 256); err != nil {
			t.Fatal(err)
		}
	}
	feedA := "documents/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/chunks"
	feedB := "documents/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/chunks"
	publish("acme", feedA)
	publish("globex", feedB)
	publish("acme", "documents/ingested")

	count := func(tenant, channel string) int64 {
		stats, _ := st.ChannelStats(ctx)
		for _, s := range stats {
			if s.TenantID == tenant && s.Channel == channel {
				return s.MessageCount
			}
		}
		return 0
	}

	// Recent: nothing is idle yet.
	if n, err := SweepDocumentFeeds(ctx, st, DocumentFeedIdleTTL, time.Now()); err != nil || n != 0 {
		t.Fatalf("recent feeds: purged %d (%v), want 0", n, err)
	}
	if count("acme", feedA) != 1 {
		t.Fatal("a recent feed was purged")
	}

	// Past the idle period every feed is idle, in every tenant.
	n, err := SweepDocumentFeeds(ctx, st, DocumentFeedIdleTTL, time.Now().Add(DocumentFeedIdleTTL+24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("idle feeds: purged %d (%v), want 2", n, err)
	}
	if count("acme", feedA) != 0 || count("globex", feedB) != 0 {
		t.Error("an idle feed survived the sweep")
	}
	if count("acme", "documents/ingested") != 1 {
		t.Error("the sweep purged a channel that is not a document's feed")
	}
}
