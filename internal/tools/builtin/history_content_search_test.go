package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC CI P2 — searching your chats by what was said.

// seedTurn indexes one turn the way the server's trace writer does: a `trace.turn:`
// row in the SPEAKER'S OWN user scope, carrying its session in the value.
func seedTurn(t *testing.T, s store.Store, tenant, user, sessionID, text string, emb *fakeEmbedder) string {
	t.Helper()
	ctx := context.Background()
	key := store.TraceTurnKeyPrefix + sessionID + ":" + text[:1]
	value, _ := json.Marshal(map[string]string{
		"text": text, "speaker": "user", "session_id": sessionID, "run_id": "run-" + sessionID,
	})
	if err := s.MemorySet(ctx, tenant, store.MemoryScopeUser, user, key, value, 0); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	vec, err := emb.Embed(ctx, []string{text})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if err := s.MemoryEmbedSet(ctx, tenant, store.MemoryScopeUser, user, key, store.MemoryEmbedding{
		Provider: emb.provider, Model: emb.model, Dimension: len(vec[0]), Vector: vec[0], EmbedText: text,
	}); err != nil {
		t.Fatalf("embed set: %v", err)
	}
	return key
}

func contentSearch(t *testing.T, h *History, ctx context.Context, query string) (tools.Result, map[string]any) {
	t.Helper()
	res, err := h.Execute(ctx, json.RawMessage(
		`{"op":"search","scope":"user","match":"content","query":"`+query+`"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		_ = json.Unmarshal([]byte(res.Text), &out)
	}
	return res, out
}

// TestHistorySearch_ContentFindsAChatItsTitleWouldNot is the payoff.
//
// The title match is what existed, and a chat named by the auto-titler is exactly the
// one a person cannot find. This asserts the content mode reaches a chat whose TITLE
// shares no word with the query.
func TestHistorySearch_ContentFindsAChatItsTitleWouldNot(t *testing.T) {
	h, s := historyVectorFixture(t)
	emb := newFakeEmbedder("fake", "v1", "postgres", "upgrade", "lunch", "pizza")
	h.Embedder = emb
	ctx := histCtx([]string{"user"}, "chat", "u1", "acme")

	dbChat := seedChat(t, s, "acme", "chat", "u1")
	foodChat := seedChat(t, s, "acme", "chat", "u1")
	seedTurn(t, s, "acme", "u1", dbChat, "postgres upgrade", emb)
	seedTurn(t, s, "acme", "u1", foodChat, "lunch pizza", emb)

	res, out := contentSearch(t, h, ctx, "postgres upgrade")
	if res.IsError {
		t.Fatalf("content search: %s", res.Text)
	}
	chats, _ := out["chats"].([]any)
	if len(chats) == 0 {
		t.Fatal("content search found nothing — the index holds the words it asked for")
	}
	first, _ := chats[0].(map[string]any)
	if got, _ := first["session_id"].(string); got != dbChat {
		t.Errorf("top chat = %q, want the one whose TURNS matched (%q)", got, dbChat)
	}
	// And the evidence: a result that cannot say WHY it is in the list leaves a reader
	// unable to judge whether the search understood them.
	turns, _ := out["matched_turns"].([]any)
	if len(turns) != len(chats) {
		t.Fatalf("matched_turns has %d entries for %d chats — they are index-aligned", len(turns), len(chats))
	}
	m, _ := turns[0].(map[string]any)
	if txt, _ := m["text"].(string); !strings.Contains(txt, "postgres") {
		t.Errorf("the matched turn does not carry the words that matched: %v", m)
	}
}

// TestHistorySearch_ContentCollapsesManyTurnsToOneChat. A conversation about one topic
// matches many times; a list repeating the same chat five times buries four answers.
func TestHistorySearch_ContentCollapsesManyTurnsToOneChat(t *testing.T) {
	h, s := historyVectorFixture(t)
	emb := newFakeEmbedder("fake", "v1", "postgres", "upgrade", "again")
	h.Embedder = emb
	ctx := histCtx([]string{"user"}, "chat", "u1", "acme")

	chat := seedChat(t, s, "acme", "chat", "u1")
	seedTurn(t, s, "acme", "u1", chat, "postgres upgrade", emb)
	seedTurn(t, s, "acme", "u1", chat, "again postgres", emb)

	_, out := contentSearch(t, h, ctx, "postgres")
	chats, _ := out["chats"].([]any)
	if len(chats) != 1 {
		t.Errorf("one chat matched twice came back %d times", len(chats))
	}
}

// TestHistorySearch_ContentNeverCrossesToAnotherUsersChat.
//
// The index is per-user by construction — a trace lives under the caller's own user
// id, resolved server-side — and each candidate is then loaded through the SAME gate
// `get` uses. This asserts the composition, because the failure would be silent: a
// stray session id in the index would otherwise walk straight into the result.
func TestHistorySearch_ContentNeverCrossesToAnotherUsersChat(t *testing.T) {
	h, s := historyVectorFixture(t)
	emb := newFakeEmbedder("fake", "v1", "postgres", "upgrade")
	h.Embedder = emb
	ctx := histCtx([]string{"user"}, "chat", "u1", "acme")

	// A chat that belongs to SOMEONE ELSE, referenced from u1's own index.
	othersChat := seedChat(t, s, "acme", "chat", "u2")
	seedTurn(t, s, "acme", "u1", othersChat, "postgres upgrade", emb)

	res, out := contentSearch(t, h, ctx, "postgres upgrade")
	if res.IsError {
		t.Fatalf("content search: %s", res.Text)
	}
	if chats, _ := out["chats"].([]any); len(chats) != 0 {
		t.Errorf("a content hit reached another user's chat: %v", chats)
	}
	if strings.Contains(res.Text, othersChat) {
		t.Errorf("the response names a chat the caller may not see: %s", res.Text)
	}
}

// TestHistorySearch_ContentRefusesWithoutAnEmbedderOrUser. Both refusals name the
// alternative, because a caller who asked for content and silently got a title match
// would trust a result that answered a different question.
func TestHistorySearch_ContentRefusesWithoutAnEmbedderOrUser(t *testing.T) {
	h, _ := historyFixture(t) // no embedder
	res, _ := contentSearch(t, h, histCtx([]string{"user"}, "chat", "u1", "acme"), "anything")
	if !res.IsError || !strings.Contains(res.Text, "embedder") {
		t.Errorf("no-embedder refusal = %q, want one that names the embedder", res.Text)
	}

	h2, _ := historyFixture(t)
	h2.Embedder = newFakeEmbedder("fake", "v1", "x")
	res2, _ := contentSearch(t, h2, histCtx([]string{"user"}, "chat", "", "acme"), "anything")
	if !res2.IsError || !strings.Contains(res2.Text, "user_id") {
		t.Errorf("no-user refusal = %q, want one that names the missing user_id", res2.Text)
	}
}

// TestHistorySearch_UnknownMatchModeIsRefused. Refused rather than defaulted: a caller
// asking for `content` against a runtime that predates it would otherwise get a title
// search labelled as what they asked for — the silent-downgrade shape that made a
// dropped `sources` value look like it had worked.
func TestHistorySearch_UnknownMatchModeIsRefused(t *testing.T) {
	h, _ := historyFixture(t)
	res, err := h.Execute(histCtx([]string{"user"}, "chat", "u1", "acme"), json.RawMessage(
		`{"op":"search","scope":"user","match":"semantic","query":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "unknown match") {
		t.Errorf("an unknown match mode was accepted: %q", res.Text)
	}
}

// TestHistorySearch_TitleMatchIsUnchanged: the default path must not move. It is the
// cheap one and the one every existing caller uses.
func TestHistorySearch_TitleMatchIsUnchanged(t *testing.T) {
	h, s := historyFixture(t)
	ctx := histCtx([]string{"user"}, "chat", "u1", "acme")
	chat := seedChat(t, s, "acme", "chat", "u1")
	title := "The Postgres upgrade"
	if err := s.SetSessionMeta(context.Background(), chat, store.SessionMetaPatch{Title: &title}); err != nil {
		t.Fatalf("set title: %v", err)
	}
	res, err := h.Execute(ctx, json.RawMessage(`{"op":"search","scope":"user","query":"postgres"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := chatIDs(t, res); len(got) != 1 || got[0] != chat {
		t.Errorf("title search returned %v, want the renamed chat with no embedder involved", got)
	}
}

// historyVectorFixture is historyFixture over a store that HAS a vector index.
//
// SQLite ships none, so the real fixture cannot answer a content search at all — and
// a test that silently fell back to the title path would assert nothing about the
// feature. vectorStore is the same in-memory index the Memory tool's vector tests
// use, wrapping the real sqlite store so sessions and authorization stay real.
func historyVectorFixture(t *testing.T) (*History, *vectorStore) {
	t.Helper()
	h, s := historyFixture(t)
	vs := newVectorStore(s)
	h.Store = vs
	return h, vs
}

// TestMemorySearch_TraceHitCarriesItsAttribution — RFC CI §3.
//
// A trace hit came back addressed only by an opaque `trace.turn:` key, which is the
// same problem a document hit had before it carried its chunk: a page of raw turns
// nobody can attribute is a page nobody can act on. The session is the half that
// matters — History resolves it into the whole conversation.
func TestMemorySearch_TraceHitCarriesItsAttribution(t *testing.T) {
	entry := map[string]any{"key": store.TraceTurnKeyPrefix + "sess-a:1-1"}
	addTraceAttribution(entry, json.RawMessage(
		`{"text":"did we move the db","speaker":"user","session_id":"sess-a","run_id":"run-9","at":"2026-09-14T10:00:00Z"}`))

	for field, want := range map[string]string{
		"session_id":    "sess-a",
		"source_run_id": "run-9",
		"speaker":       "user",
		"said_at":       "2026-09-14T10:00:00Z",
	} {
		if got, _ := entry[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// TestMemorySearch_TraceAttributionToleratesAnOlderRow. A row written by an earlier
// build carries fewer fields, and a hit with no session is still a usable hit — so
// absent fields are simply not projected rather than projected empty.
func TestMemorySearch_TraceAttributionToleratesAnOlderRow(t *testing.T) {
	entry := map[string]any{"key": "k"}
	addTraceAttribution(entry, json.RawMessage(`{"text":"only the words"}`))
	for _, absent := range []string{"session_id", "source_run_id", "speaker", "said_at"} {
		if _, present := entry[absent]; present {
			t.Errorf("%s was projected from a row that does not carry it", absent)
		}
	}
	// And malformed JSON must not panic or half-fill the entry.
	addTraceAttribution(entry, json.RawMessage(`not json`))
	if len(entry) != 1 {
		t.Errorf("a malformed value added fields: %v", entry)
	}
}
