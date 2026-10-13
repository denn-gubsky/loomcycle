package ollama

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// The bodies an Ollama 0.40.3 host returned when its llama-server died.
const (
	crashAborted    = `{"error":"llama-server process has terminated: signal: aborted (core dumped)"}`
	crashNotRunning = `{"error":"llama-server process no longer running: unknown "}`
	crashEOF        = `{"error":"an error was encountered while running the model: unexpected EOF"}`
	crashCompletion = `{"error":"Post \"http://127.0.0.1:41233/completion\": EOF"}`
)

func TestIsRunnerDeath_MatchesOnlyARunnerThatDied(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"chat: runner aborted", 500, crashAborted, true},
		{"chat: runner no longer running", 500, crashNotRunning, true},
		{"chat: unexpected EOF from the model", 500, crashEOF, true},
		{"embed: runner aborted", 500, crashAborted, true},
		{"embed: runner error as a 400", 400, crashAborted, true},
		{"embed: request to the runner dropped", 400, `{"error":"do embedding request: Post \"http://127.0.0.1:41233/embedding\": EOF"}`, true},
		{"decision: request to the runner dropped", 500, crashCompletion, true},
		{"runner port refuses while it restarts", 500, `{"error":"Post \"http://127.0.0.1:41233/completion\": dial tcp 127.0.0.1:41233: connect: connection refused"}`, true},
		{"the runner's older name", 500, `{"error":"llama runner process has terminated: signal: killed"}`, true},
		{"a body cut short mid-JSON", 500, `{"error":"llama-server process has terminated: sig`, true},
		{"plain text, not JSON", 500, `llama-server process no longer running`, true},

		{"model not found, 404", 404, `{"error":"model \"nomic-embed-text\" not found, try pulling it first"}`, false},
		{"model not found, 500", 500, `{"error":"model 'qwen3.6' not found"}`, false},
		{"model too large for memory", 500, `{"error":"model requires more system memory (24.0 GiB) than is available (8.0 GiB)"}`, false},
		{"model fault that is not a dropped connection", 500, `{"error":"an error was encountered while running the model: invalid grammar"}`, false},
		{"bad request", 400, `{"error":"invalid character 'x' looking for beginning of value"}`, false},
		{"model does not embed", 400, `{"error":"this model does not support embeddings"}`, false},
		{"a crash text under a status that is not an error from the runner", 404, crashAborted, false},
		{"a crash text in a 200", 200, crashAborted, false},
		{"rate limited", 429, `{"error":"too many requests"}`, false},
		{"empty body", 500, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRunnerDeath(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("IsRunnerDeath(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

const retryChatDone = `{"model":"qwen3.6","message":{"role":"assistant","content":"hello"},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":1}` + "\n"

// chatServer answers the n-th /api/chat request (from 0) with reply(n) and
// counts them.
func chatServer(t *testing.T, reply func(n int, w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		reply(int(calls.Add(1))-1, w)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func crash(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

// retryEvents collects what a call sends to OnEvent.
type retryEvents struct {
	mu  sync.Mutex
	got []providers.RetryInfo
}

func (r *retryEvents) on(ev providers.Event) {
	if ev.Type != providers.EventRetry || ev.Retry == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, *ev.Retry)
}

func (r *retryEvents) all() []providers.RetryInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]providers.RetryInfo(nil), r.got...)
}

func chatRequest(onEvent func(providers.Event)) providers.Request {
	return providers.Request{
		Model:    "qwen3.6",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		OnEvent:  onEvent,
	}
}

func fastDriver(id, url string) *Driver {
	d := New(id, "", url, streamhttp.Options{}, nil)
	d.runnerRetryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	return d
}

func TestCall_RunnerDeathThenSuccessCompletesAfterOneRetry(t *testing.T) {
	for _, id := range []string{"ollama-local", "ollama"} {
		for name, body := range map[string]string{"aborted": crashAborted, "not running": crashNotRunning, "unexpected EOF": crashEOF} {
			t.Run(id+"/"+name, func(t *testing.T) {
				srv, calls := chatServer(t, func(n int, w http.ResponseWriter) {
					if n == 0 {
						crash(w, 500, body)
						return
					}
					fmt.Fprint(w, retryChatDone)
				})
				var events retryEvents
				ch, err := fastDriver(id, srv.URL).Call(context.Background(), chatRequest(events.on))
				if err != nil {
					t.Fatalf("Call: %v (a runner death must be retried, not returned)", err)
				}
				var text string
				var done *providers.Event
				for ev := range ch {
					switch ev.Type {
					case providers.EventText:
						text += ev.Text
					case providers.EventDone:
						done = &ev
					case providers.EventError:
						t.Fatalf("EventError: %s", ev.Error)
					}
				}
				if text != "hello" || done == nil {
					t.Fatalf("text = %q, done = %v; want the retried call's answer", text, done)
				}
				if done.Usage == nil || done.Usage.InputTokens != 5 {
					t.Errorf("done usage = %+v, want the retried call's usage", done.Usage)
				}
				if got := calls.Load(); got != 2 {
					t.Errorf("requests = %d, want 2 (the call and exactly one retry)", got)
				}
				got := events.all()
				if len(got) != 1 {
					t.Fatalf("retry events = %+v, want exactly one", got)
				}
				want := providers.RetryInfo{Provider: id, Attempt: 1, WaitMs: 1, Reason: providers.RetryReasonSchedule}
				if got[0] != want {
					t.Errorf("retry event = %+v, want %+v", got[0], want)
				}
			})
		}
	}
}

func TestCall_ErrorThatIsNotARunnerDeathIsNotRetried(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"500 model not found":  {500, `{"error":"model 'qwen3.6' not found"}`},
		"404 model not found":  {404, `{"error":"model \"qwen3.6\" not found, try pulling it first"}`},
		"500 out of memory":    {500, `{"error":"model requires more system memory (24.0 GiB) than is available (8.0 GiB)"}`},
		"400 malformed":        {400, `{"error":"invalid character 'x' looking for beginning of value"}`},
		"404 with crash words": {404, crashAborted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, calls := chatServer(t, func(_ int, w http.ResponseWriter) { crash(w, tc.status, tc.body) })
			var events retryEvents
			_, err := fastDriver("ollama-local", srv.URL).Call(context.Background(), chatRequest(events.on))
			// The whole body must still reach the error: the classifier reads
			// it first and has to hand it back.
			want := fmt.Sprintf("ollama-local %d: %s", tc.status, tc.body)
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("requests = %d, want 1 (no retry)", got)
			}
			if got := events.all(); len(got) != 0 {
				t.Errorf("retry events = %+v, want none", got)
			}
		})
	}
}

