package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/cdc"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestMemoryChanges_AuthGate pins the locked decision: both change feeds are
// substrate:tenant AND carved out of member access (not member-accessible).
func TestMemoryChanges_AuthGate(t *testing.T) {
	for _, p := range []string{"/v1/_memory/changes", "/v1/_document/changes"} {
		if got := requiredScopeFor(http.MethodGet, p); got != auth.ScopeTenant {
			t.Errorf("%s scope = %q, want %s", p, got, auth.ScopeTenant)
		}
		if tenantMemberAccessible(http.MethodGet, p) {
			t.Errorf("%s must NOT be member-accessible (memberCarveOut)", p)
		}
	}
}

// TestMemoryChanges_SSEStreamsAndFilters seeds one memory + one document change
// and asserts each feed streams only its own family (tenant-scoped).
func TestMemoryChanges_SSEStreamsAndFilters(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.AppendMemoryChange(ctx, store.MemoryChange{Type: store.MemoryChangeSet, Scope: store.MemoryScopeAgent, ScopeID: "a1", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMemoryChange(ctx, store.MemoryChange{Type: store.DocumentChangeUpdated, Scope: store.MemoryScopeAgent, ScopeID: "a1", ChunkID: "CID"}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{store: st, cfgHolder: config.NewHolder(&config.Config{})}

	mem := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes")
	if !strings.Contains(mem, `"memory.set"`) || !strings.Contains(mem, `"k1"`) {
		t.Errorf("memory feed missing its change: %s", mem)
	}
	if strings.Contains(mem, `"document.chunk.updated"`) {
		t.Errorf("memory feed leaked a document change: %s", mem)
	}

	doc := runSSEOnce(t, srv.handleDocumentChanges, "/v1/_document/changes")
	if !strings.Contains(doc, `"document.chunk.updated"`) || !strings.Contains(doc, `"CID"`) {
		t.Errorf("document feed missing its change: %s", doc)
	}
	if strings.Contains(doc, `"memory.set"`) {
		t.Errorf("document feed leaked a memory change: %s", doc)
	}
}

// runSSEOnce invokes an SSE handler with a short-lived context: it drains the
// already-present changes on entry, then returns when the context times out.
func runSSEOnce(t *testing.T, h http.HandlerFunc, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

// TestMemoryChanges_OpeningFrameSaysWhetherTheFeedIsOn.
//
// With LOOMCYCLE_MEMORY_CHANGES_ENABLED unset, nothing writes the change table, so
// the stream connects, keepalives flow, and no change frame ever arrives — which
// looks exactly like a healthy feed over a quiet store. A reader watching for "is
// the pass doing anything" would wait forever on a misconfiguration and conclude
// the answer was no.
//
// The answer comes from the STORE, not from re-reading the env var: the CDC
// decorator is in the write path exactly when the feed is on, so its presence
// cannot disagree with whether writes are actually captured.
func TestMemoryChanges_OpeningFrameSaysWhetherTheFeedIsOn(t *testing.T) {
	raw, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	for _, tc := range []struct {
		name  string
		store store.Store
		want  string
	}{
		// A plain store does not implement CapturesChanges at all.
		{"feed off", raw, `"enabled":false`},
		{"feed on", cdc.Wrap(raw, nil), `"enabled":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{store: tc.store, cfgHolder: config.NewHolder(&config.Config{})}
			for path, h := range map[string]http.HandlerFunc{
				"/v1/_memory/changes":   srv.handleMemoryChanges,
				"/v1/_document/changes": srv.handleDocumentChanges,
			} {
				body := runSSEOnce(t, h, path)
				if !strings.Contains(body, "event: feed") {
					t.Errorf("%s: no opening feed frame — a reader cannot tell a disabled feed "+
						"from a quiet one:\n%s", path, body)
				}
				if !strings.Contains(body, tc.want) {
					t.Errorf("%s: opening frame missing %s:\n%s", path, tc.want, body)
				}
			}
		})
	}
}

// TestMemoryChanges_OpeningFrameEchoesTheCursor.
//
// A reader resuming with ?since=N needs to know the server accepted that cursor.
// Without it, a `since` the server silently ignored (unparseable, negative) looks
// like a feed with nothing new, and the reader re-reads from 0 or waits forever
// depending on which way it guessed.
func TestMemoryChanges_OpeningFrameEchoesTheCursor(t *testing.T) {
	raw, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	srv := &Server{store: cdc.Wrap(raw, nil), cfgHolder: config.NewHolder(&config.Config{})}

	if body := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes?since=42"); !strings.Contains(body, `"since":42`) {
		t.Errorf("opening frame did not echo the accepted cursor:\n%s", body)
	}
	// A cursor the server REFUSED must report as the value actually in use, not as
	// the one that was asked for.
	if body := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes?since=-7"); !strings.Contains(body, `"since":0`) {
		t.Errorf("a rejected cursor must report the cursor in force (0):\n%s", body)
	}
}

// TestMemoryChanges_OpeningFrameReportsEmbedderHealth.
//
// The reason this rides the change feed rather than a health endpoint: an
// embedding failure on a content write is deliberately not fatal (the body is
// stored, the embedding is skipped with a log line), so an embedder outage produces
// a perfectly busy change feed over a store whose new rows are unsearchable. A
// reader watching the feed to answer "is the memory pipeline healthy" would
// otherwise conclude yes.
//
// `absent` and `untried` are asserted as distinct because collapsing them is the
// mistake: one means nothing will ever be embedded, the other means nothing has been
// yet.
func TestMemoryChanges_OpeningFrameReportsEmbedderHealth(t *testing.T) {
	raw, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	t.Run("no embedder configured", func(t *testing.T) {
		srv := &Server{store: cdc.Wrap(raw, nil), cfgHolder: config.NewHolder(&config.Config{})}
		body := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes")
		if !strings.Contains(body, `"state":"absent"`) {
			t.Errorf("want embedder state absent:\n%s", body)
		}
	})

	t.Run("configured and observed", func(t *testing.T) {
		obs := providers.ObserveEmbedder(&stubEmbedder{})
		if _, e := obs.Embed(context.Background(), []string{"x"}); e != nil {
			t.Fatal(e)
		}
		srv := &Server{store: cdc.Wrap(raw, nil), cfgHolder: config.NewHolder(&config.Config{})}
		srv.SetEmbedder(obs)
		body := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes")
		for _, want := range []string{`"state":"ok"`, `"provider":"stub"`, `"calls":1`} {
			if !strings.Contains(body, want) {
				t.Errorf("opening frame missing %s:\n%s", want, body)
			}
		}
	})

	t.Run("failing is visible and counted", func(t *testing.T) {
		obs := providers.ObserveEmbedder(&stubEmbedder{err: errors.New("boom")})
		for i := 0; i < 2; i++ {
			_, _ = obs.Embed(context.Background(), []string{"x"})
		}
		srv := &Server{store: cdc.Wrap(raw, nil), cfgHolder: config.NewHolder(&config.Config{})}
		srv.SetEmbedder(obs)
		body := runSSEOnce(t, srv.handleDocumentChanges, "/v1/_document/changes")
		// The count is the actionable part: it is how many rows need re-embedding.
		if !strings.Contains(body, `"state":"failing"`) || !strings.Contains(body, `"failures":2`) {
			t.Errorf("want failing with a count:\n%s", body)
		}
		// And the raw error must not travel — only a classified kind.
		if strings.Contains(body, "boom") {
			t.Errorf("the frame leaked the driver's error text:\n%s", body)
		}
	})
}

type stubEmbedder struct{ err error }

func (s *stubEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	return [][]float32{{1}}, nil
}
func (s *stubEmbedder) Model() string    { return "stub-model" }
func (s *stubEmbedder) Provider() string { return "stub" }
func (s *stubEmbedder) Dimension() int   { return 1 }

// failingChangeStore serves GetMemoryChangesSince from a script, one step per
// call; every other Store method is unimplemented (the nil embedded interface
// panics), which is fine because the change feed calls nothing else.
type failingChangeStore struct {
	store.Store
	steps  []changeStep
	sinces []int64
	// done is called when the script runs out; the call then reports ctx's error,
	// as a real driver does when its context is cancelled mid-read.
	done func()
}

type changeStep struct {
	rows []store.MemoryChange
	err  error
}

func (f *failingChangeStore) GetMemoryChangesSince(ctx context.Context, _ string, afterSeq int64, _ int) ([]store.MemoryChange, error) {
	f.sinces = append(f.sinces, afterSeq)
	if len(f.sinces) > len(f.steps) {
		if f.done != nil {
			f.done()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("script exhausted")
	}
	st := f.steps[len(f.sinces)-1]
	return st.rows, st.err
}

// TestMemoryChanges_StoreReadFailureSendsAnErrorFrame.
//
// A store read error used to be swallowed and polling simply continued, so a
// broken store looked exactly like a quiet feed: keepalives, no change frames, no
// sign anything was wrong. The reader must be told — and told without the
// driver's own error text, which can name hosts, roles or credentials.
func TestMemoryChanges_StoreReadFailureSendsAnErrorFrame(t *testing.T) {
	leaky := errors.New("dial tcp db.internal:5432: password authentication failed for user hunter2")
	fs := &failingChangeStore{}
	for i := 0; i < 50; i++ {
		fs.steps = append(fs.steps, changeStep{err: leaky})
	}
	srv := &Server{store: fs, cfgHolder: config.NewHolder(&config.Config{})}

	for path, h := range map[string]http.HandlerFunc{
		"/v1/_memory/changes":   srv.handleMemoryChanges,
		"/v1/_document/changes": srv.handleDocumentChanges,
	} {
		fs.sinces = nil
		body := runSSEOnce(t, h, path)
		if n := strings.Count(body, "event: error\n"); n != 1 {
			t.Fatalf("%s: want exactly one error frame, got %d:\n%s", path, n, body)
		}
		frame := body[strings.Index(body, "event: error\n"):]
		frame = frame[:strings.Index(frame, "\n\n")]
		if !strings.Contains(frame, `"code":"store_unavailable"`) {
			t.Errorf("%s: error frame missing code store_unavailable:\n%s", path, frame)
		}
		// The client backfills `type` from the event name, as for `feed`; a type in
		// the payload would override that.
		if strings.Contains(frame, `"type"`) {
			t.Errorf("%s: error frame must not carry a type field:\n%s", path, frame)
		}
		for _, secret := range []string{"hunter2", "db.internal", "password"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s: the frame leaked the driver's error text (%q):\n%s", path, secret, body)
			}
		}
	}
}

// TestMemoryChanges_FailureStreakSendsOneErrorFrameAndSkipsNothing drives the
// tail loop through two failure streaks and checks the four properties that make
// the error frame trustworthy: one frame per streak (not one per poll), the
// cursor holds across a failure so no change is skipped, changes resume after
// recovery, and the backoff resets once reads succeed.
func TestMemoryChanges_FailureStreakSendsOneErrorFrameAndSkipsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boom := errors.New("boom")
	fs := &failingChangeStore{
		steps: []changeStep{
			{err: boom}, // streak 1 begins: one error frame
			{err: boom},
			{err: boom},
			{rows: []store.MemoryChange{ // recovery: everything since 0 arrives
				{Seq: 1, Type: store.MemoryChangeSet, ScopeID: "a1", Key: "k1"},
				{Seq: 2, Type: store.DocumentChangeUpdated, ScopeID: "a1", ChunkID: "CID"},
				{Seq: 3, Type: store.MemoryChangeSet, ScopeID: "a1", Key: "k3"},
			}},
			{},          // quiet
			{err: boom}, // streak 2 begins: a second error frame
			{},          // recovered, nothing new
		},
		done: cancel,
	}
	srv := &Server{store: fs, cfgHolder: config.NewHolder(&config.Config{})}

	rec := httptest.NewRecorder()
	stream, ok := newSSE(rec)
	if !ok {
		t.Fatal("recorder does not support streaming")
	}
	stream.start()

	const base, ceiling = 10 * time.Millisecond, 40 * time.Millisecond
	var slept []time.Duration
	srv.tailMemoryChanges(ctx, stream, "t1", 0, false, changeFeedPolling{
		base: base, max: ceiling,
		sleep: func(ctx context.Context, d time.Duration) bool {
			slept = append(slept, d)
			return ctx.Err() == nil
		},
	})
	body := rec.Body.String()

	// The final read is cut short by the cancelled context; that is the reader
	// leaving, not a third streak.
	if n := strings.Count(body, "event: error\n"); n != 2 {
		t.Errorf("want one error frame per streak (2), got %d:\n%s", n, body)
	}

	// The cursor stays at 0 through the failures, so the recovering read asks for
	// everything the failed ones would have returned.
	wantSince := []int64{0, 0, 0, 0, 3, 3, 3, 3}
	if fmt.Sprint(fs.sinces) != fmt.Sprint(wantSince) {
		t.Errorf("read cursors = %v, want %v (a failure must not move the cursor)", fs.sinces, wantSince)
	}

	if strings.Count(body, `"key":"k1"`) != 1 || strings.Count(body, `"key":"k3"`) != 1 {
		t.Errorf("each memory change must arrive exactly once after recovery:\n%s", body)
	}
	if strings.Contains(body, `"CID"`) {
		t.Errorf("memory feed leaked a document change:\n%s", body)
	}
	if strings.Index(body, `"k1"`) > strings.Index(body, `"k3"`) {
		t.Errorf("changes arrived out of order:\n%s", body)
	}
	// Recovery does not repeat the opening frame (sent by the handler, never by
	// the tail loop).
	if strings.Contains(body, "event: feed") {
		t.Errorf("recovery must not re-send the feed frame:\n%s", body)
	}

	// Doubling up to the cap during a streak; back to base on success; and the
	// second streak starts from base again rather than where the first left off.
	wantSlept := []time.Duration{2 * base, 4 * base, ceiling, base, base, 2 * base, base}
	if fmt.Sprint(slept) != fmt.Sprint(wantSlept) {
		t.Errorf("sleeps = %v, want %v", slept, wantSlept)
	}
}
