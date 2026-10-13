package decision

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runnerDied is what /v1/systemone returned on an Ollama 0.40.3 host whose
// llama-server died under the call.
const runnerDied = `{"error":"Post \"http://127.0.0.1:41233/completion\": EOF"}`

// serveInTurn answers the n-th request (from 0) with replies[n], and the last
// reply for every request after that.
func serveInTurn(t *testing.T, replies ...func(http.ResponseWriter)) (*stub, string) {
	t.Helper()
	var n atomic.Int32
	return serve(t, func(w http.ResponseWriter) {
		i := int(n.Add(1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		replies[i](w)
	})
}

func fastRetryDriver(t *testing.T, o Options) Driver {
	t.Helper()
	d := newDriver(t, o)
	d.(*ollamaDriver).runnerRetryWait = time.Millisecond
	return d
}

func TestOllama_RunnerDeathThenSuccessAnswersAfterOneRetry(t *testing.T) {
	for name, body := range map[string]string{
		"request to the runner dropped": runnerDied,
		"runner aborted":                `{"error":"llama-server process has terminated: signal: aborted (core dumped)"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, url := serveInTurn(t, replyJSON(http.StatusInternalServerError, body), replyJSON(http.StatusOK, threeAnswers))
			resp, err := fastRetryDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble", Questions: threeQuestions()})
			if err != nil {
				t.Fatalf("Decide: %v (a runner death must be retried, not returned)", err)
			}
			if got := resp.Answers["route"].Choice; got == nil || *got != "billing" {
				t.Errorf("route = %v, want the retried call's answer", got)
			}
			if resp.Usage.InputTokens != 909 {
				t.Errorf("usage = %+v, want the retried call's usage", resp.Usage)
			}
			if got := s.calls(); got != 2 {
				t.Fatalf("requests = %d, want 2 (the call and one retry)", got)
			}
			if !reflect.DeepEqual(s.bodies[0], s.bodies[1]) {
				t.Errorf("the retry sent %v, want the first request's body %v", s.bodies[1], s.bodies[0])
			}
		})
	}
}

func TestOllama_RunnerDeathTwiceFailsAfterOneRetry(t *testing.T) {
	s, url := serveInTurn(t, replyJSON(http.StatusInternalServerError, runnerDied))
	_, err := fastRetryDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble", Questions: threeQuestions()})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeCallFailed || !strings.Contains(e.Message, "HTTP 500") {
		t.Fatalf("err = %v, want the 500 as a call failure", err)
	}
	if got := s.calls(); got != 2 {
		t.Errorf("requests = %d, want 2 (one retry, no more)", got)
	}
}

func TestOllama_ErrorThatIsNotARunnerDeathIsNotRetried(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"500 that names no runner": {http.StatusInternalServerError, `{"error":"model requires more system memory than is available"}`},
		"404 model not found":      {http.StatusNotFound, `{"error":"model 'nimble' not found"}`},
		"400 bad question":         {http.StatusBadRequest, `{"error":"question \"route\": type must be one of choice, noul, score"}`},
		"400 naming the runner":    {http.StatusBadRequest, `{"error":"question \"llama-server\": type must be one of choice, noul, score"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, url := serveInTurn(t, replyJSON(tc.status, tc.body), replyJSON(http.StatusOK, threeAnswers))
			_, err := fastRetryDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble", Questions: threeQuestions()})
			if err == nil {
				t.Fatal("Decide succeeded, want the refusal (it must not be retried into the next reply)")
			}
			if got := s.calls(); got != 1 {
				t.Errorf("requests = %d, want 1 (no retry)", got)
			}
		})
	}
}

// TestOllama_RunnerRetryStaysInsideTheCallTimeout — the wait before the retry
// counts against the call's own timeout, so a retry cannot make a call outlast it.
func TestOllama_RunnerRetryStaysInsideTheCallTimeout(t *testing.T) {
	s, url := serveInTurn(t, replyJSON(http.StatusInternalServerError, runnerDied), replyJSON(http.StatusOK, threeAnswers))
	d := newDriver(t, Options{BaseURL: url, Timeout: 50 * time.Millisecond})
	d.(*ollamaDriver).runnerRetryWait = time.Hour
	start := time.Now()
	_, err := d.Decide(context.Background(), Request{Model: "nimble", Questions: threeQuestions()})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeTimeout {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Decide took %v, want it cut off at the call timeout", took)
	}
	if got := s.calls(); got != 1 {
		t.Errorf("requests = %d, want 1 (timed out before the retry was sent)", got)
	}
}