func TestCall_RunnerDeathOnEveryAttemptFailsAfterTwoRetries(t *testing.T) {
	srv, calls := chatServer(t, func(_ int, w http.ResponseWriter) { crash(w, 500, crashAborted) })
	var events retryEvents
	_, err := fastDriver("ollama-local", srv.URL).Call(context.Background(), chatRequest(events.on))
	want := "ollama-local 500: " + crashAborted
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q (the last response, reported as before)", err, want)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (the call and two retries)", got)
	}
	got := events.all()
	if len(got) != 2 || got[0].Attempt != 1 || got[1].Attempt != 2 {
		t.Errorf("retry events = %+v, want attempts 1 and 2", got)
	}
}

// TestRunnerRetryWaits_AreTwoThenFiveSeconds pins the production bound the
// other tests shorten.
func TestRunnerRetryWaits_AreTwoThenFiveSeconds(t *testing.T) {
	if len(runnerRetryWaits) != 2 || runnerRetryWaits[0] != 2*time.Second || runnerRetryWaits[1] != 5*time.Second {
		t.Errorf("runnerRetryWaits = %v, want [2s 5s]", runnerRetryWaits)
	}
}

func TestCall_CancelDuringRunnerRetryWaitReturnsTheContextError(t *testing.T) {
	srv, calls := chatServer(t, func(_ int, w http.ResponseWriter) { crash(w, 500, crashAborted) })
	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	d.runnerRetryWaits = []time.Duration{time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The retry event is sent just before the wait starts, so cancelling from
	// it cancels during the wait, not during the request.
	req := chatRequest(func(ev providers.Event) {
		if ev.Type == providers.EventRetry {
			cancel()
		}
	})
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := d.Call(ctx, req)
		done <- result{err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Call did not return after the context was cancelled during the retry wait")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (cancelled before the retry was sent)", got)
	}
}

