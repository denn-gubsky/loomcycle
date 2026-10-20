package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// fakeAuth stands in for the AWS authentication: it marks each request it
// authenticates and counts what the driver asked of it.
type fakeAuth struct {
	applied, invalidated atomic.Int32
	err                  error
	models               []string
}

func (f *fakeAuth) Apply(_ context.Context, r *http.Request, body []byte, _ func(*http.Request, string)) error {
	if f.err != nil {
		return f.err
	}
	n := f.applied.Add(1)
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 signed-%d-over-%d-bytes", n, len(body)))
	return nil
}
func (f *fakeAuth) Invalidate() { f.invalidated.Add(1) }
func (f *fakeAuth) Classify(_ int, b []byte) (bool, string) {
	return strings.Contains(string(b), "ExpiredToken"), ""
}
func (f *fakeAuth) ListModels(context.Context) ([]string, error) { return f.models, nil }

var okFrames = []string{
	`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
	"data: [DONE]\n\n",
}

// bedrockServer answers with the scripted statuses in order (200 = the SSE
// stream), recording each request's auth headers.
func bedrockServer(t *testing.T, statuses []int, bodies []string) (*httptest.Server, *[]http.Header) {
	t.Helper()
	var seen []http.Header
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		seen = append(seen, r.Header.Clone())
		i := int(n.Add(1)) - 1
		if i < len(statuses) && statuses[i] != 200 {
			w.WriteHeader(statuses[i])
			fmt.Fprint(w, bodies[i])
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range okFrames {
			fmt.Fprint(w, f)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func authDriver(url string, a providers.RequestAuth) *Driver {
	d := New("", url, streamhttp.Options{}, nil)
	d.SetKeyEnvName("AWS_BEARER_TOKEN_BEDROCK")
	d.auth = a
	return d
}

func drain(t *testing.T, ch <-chan providers.Event, err error) error {
	t.Helper()
	if err != nil {
		return err
	}
	for ev := range ch {
		if ev.Type == providers.EventError {
			return errors.New(ev.Error)
		}
	}
	return nil
}

var hiReq = providers.Request{Model: "openai.gpt-oss-20b-1:0", Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}}}

// The operator's call is authenticated by the RequestAuth and carries no key.
func TestCall_OperatorCallIsAuthenticatedByRequestAuth(t *testing.T) {
	srv, seen := bedrockServer(t, nil, nil)
	a := &fakeAuth{}
	if err := callAndDrain(t, authDriver(srv.URL, a), context.Background()); err != nil {
		t.Fatal(err)
	}
	h := (*seen)[0]
	if !strings.HasPrefix(h.Get("Authorization"), "AWS4-HMAC-SHA256 signed-1") {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
}

func callAndDrain(t *testing.T, d *Driver, ctx context.Context) error {
	t.Helper()
	ch, err := d.Call(ctx, hiReq)
	return drain(t, ch, err)
}

// A tenant's own Bedrock key, stored under AWS_BEARER_TOKEN_BEDROCK, is sent
// as the key, and the operator's AWS credentials are not used.
func TestCall_TenantKeyWinsOverRequestAuth(t *testing.T) {
	srv, seen := bedrockServer(t, nil, nil)
	a := &fakeAuth{}
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		return providers.CredentialResolution{Value: "tenant-bedrock-key", Scope: "tenant"}, name == "AWS_BEARER_TOKEN_BEDROCK", nil
	})
	if err := callAndDrain(t, authDriver(srv.URL, a), ctx); err != nil {
		t.Fatal(err)
	}
	if a.applied.Load() != 0 || (*seen)[0].Get("Authorization") != "Bearer tenant-bedrock-key" {
		t.Errorf("applied=%d Authorization=%q", a.applied.Load(), (*seen)[0].Get("Authorization"))
	}
}

// A run restricted from the operator's credentials (RFC AX) is refused before
// anything is signed: AWS credentials are the operator's, as a host key is.
func TestCall_RestrictedRunIsRefusedBeforeSigning(t *testing.T) {
	srv, seen := bedrockServer(t, nil, nil)
	a := &fakeAuth{}
	_, err := authDriver(srv.URL, a).Call(providers.WithOperatorKeyAllowed(context.Background(), false), hiReq)
	if !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		t.Fatalf("err = %v, want ErrOperatorKeyForbidden", err)
	}
	if a.applied.Load() != 0 || len(*seen) != 0 {
		t.Errorf("signed %d, sent %d", a.applied.Load(), len(*seen))
	}
}

// An answer that the credentials expired drops them and retries once, signed
// afresh; a second expiry is returned, not retried again.
func TestCall_ExpiredCredentialsRetryOnceWithFreshOnes(t *testing.T) {
	srv, seen := bedrockServer(t, []int{403}, []string{`{"message":"ExpiredTokenException"}`})
	a := &fakeAuth{}
	if err := callAndDrain(t, authDriver(srv.URL, a), context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.invalidated.Load() != 1 || len(*seen) != 2 || (*seen)[0].Get("Authorization") == (*seen)[1].Get("Authorization") {
		t.Errorf("invalidated=%d sends=%d", a.invalidated.Load(), len(*seen))
	}

	srv2, seen2 := bedrockServer(t, []int{403, 403}, []string{`ExpiredToken`, `ExpiredToken`})
	if err := callAndDrain(t, authDriver(srv2.URL, &fakeAuth{}), context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("second expiry: err = %v", err)
	}
	if len(*seen2) != 2 {
		t.Errorf("sends = %d, want 2", len(*seen2))
	}
}

// A denial is permanent: no refresh, no retry, and its body reaches the error.
func TestCall_DenialIsNotRetried(t *testing.T) {
	body := `{"error":{"message":"not authorized to perform: bedrock:InvokeModel","type":"permission_denied_error","code":"access_denied"}}`
	srv, seen := bedrockServer(t, []int{401}, []string{body}) // runtime /openai denies with 401 (P0)
	a := &fakeAuth{}
	err := callAndDrain(t, authDriver(srv.URL, a), context.Background())
	if err == nil || !strings.Contains(err.Error(), "bedrock:InvokeModel") {
		t.Fatalf("err = %v", err)
	}
	if a.invalidated.Load() != 0 || len(*seen) != 1 {
		t.Errorf("invalidated=%d sends=%d", a.invalidated.Load(), len(*seen))
	}
}

// Credentials that cannot be obtained fail the call with the reason.
func TestCall_NoCredentialsFailsTheCall(t *testing.T) {
	srv, seen := bedrockServer(t, nil, nil)
	a := &fakeAuth{err: errors.New("aws credentials unavailable: no EC2 IMDS role found")}
	err := callAndDrain(t, authDriver(srv.URL, a), context.Background())
	if !errors.Is(err, providers.ErrAuthUnavailable) || !strings.Contains(err.Error(), "aws credentials unavailable") || len(*seen) != 0 {
		t.Errorf("err = %v, sends = %d", err, len(*seen))
	}
}

// Bedrock lists models outside its Chat Completions route: the listing comes
// from the RequestAuth, and nothing is sent to the base URL.
func TestListModels_ComesFromRequestAuth(t *testing.T) {
	srv, seen := bedrockServer(t, nil, nil)
	d := authDriver(srv.URL, &fakeAuth{models: []string{"openai.gpt-oss-20b-1:0"}})
	got, err := d.ListModels(context.Background())
	if err != nil || len(got) != 1 || got[0] != "openai.gpt-oss-20b-1:0" || len(*seen) != 0 {
		t.Errorf("models = %v, err = %v, sends = %d", got, err, len(*seen))
	}
}
