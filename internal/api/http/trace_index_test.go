package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC CI P1 — what the indexed turn is, and what it must never be.

// TestUserTurnText_TakesUserProseAndNothingElse.
//
// A system segment is operator configuration, not something anybody said — indexing
// it would put the agent's own prompt into a user's "what did I say" search. An image
// block is bytes: embedding its base64 pays the embedder to read noise.
func TestUserTurnText_TakesUserProseAndNothingElse(t *testing.T) {
	got := userTurnText([]loop.PromptSegment{
		{Role: "system", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "You are a helpful assistant."}}},
		{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "Did we move the test database?"},
			{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgo="},
			{Type: "untrusted-block", Text: "pasted log line"},
		}},
	})
	if strings.Contains(got, "helpful assistant") {
		t.Error("the system prompt was indexed as something the user said")
	}
	if strings.Contains(got, "iVBORw0KGgo") {
		t.Error("image bytes were indexed — the embedder is being paid to read base64")
	}
	if !strings.Contains(got, "Did we move the test database?") {
		t.Errorf("the user's own words are missing: %q", got)
	}
	// An untrusted block is still something the user put in the turn, so it counts.
	if !strings.Contains(got, "pasted log line") {
		t.Errorf("pasted content was dropped: %q", got)
	}
}

// TestUserTurnText_EmptyTurnIndexesNothing: a turn with only an image, or only
// whitespace, produces no row. A row that exists ranks against every query.
func TestUserTurnText_EmptyTurnIndexesNothing(t *testing.T) {
	for name, segs := range map[string][]loop.PromptSegment{
		"image only":      {{Role: "user", Content: []loop.PromptContentBlock{{Type: "image", Data: "abc"}}}},
		"whitespace only": {{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "   \n  "}}}},
		"no segments":     nil,
		"system only":     {{Role: "system", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "cfg"}}}},
	} {
		if got := userTurnText(segs); got != "" {
			t.Errorf("%s produced %q, want no row", name, got)
		}
	}
}

// TestTraceTurnKey_IsNamespacedAndPerSession. The prefix is what the classifier and
// the search exclusion both key on; the session is what makes a hit attributable.
func TestTraceTurnKey_IsNamespacedAndPerSession(t *testing.T) {
	k := traceTurnKey("sess-abc")
	if !strings.HasPrefix(k, store.TraceTurnKeyPrefix) {
		t.Errorf("key %q is outside the trace namespace — it would be classified as a note "+
			"and returned by every unfiltered search", k)
	}
	if !strings.Contains(k, "sess-abc") {
		t.Errorf("key %q does not name its session", k)
	}
	// Two turns in one session must not collide: the second would overwrite the first.
	if k == traceTurnKey("sess-abc") {
		t.Error("two turns in the same session produced the same key — the later one " +
			"silently replaces the earlier")
	}
}

// traceServer builds a Server whose only job is to index a turn: a real store, the
// trace index on, and a redactor holding one secret.
func traceServer(t *testing.T, enabled bool) (*Server, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	cfg.Env.MemoryTraceIndexEnabled = enabled
	s := &Server{cfgHolder: config.NewHolder(cfg), store: st}
	s.redactor = redact.New(map[string]string{"API_KEY": "sk-live-supersecret"}, true)
	return s, st
}

func readTrace(t *testing.T, st store.Store, tenant, user string) (string, string) {
	t.Helper()
	rows, _, err := st.MemoryList(context.Background(), tenant, store.MemoryScopeUser, user,
		store.TraceTurnKeyPrefix, 50)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	if len(rows) == 0 {
		return "", ""
	}
	return rows[0].Key, string(rows[0].Value)
}