func TestCall_ErrorFrameInsideAStartedStreamIsNotRetried(t *testing.T) {
	srv, calls := chatServer(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"model":"qwen3.6","message":{"role":"assistant","content":"partial"},"done":false}`+"\n")
		fmt.Fprint(w, crashAborted+"\n")
	})
	var events retryEvents
	ch, err := fastDriver("ollama-local", srv.URL).Call(context.Background(), chatRequest(events.on))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var text, errText string
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			text += ev.Text
		case providers.EventError:
			errText = ev.Error
		}
	}
	if text != "partial" || !strings.Contains(errText, "llama-server process has terminated") {
		t.Errorf("text = %q, error = %q; want the partial text then the stream's error", text, errText)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (text already reached the caller, so no retry)", got)
	}
	if got := events.all(); len(got) != 0 {
		t.Errorf("retry events = %+v, want none", got)
	}
}

// embedServer answers the n-th /api/embed request (from 0) with reply(n).
func embedServer(t *testing.T, reply func(n int, w http.ResponseWriter)) (*Embedder, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(int(calls.Add(1))-1, w)
	}))
	t.Cleanup(srv.Close)
	e, err := newEmbedder("ollama-local", providers.EmbedderOptions{Model: "nomic-embed-text", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	e.runnerRetryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	return e, &calls
}

func TestOllamaEmbedder_RunnerDeathThenSuccessReturnsTheVectors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"500 runner aborted":      {500, crashAborted},
		"400 runner aborted":      {400, crashAborted},
		"400 request to runner":   {400, `{"error":"do embedding request: Post \"http://127.0.0.1:41233/embedding\": EOF"}`},
		"500 runner not running ": {500, crashNotRunning},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, calls := embedServer(t, func(n int, w http.ResponseWriter) {
				if n == 0 {
					crash(w, tc.status, tc.body)
					return
				}
				fmt.Fprint(w, `{"model":"nomic-embed-text","embeddings":[[0.1,0.2],[0.3,0.4]]}`)
			})
			got, err := e.Embed(context.Background(), []string{"a", "b"})
			if err != nil {
				t.Fatalf("Embed: %v (a runner death must be retried, not returned)", err)
			}
			if len(got) != 2 || got[1][0] != 0.3 {
				t.Errorf("vectors = %v, want the retried call's two vectors in order", got)
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("requests = %d, want 2 (the call and one retry)", n)
			}
		})
	}
}

func TestOllamaEmbedder_ErrorThatIsNotARunnerDeathIsNotRetried(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"404 model not found": {404, `{"error":"model \"nomic-embed-text\" not found, try pulling it first"}`},
		"500 model not found": {500, `{"error":"model 'nomic-embed-text' not found"}`},
		"400 not an embedder": {400, `{"error":"this model does not support embeddings"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, calls := embedServer(t, func(_ int, w http.ResponseWriter) { crash(w, tc.status, tc.body) })
			if _, err := e.Embed(context.Background(), []string{"a"}); err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("err = %v, want it to carry %s", err, tc.body)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("requests = %d, want 1 (no retry)", n)
			}
		})
	}
}

func TestOllamaEmbedder_RunnerDeathOnEveryAttemptFailsAfterTwoRetries(t *testing.T) {
	e, calls := embedServer(t, func(_ int, w http.ResponseWriter) { crash(w, 500, crashAborted) })
	_, err := e.Embed(context.Background(), []string{"a"})
	want := `ollama /api/embed: model "nomic-embed-text": status 500: ` + crashAborted
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q (reported as before)", err, want)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("requests = %d, want 3 (the call and two retries)", n)
	}
}
