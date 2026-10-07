package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func postKeyedRun(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const keyedRunBody = `{"agent":"r","user_id":"u1","idempotency_key":"ccq-score:c1:abc","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`

// POST /v1/runs with a key that is already held starts nothing: the response
// is the existing run's stream, opened by the same two frames a fresh run
// sends, the agent frame marked deduplicated.
func TestRuns_ADuplicateKeyStreamsTheExistingRun(t *testing.T) {
	s, gate := newGatedBatchServer(t, 1) // one slot: a second run could not be admitted
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	type result struct {
		status int
		body   string
	}
	first := make(chan result, 1)
	go func() {
		st, b := postKeyedRun(t, ts.URL, keyedRunBody)
		first <- result{st, b}
	}()
	var run store.Run
	waitFor(t, "the first run to hold the key", func() bool {
		r, found, _ := s.store.RunByIdempotencyKey(ctx, clientRunKey("", "u1", "ccq-score:c1:abc"))
		run = r
		return found
	})
	sessions := sessionCount(t, s.store)

	second := make(chan result, 1)
	go func() {
		st, b := postKeyedRun(t, ts.URL, keyedRunBody)
		second <- result{st, b}
	}()
	select {
	case r := <-second:
		t.Fatalf("the duplicate's stream ended while the run was still going: %d %s", r.status, r.body)
	case <-time.After(400 * time.Millisecond):
	}
	gate <- struct{}{}

	for name, ch := range map[string]chan result{"first": first, "second": second} {
		select {
		case r := <-ch:
			if r.status != http.StatusOK || !strings.Contains(r.body, `"run_id":"`+run.ID+`"`) || !strings.Contains(r.body, "partial") || !strings.Contains(r.body, "event: done") {
				t.Errorf("%s response = %d %s; want run %s's stream to its end", name, r.status, r.body, run.ID)
			}
			if dedup := strings.Contains(r.body, `"deduplicated":true`); dedup != (name == "second") {
				t.Errorf("%s response deduplicated = %v", name, dedup)
			}
			if name == "second" {
				if i, j := strings.Index(r.body, "event: session"), strings.Index(r.body, "event: agent"); i != 0 || j < i {
					t.Errorf("the duplicate's stream does not open with session then agent: %s", r.body)
				}
				if !strings.Contains(r.body, run.SessionID) {
					t.Errorf("the duplicate's session frame does not name session %s: %s", run.SessionID, r.body)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s response did not end with the run", name)
		}
	}
	if n := sessionCount(t, s.store); n != sessions {
		t.Errorf("the duplicate left %d session(s) behind", n-sessions)
	}

	// After the run has ended, the same key replays it and ends at once.
	st, body := postKeyedRun(t, ts.URL, keyedRunBody)
	if st != http.StatusOK || !strings.Contains(body, `"deduplicated":true`) || !strings.Contains(body, "partial") || !strings.Contains(body, "event: done") {
		t.Errorf("a replay after the end = %d %s", st, body)
	}
}

func TestRuns_AKeyIsRefusedWhereItGuardsNothing(t *testing.T) {
	s, _ := newGatedBatchServer(t, 2)
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	seg := `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]`
	for name, tc := range map[string]struct{ body, want string }{
		"a malformed key":   {`{"agent":"r","idempotency_key":"a b",` + seg + `}`, "idempotency_key must match"},
		"with a session_id": {`{"agent":"r","idempotency_key":"k","session_id":"s_x",` + seg + `}`, "fresh run"},
		"with start false":  {`{"agent":"r","idempotency_key":"k","start":false,` + seg + `}`, "fresh run"},
	} {
		t.Run(name, func(t *testing.T) {
			st, body := postKeyedRun(t, ts.URL, tc.body)
			if st != http.StatusBadRequest || !strings.Contains(body, tc.want) {
				t.Errorf("response = %d %q, want 400 containing %q", st, body, tc.want)
			}
		})
	}
}

// The lookup can miss a run created right after it. The index then refuses
// the second run, and the response is the first run's stream, not a 500.
func TestRuns_ARequestRefusedByTheIndexStreamsTheWinner(t *testing.T) {
	s, gate := newGatedBatchServer(t, 4)
	blind := &missLookup{Store: s.store, miss: 2} // the POST's lookup, after the first run's own
	s.store = blind
	ts := httptest.NewServer(s.Mux())
	t.Cleanup(ts.Close)
	ctx := context.Background()
	first := batchOne(t, s, ctx, "detach", 0, keyed("ccq-score:c1:abc", "u1"))

	done := make(chan string, 1)
	go func() {
		_, b := postKeyedRun(t, ts.URL, keyedRunBody)
		done <- b
	}()
	waitFor(t, "the request to be refused by the index", func() bool { return blind.lookups.Load() >= 3 })
	gate <- struct{}{}
	select {
	case body := <-done:
		if !strings.Contains(body, `"run_id":"`+first.RunID+`"`) || !strings.Contains(body, `"deduplicated":true`) {
			t.Errorf("response = %s, want run %s's stream, deduplicated", body, first.RunID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the response did not end with the run")
	}
}
