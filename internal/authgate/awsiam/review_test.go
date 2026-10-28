package awsiam

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

// Only signed headers are forwarded to STS, so a header that changes how
// the answer travels (an encoding, a hop-by-hop header) is one the caller
// chose and signed: refused before STS is asked, as a bad credential the
// throttle counts.
func TestGate_SignedTransportHeaderIsRefusedBeforeSTS(t *testing.T) {
	for _, h := range []string{"Accept-Encoding", "Connection", "Te", "Transfer-Encoding", "Upgrade", "Expect"} {
		f := newFakeSTS(t)
		d := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", stsBody, t0, func(r *http.Request) {
			r.Header.Set(h, "gzip")
		})
		_, err := f.gate().Verify(context.Background(), d)
		if !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Errorf("%s signed: Verify = %v, want ErrInvalidCredential", h, err)
		}
		if f.calls.Load() != 0 {
			t.Errorf("%s signed: STS was asked", h)
		}
	}
}

// A 200 the gate cannot read is not STS being down: the answer came back,
// and its shape is what the caller's signed headers chose. Counted as a
// refusal, not answered 503 (which counted nothing and left the request
// replayable).
func TestGate_UnreadableOKAnswerIsARefusal(t *testing.T) {
	f := newFakeSTS(t)
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("\x1f\x8b not an identity"))
	}))
	t.Cleanup(odd.Close)
	g := f.gate()
	g.HTTP, g.endpoint = odd.Client(), func(string) string { return odd.URL + "/" }
	_, err := g.Verify(context.Background(), validLogin(t))
	if !errors.Is(err, authgate.ErrInvalidCredential) || errors.Is(err, ErrSTSUnavailable) {
		t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
	}
}

// The shared replay record being down is the server's fault, and not STS
// being down: its own error, so the caller is neither blamed (counted) nor
// told STS is unreachable.
func TestGate_SharedRecordFaultIsItsOwnError(t *testing.T) {
	f := newFakeSTS(t)
	g := f.gate()
	g.Shared, g.SharedScope = &sharedRecord{err: errors.New("db down")}, "awsiam:ci"
	_, err := g.Verify(context.Background(), validLogin(t))
	if !errors.Is(err, ErrRecordUnavailable) || errors.Is(err, authgate.ErrInvalidCredential) || errors.Is(err, ErrSTSUnavailable) {
		t.Fatalf("Verify = %v, want ErrRecordUnavailable only", err)
	}
}
