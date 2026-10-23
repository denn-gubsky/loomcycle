// The Bedrock embedder (internal/providers/bedrock) authenticated by the real
// AWS auth (internal/awsauth), the kind EmbedderAuth hands it: what goes on the
// wire, how missing and expired credentials are handled, that no secret leaks,
// and that the query mode survives the server's wrappers (lanegate). These
// tests used to live in the bedrock package, which is in the internal/providers
// module and cannot import awsauth or lanegate, both root packages. The
// embedder's own behaviour is still tested there, with a stand-in auth.
package providerbuild_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awsauth"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/bedrock"
)

const (
	testRegion  = "eu-central-1"
	testBaseURL = "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic"
	testToken   = "FAKE-SESSION-TOKEN-must-never-be-echoed"
	testSecret  = "fake-secret-access-key-0123456789"
)

// recorded is one request as it left the embedder (after signing).
type recorded struct {
	requestURI string
	header     http.Header
	body       invokeBody
	rawBody    map[string]any
}

// invokeBody is the InvokeModel request body as the fake reads it (bedrock's
// own type is unexported).
type invokeBody struct {
	Texts           []string `json:"texts"`
	InputType       string   `json:"input_type"`
	Truncate        string   `json:"truncate"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

// fakeBedrock is an InvokeModel stand-in. The embedder keeps its real
// bedrock-runtime URL; redirect (an http.RoundTripper) sends each request here
// unchanged but for scheme and host, so the path on the wire is the one the
// embedder built.
type fakeBedrock struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	reqs []recorded

	// answer, when set, decides each response; nil = 200 with floats.
	answer func(n int, b invokeBody) (status int, header http.Header, body string)
	// shape is the 200 response shape: "floats" (default), "by_type", or
	// "no_type" (a floats list with no response_type).
	shape string
	// width overrides the vector width answered; 0 = the request's
	// output_dimension, else the model's own width.
	width int
}

func newFakeBedrock(t *testing.T) *fakeBedrock {
	f := &fakeBedrock{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBedrock) serve(w http.ResponseWriter, r *http.Request) {
	var b invokeBody
	var raw map[string]any
	data := readAll(r)
	_ = json.Unmarshal(data, &b)
	_ = json.Unmarshal(data, &raw)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{requestURI: r.RequestURI, header: r.Header.Clone(), body: b, rawBody: raw})
	n := len(f.reqs)
	f.mu.Unlock()
	if f.answer != nil {
		if status, h, body := f.answer(n, b); status != 0 {
			for k, vs := range h {
				w.Header()[k] = vs
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
	}
	width := f.width
	if width == 0 {
		width = b.OutputDimension
	}
	if width == 0 {
		width = 1024 // v3, fixed
		if strings.Contains(r.RequestURI, "embed-v4") {
			width = 1536 // v4's default
		}
	}
	vecs := make([][]float32, len(b.Texts))
	for i, text := range b.Texts {
		v := make([]float32, width)
		// The text's number rides in the vector, so a test can check order.
		if k, err := strconv.Atoi(strings.TrimPrefix(text, "t")); err == nil {
			v[0] = float32(k)
		}
		vecs[i] = v
	}
	var out map[string]any
	switch f.shape {
	case "by_type":
		out = map[string]any{"response_type": "embeddings_by_type", "embeddings": map[string]any{"float": vecs}, "texts": b.Texts}
	case "no_type":
		out = map[string]any{"embeddings": vecs}
	default:
		out = map[string]any{"response_type": "embeddings_floats", "embeddings": vecs, "texts": b.Texts}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func readAll(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

func ioNopCloser(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// independentSigV4 derives a SigV4 signature from its definition, apart from
// awsauth, so the signature test does not grade the signer by itself.
func independentSigV4(secret, day, region, service, stringToSign string) string {
	mac := func(key []byte, data string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(data))
		return m.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), day)
	k = mac(k, region)
	k = mac(k, service)
	k = mac(k, "aws4_request")
	return hex.EncodeToString(mac(k, stringToSign))
}

func (f *fakeBedrock) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// client returns an http.Client that delivers every request to the fake.
func (f *fakeBedrock) client() *http.Client {
	target, _ := url.Parse(f.srv.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || !strings.HasPrefix(r.URL.Host, "bedrock-runtime.") {
			return nil, fmt.Errorf("request left for %s, want the bedrock-runtime host", r.URL.Host)
		}
		out := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		out.URL = &u
		out.Host = ""
		return http.DefaultTransport.RoundTrip(out)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// staticSource hands out fixed temporary credentials and counts invalidations.
type staticSource struct {
	invalidated atomic.Int32
	err         error
}

func (s *staticSource) Retrieve(context.Context) (awsauth.Credentials, error) {
	if s.err != nil {
		return awsauth.Credentials{}, s.err
	}
	return awsauth.Credentials{AccessKeyID: "AKIDBEDROCKTEST", SecretAccessKey: testSecret, SessionToken: testToken, Source: "test"}, nil
}
func (s *staticSource) Invalidate() { s.invalidated.Add(1) }

func sigv4Auth(src awsauth.CredentialSource, mode string) *awsauth.Auth {
	return awsauth.NewWithSource(awsauth.Config{Mode: mode, Region: testRegion, BaseURL: testBaseURL}, src)
}

// newTest builds an embedder against f with the real AWS auth on src. Its
// retries wait the embedder's own backoff (250 ms, then 1 s): the override
// bedrock's in-package tests use is unexported.
func newTest(t *testing.T, f *fakeBedrock, model string, dims, batch int, auth providers.RequestAuth) *bedrock.Embedder {
	t.Helper()
	if auth == nil {
		auth = sigv4Auth(&staticSource{}, awsauth.ModeSigV4)
	}
	e, err := bedrock.New(providers.EmbedderOptions{
		Model: model, Dimensions: dims, BatchSize: batch, BaseURL: testBaseURL,
		Auth: auth, HTTPClient: f.client(), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestEmbed_V4RequestIsSignedInvokeModelWithEscapedModelID — the request on the
// wire: the model id's ':' as %3A, a SigV4-signed POST carrying the session
// token, and Cohere v4's body with no embedding_types.
func TestEmbed_V4RequestIsSignedInvokeModelWithEscapedModelID(t *testing.T) {
	f := newFakeBedrock(t)
	e := newTest(t, f, "cohere.embed-v4:0", 1024, 0, nil)
	if _, err := e.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.requestURI != "/model/cohere.embed-v4%3A0/invoke" {
		t.Errorf("wire path = %q, want /model/cohere.embed-v4%%3A0/invoke", r.requestURI)
	}
	if got := r.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	if got := r.header.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIDBEDROCKTEST/") || !strings.Contains(got, "/eu-central-1/bedrock/aws4_request") {
		t.Errorf("Authorization = %q, want a SigV4 signature for eu-central-1/bedrock", got)
	}
	if r.header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date missing")
	}
	if r.header.Get("X-Amz-Security-Token") != testToken {
		t.Error("X-Amz-Security-Token missing or wrong")
	}
	want := map[string]any{"texts": []any{"hello"}, "input_type": "search_document", "truncate": "RIGHT", "output_dimension": float64(1024)}
	if fmt.Sprint(r.rawBody) != fmt.Sprint(want) {
		t.Errorf("body = %v, want %v", r.rawBody, want)
	}
}

// fixedClockAuth signs with awsauth's signer at a fixed instant, so the
// signature is a known value.
type fixedClockAuth struct{ now time.Time }

func (a fixedClockAuth) Apply(_ context.Context, req *http.Request, body []byte, _ func(*http.Request, string)) error {
	awsauth.SignRequest(req, body, awsauth.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: "session"}, "us-east-1", "bedrock", a.now)
	return nil
}
func (fixedClockAuth) Invalidate()                                  {}
func (fixedClockAuth) Classify(int, []byte) (bool, string)          { return false, "" }
func (fixedClockAuth) ListModels(context.Context) ([]string, error) { return nil, nil }

// TestEmbed_SignatureCoversTheDoubleEncodedModelPath — AWS computes the
// canonical URI by encoding the path it received once more, so a model id
// with ':' sent as %3A is signed as %253A. The expected signature is computed
// here independently of the embedder, from that canonical request; a raw ':'
// on the wire signs a different path and fails.
func TestEmbed_SignatureCoversTheDoubleEncodedModelPath(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var got *http.Request
	var gotBody []byte
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		gotBody = readAll(r)
		vec := make([]string, 1536)
		for i := range vec {
			vec[i] = "0"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
			Body: ioNopCloser(`{"response_type":"embeddings_floats","embeddings":[[` + strings.Join(vec, ",") + `]]}`)}, nil
	})}
	e, err := bedrock.New(providers.EmbedderOptions{Model: "cohere.embed-v4:0", BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		Auth: fixedClockAuth{now: now}, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(context.Background(), []string{"hi"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if p := got.URL.EscapedPath(); p != "/model/cohere.embed-v4%3A0/invoke" {
		t.Fatalf("escaped path = %q", p)
	}
	canonical := strings.Join([]string{
		"POST",
		"/model/cohere.embed-v4%253A0/invoke",
		"",
		"accept:application/json\ncontent-type:application/json\nhost:bedrock-runtime.us-east-1.amazonaws.com\nx-amz-date:20261022T120000Z\nx-amz-security-token:session\n",
		"accept;content-type;host;x-amz-date;x-amz-security-token",
		sha256Hex(gotBody),
	}, "\n")
	want := independentSigV4("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20261022", "us-east-1", "bedrock",
		"AWS4-HMAC-SHA256\n20261022T120000Z\n20261022/us-east-1/bedrock/aws4_request\n"+sha256Hex([]byte(canonical)))
	auth := got.Header.Get("Authorization")
	if !strings.HasSuffix(auth, "Signature="+want) {
		t.Fatalf("Authorization = %q\nwant signature %s over canonical path /model/cohere.embed-v4%%253A0/invoke", auth, want)
	}
}

// TestEmbed_StoredTextIsADocumentAndASearchIsAQuery — Embed sends
// search_document and EmbedQuery search_query, also through the server's
// wrappers (the provider's lane, then the health observer).
func TestEmbed_StoredTextIsADocumentAndASearchIsAQuery(t *testing.T) {
	f := newFakeBedrock(t)
	e := newTest(t, f, "eu.cohere.embed-v4:0", 1024, 0, nil)
	wrapped := providers.ObserveEmbedder(lanegate.WrapEmbedder("bedrock-claude", e, nil))
	ctx := context.Background()
	steps := []struct {
		name string
		call func() error
		want string
	}{
		{"driver Embed", func() error { _, err := e.Embed(ctx, []string{"a"}); return err }, "search_document"},
		{"driver EmbedQuery", func() error { _, err := e.EmbedQuery(ctx, []string{"a"}); return err }, "search_query"},
		{"wrapped Embed", func() error { _, err := wrapped.Embed(ctx, []string{"a"}); return err }, "search_document"},
		{"wrapped EmbedQuery", func() error { _, err := providers.EmbedQuery(ctx, wrapped, []string{"a"}); return err }, "search_query"},
	}
	for i, s := range steps {
		if err := s.call(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := f.requests()[i].body.InputType; got != s.want {
			t.Errorf("%s sent input_type %q, want %q", s.name, got, s.want)
		}
	}
	if h := wrapped.Health(); h.Calls != 2 || h.Failures != 0 {
		t.Errorf("observer = %+v, want the two wrapped calls counted", h)
	}
}

func awsError(errType, msg string) (http.Header, string) {
	return http.Header{"X-Amzn-Errortype": {errType + ":http://internal.amazon.com/coral/com.amazon.bedrock/"}}, `{"message":"` + msg + `"}`
}

// TestEmbed_RetriesOnlyTransientAnswers — the retry matrix: throttles and
// transient server errors are tried up to 3 times; a client error, a denial,
// a missing model and a model error are not retried.
func TestEmbed_RetriesOnlyTransientAnswers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int // answers in order; past the end = 200
		errType   string
		wantCalls int
		wantErr   bool
	}{
		{"429 then 200", []int{429}, "ThrottlingException", 2, false},
		{"408 then 200", []int{408}, "ModelTimeoutException", 2, false},
		{"500 then 200", []int{500}, "InternalServerException", 2, false},
		{"503 three times", []int{503, 503, 503, 503}, "ServiceUnavailableException", 3, true},
		{"400 once", []int{400, 400}, "ValidationException", 1, true},
		{"403 access denied once", []int{403, 403}, "AccessDeniedException", 1, true},
		{"404 once", []int{404, 404}, "ResourceNotFoundException", 1, true},
		{"424 once", []int{424, 424}, "ModelErrorException", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBedrock(t)
			f.answer = func(n int, _ invokeBody) (int, http.Header, string) {
				if n > len(tc.statuses) {
					return 0, nil, ""
				}
				h, body := awsError(tc.errType, "the "+tc.errType+" message")
				return tc.statuses[n-1], h, body
			}
			src := &staticSource{}
			e := newTest(t, f, "cohere.embed-v4:0", 0, 0, sigv4Auth(src, awsauth.ModeSigV4))
			_, err := e.Embed(context.Background(), []string{"private text 1", "private text 2"})
			if got := len(f.requests()); got != tc.wantCalls {
				t.Errorf("%d calls, want %d", got, tc.wantCalls)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if src.invalidated.Load() != 0 {
				t.Errorf("credentials invalidated %d times on a non-expiry answer", src.invalidated.Load())
			}
			if err == nil {
				return
			}
			msg := err.Error()
			for _, want := range []string{`"cohere.embed-v4:0"`, testRegion, strconv.Itoa(tc.statuses[0]), tc.errType, "the " + tc.errType + " message"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name %s", msg, want)
				}
			}
			if strings.Contains(msg, "private text") {
				t.Errorf("error echoes an input text: %q", msg)
			}
		})
	}
}

// TestEmbed_ExpiredCredentialsAreRefreshedOnce — an expired-token 403 drops the
// cached credentials and is sent once more; a second expiry is final.
func TestEmbed_ExpiredCredentialsAreRefreshedOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expired   int
		wantCalls int
		wantErr   bool
	}{
		{"expired then ok", 1, 2, false},
		{"expired twice", 2, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBedrock(t)
			f.answer = func(n int, _ invokeBody) (int, http.Header, string) {
				if n <= tc.expired {
					h, body := awsError("ExpiredTokenException", "The security token included in the request is expired")
					return 403, h, body
				}
				return 0, nil, ""
			}
			src := &staticSource{}
			e := newTest(t, f, "cohere.embed-v4:0", 0, 0, sigv4Auth(src, awsauth.ModeSigV4))
			_, err := e.Embed(context.Background(), []string{"x"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if got := len(f.requests()); got != tc.wantCalls {
				t.Errorf("%d calls, want %d", got, tc.wantCalls)
			}
			if src.invalidated.Load() != 1 {
				t.Errorf("credentials invalidated %d times, want 1", src.invalidated.Load())
			}
		})
	}
}

// TestEmbed_NoCredentialsIsAuthUnavailable — a chain with nothing to offer
// fails as ErrAuthUnavailable (permanent), naming model and region, and sends
// nothing.
func TestEmbed_NoCredentialsIsAuthUnavailable(t *testing.T) {
	f := newFakeBedrock(t)
	e := newTest(t, f, "cohere.embed-v4:0", 0, 0, sigv4Auth(&staticSource{err: awsauth.ErrNoCredentials}, awsauth.ModeSigV4))
	_, err := e.Embed(context.Background(), []string{"private text"})
	if !errors.Is(err, providers.ErrAuthUnavailable) {
		t.Fatalf("Embed = %v, want ErrAuthUnavailable", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "cohere.embed-v4:0") || !strings.Contains(msg, testRegion) || strings.Contains(msg, "private text") {
		t.Errorf("error = %q, want model and region and no input", msg)
	}
	if got := len(f.requests()); got != 0 {
		t.Errorf("%d requests sent without credentials", got)
	}
}

// TestEmbed_BedrockTokenModeSendsABearerKeyAndNoSignature — auth type
// aws_bedrock_token: a minted key as Authorization: Bearer, no SigV4 headers.
func TestEmbed_BedrockTokenModeSendsABearerKeyAndNoSignature(t *testing.T) {
	f := newFakeBedrock(t)
	e := newTest(t, f, "cohere.embed-multilingual-v3", 0, 0, sigv4Auth(&staticSource{}, awsauth.ModeBedrockToken))
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	h := f.requests()[0].header
	if got := h.Get("Authorization"); !strings.HasPrefix(got, "Bearer bedrock-api-key-") {
		t.Errorf("Authorization is not a Bearer Bedrock key (prefix %.20q)", got)
	}
	for _, k := range []string{"X-Amz-Date", "X-Amz-Security-Token"} {
		if h.Get(k) != "" {
			t.Errorf("%s sent in token mode", k)
		}
	}
}

// TestEmbed_CredentialsReachTheRedactorAndNoErrorOrLog — every secret the
// chain hands over is registered with the server's redactor, and none appears
// in the error or a log line of a failed call, even when AWS answers a
// signature failure with its canonical request (which carries the session
// token) in the message.
func TestEmbed_CredentialsReachTheRedactorAndNoErrorOrLog(t *testing.T) {
	var logs bytes.Buffer
	prevSlog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	prevLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { slog.SetDefault(prevSlog); log.SetOutput(prevLog) })

	f := newFakeBedrock(t)
	f.answer = func(int, invokeBody) (int, http.Header, string) {
		h, _ := awsError("InvalidSignatureException", "")
		msg := "Signature mismatch.\n\nThe Canonical String for this request should have been\n'POST\n/model/x\n\nx-amz-security-token:" + testToken + "'"
		body, _ := json.Marshal(map[string]string{"message": msg})
		return 403, h, string(body)
	}
	var sunk []string
	auth := sigv4Auth(&staticSource{}, awsauth.ModeSigV4)
	auth.SetSecretSink(func(v string) { sunk = append(sunk, v) })
	e := newTest(t, f, "cohere.embed-v4:0", 0, 0, auth)

	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "InvalidSignatureException") || !strings.Contains(err.Error(), "Signature mismatch.") {
		t.Fatalf("Embed = %v, want the signature failure", err)
	}
	for _, secret := range []string{testToken, testSecret} {
		if !slices.Contains(sunk, secret) {
			t.Errorf("a credential never reached the redactor (%d values did)", len(sunk))
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error carries a credential: %q", err)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("a log line carries a credential")
		}
	}
}
