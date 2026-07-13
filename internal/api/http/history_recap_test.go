package http

import (
	"context"
	"testing"
)

// TestRecapSession_ProducesSummary: the server-injected History op=recap
// summarizer (Server.RecapSession — the off-loop twin of the compaction summary
// step) replays a chat's whole session transcript and returns the provider's
// summary text. Reuses the compaction fixture's scripted provider, which returns
// "COMPACTED SUMMARY" for any summarize call.
func TestRecapSession_ProducesSummary(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, _ := seedConversation(t, srv, true)

	summary, err := srv.RecapSession(context.Background(), sessID)
	if err != nil {
		t.Fatalf("RecapSession: %v", err)
	}
	if summary != "COMPACTED SUMMARY" {
		t.Errorf("summary = %q, want the scripted provider's summary", summary)
	}
}

// TestRecapSession_EmptyTranscriptErrors: a chat with no transcript yet is a
// clean error, never a panic or an empty-string "success" the tool would persist.
func TestRecapSession_EmptyTranscriptErrors(t *testing.T) {
	srv, _ := compactFixture(t)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "compactor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RecapSession(ctx, sess.ID); err == nil {
		t.Fatal("recap of a chat with no transcript must return an error")
	}
}