// TestIndexUserTurn_RedactsBeforeStoring — RFC CI §6, the third admission criterion.
//
// The secret-redaction transform runs on CONTEXT, not on this write path, so a turn
// containing a pasted credential would be embedded verbatim. An embedding is not
// something you can un-say: the row can be deleted, but whatever read it while it
// existed has already read it. So the redactor runs here, and this proves it does
// rather than assuming the nil-safe call is wired.
func TestIndexUserTurn_RedactsBeforeStoring(t *testing.T) {
	s, st := traceServer(t, true)
	s.indexUserTurn(context.Background(), "r1", "sess-a", "acme", "u1",
		[]loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "deploy with sk-live-supersecret please"}}}})

	key, value := readTrace(t, st, "acme", "u1")
	if key == "" {
		t.Fatal("nothing was indexed")
	}
	if strings.Contains(value, "sk-live-supersecret") {
		t.Errorf("the indexed turn holds the credential verbatim: %s", value)
	}
	if !strings.Contains(value, "deploy with") {
		t.Errorf("redaction ate the turn instead of the secret: %s", value)
	}
}

// TestIndexUserTurn_OffByDefault. Indexing raw turns copies personal data into a
// second place — a posture change, not a behaviour change — so an unconfigured
// deployment must write nothing at all.
func TestIndexUserTurn_OffByDefault(t *testing.T) {
	s, st := traceServer(t, false)
	s.indexUserTurn(context.Background(), "r1", "sess-a", "acme", "u1",
		[]loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "anything"}}}})
	if key, _ := readTrace(t, st, "acme", "u1"); key != "" {
		t.Errorf("an unconfigured deployment indexed a turn at %s", key)
	}
}

// TestIndexUserTurn_NeedsAUserToFileUnder. A turn with no user has no scope to live
// in, and a scope chosen by default is a disclosure chosen by default.
func TestIndexUserTurn_NeedsAUserToFileUnder(t *testing.T) {
	s, st := traceServer(t, true)
	s.indexUserTurn(context.Background(), "r1", "sess-a", "acme", "",
		[]loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "whose words are these"}}}})
	rows, _, err := st.MemoryList(context.Background(), "acme", store.MemoryScopeUser, "",
		store.TraceTurnKeyPrefix, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a turn with no user was filed anyway, under %q", rows[0].Key)
	}
}

// TestIndexUserTurn_CarriesItsAttribution. A hit nobody can attribute is a hit nobody
// can use — the session and run travel in the VALUE so a reader never has to parse
// the key, whose shape is the writer's business.
func TestIndexUserTurn_CarriesItsAttribution(t *testing.T) {
	s, st := traceServer(t, true)
	s.indexUserTurn(context.Background(), "run-7", "sess-b", "acme", "u1",
		[]loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "what did we say about postgres"}}}})
	_, value := readTrace(t, st, "acme", "u1")
	for _, want := range []string{"sess-b", "run-7", `"speaker":"user"`} {
		if !strings.Contains(value, want) {
			t.Errorf("the indexed turn does not carry %s: %s", want, value)
		}
	}
}

// TestIndexUserTurn_ExpiresSoTheWindowNeedsNoSweeperOfItsOwn. The retention window is
// the row's TTL: an expired row reads as missing before any sweeper runs, MemorySweep
// deletes it on its own schedule, and its embedding cascades on that delete. A fourth
// retention family could drift from the class it prunes; a TTL cannot.
func TestIndexUserTurn_ExpiresSoTheWindowNeedsNoSweeperOfItsOwn(t *testing.T) {
	s, st := traceServer(t, true)
	s.indexUserTurn(context.Background(), "r1", "sess-a", "acme", "u1",
		[]loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
			{Type: "trusted-text", Text: "a turn that should age out"}}}})
	rows, _, err := st.MemoryList(context.Background(), "acme", store.MemoryScopeUser, "u1",
		store.TraceTurnKeyPrefix, 50)
	if err != nil || len(rows) == 0 {
		t.Fatalf("nothing indexed: %v", err)
	}
	if rows[0].ExpiresAt.IsZero() {
		t.Error("the indexed turn has no expiry — it would outlive the retention window " +
			"forever, and nothing else prunes this class")
	}
}
