package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const testBaseURL = "https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic"

// recorded is one request as it left the embedder (after signing).
type recorded struct {
	requestURI string
	header     http.Header
	body       invokeBody
	rawBody    map[string]any
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
	// tokensPerText, when set, is answered as InvokeModel's input token count
	// header, per text in the request.
	tokensPerText int
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
	if f.tokensPerText > 0 {
		w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", strconv.Itoa(f.tokensPerText*len(b.Texts)))
	}
	_ = json.NewEncoder(w).Encode(out)
}

// TestEmbed_ReportsTheInputTokenCountHeader — Bedrock reports what each
// InvokeModel consumed in a header; every batch's count reaches the call's
// accumulator, and an answer without the header reports nothing.
func TestEmbed_ReportsTheInputTokenCountHeader(t *testing.T) {
	f := newFakeBedrock(t)
	f.tokensPerText = 7
	e := newTest(t, f, "cohere.embed-multilingual-v3", 0, 2, nil)
	ctx, acc := providers.WithEmbedTokens(context.Background())
	if _, err := e.Embed(ctx, texts(5)); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n, ok := acc.Total(); !ok || n != 35 {
		t.Errorf("reported tokens = %d (%v), want 35 across the three batches", n, ok)
	}

	plain := newFakeBedrock(t)
	e = newTest(t, plain, "cohere.embed-multilingual-v3", 0, 0, nil)
	ctx, acc = providers.WithEmbedTokens(context.Background())
	if _, err := e.Embed(ctx, texts(2)); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n, ok := acc.Total(); ok {
		t.Errorf("an answer with no count header reported %d tokens", n)
	}
}

func readAll(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
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

// keyAuth sends a fixed key through the driver's own setKey. These tests are
// about the embedder; what AWS authentication puts on its requests is tested
// with the real signer in internal/providerbuild (bedrock_embedder_test.go),
// because internal/awsauth is a root package this module cannot import.
type keyAuth struct{}

func (keyAuth) Apply(_ context.Context, req *http.Request, _ []byte, setKey func(*http.Request, string)) error {
	setKey(req, "test-key")
	return nil
}
func (keyAuth) Invalidate()                                  {}
func (keyAuth) Classify(int, []byte) (bool, string)          { return false, "" }
func (keyAuth) ListModels(context.Context) ([]string, error) { return nil, nil }

// newTest builds an embedder against f, authenticated by auth or, when nil,
// keyAuth.
func newTest(t *testing.T, f *fakeBedrock, model string, dims, batch int, auth providers.RequestAuth) *Embedder {
	t.Helper()
	if auth == nil {
		auth = keyAuth{}
	}
	e, err := New(providers.EmbedderOptions{
		Model: model, Dimensions: dims, BatchSize: batch, BaseURL: testBaseURL,
		Auth: auth, HTTPClient: f.client(), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.waits = []time.Duration{time.Millisecond, time.Millisecond}
	return e
}

func texts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strconv.Itoa(i)
	}
	return out
}

// TestEmbed_BodyFieldsFollowTheModelGeneration — v3 truncates with END and
// takes no output_dimension; v4 sends output_dimension only when asked for one.
func TestEmbed_BodyFieldsFollowTheModelGeneration(t *testing.T) {
	for _, tc := range []struct {
		model    string
		dims     int
		truncate string
		wantDim  bool
	}{
		{"cohere.embed-multilingual-v3", 0, "END", false},
		{"cohere.embed-multilingual-v3", 1024, "END", false},
		{"eu.cohere.embed-v4:0", 0, "RIGHT", false},
		{"global.cohere.embed-v4:0", 256, "RIGHT", true},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.model, tc.dims), func(t *testing.T) {
			f := newFakeBedrock(t)
			e := newTest(t, f, tc.model, tc.dims, 0, nil)
			if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			r := f.requests()[0]
			if r.rawBody["truncate"] != tc.truncate {
				t.Errorf("truncate = %v, want %s", r.rawBody["truncate"], tc.truncate)
			}
			if _, has := r.rawBody["output_dimension"]; has != tc.wantDim {
				t.Errorf("output_dimension present = %v, want %v (body %v)", has, tc.wantDim, r.rawBody)
			}
			if _, has := r.rawBody["embedding_types"]; has {
				t.Errorf("embedding_types sent: %v", r.rawBody)
			}
		})
	}
}

// TestEmbed_ReadsBothResponseShapes — Cohere answers floats as a list
// (embeddings_floats) or as a map by type (embeddings_by_type); both are read,
// as is a list with no response_type.
func TestEmbed_ReadsBothResponseShapes(t *testing.T) {
	for _, model := range []string{"cohere.embed-multilingual-v3", "eu.cohere.embed-v4:0"} {
		for _, shape := range []string{"floats", "by_type", "no_type"} {
			t.Run(model+"/"+shape, func(t *testing.T) {
				f := newFakeBedrock(t)
				f.shape = shape
				e := newTest(t, f, model, 0, 0, nil)
				vecs, err := e.Embed(context.Background(), texts(3))
				if err != nil {
					t.Fatalf("Embed: %v", err)
				}
				if len(vecs) != 3 || len(vecs[2]) != e.Dimension() || vecs[2][0] != 2 {
					t.Fatalf("got %d vectors (width %d), want 3 of width %d in order", len(vecs), len(vecs[0]), e.Dimension())
				}
			})
		}
	}
}

// TestEmbed_RefusesAnAnswerThatDoesNotMatchTheRequest — a short answer would
// pair later texts with the wrong vectors, and a different width would corrupt
// the store's dimension bookkeeping: both fail the call.
func TestEmbed_RefusesAnAnswerThatDoesNotMatchTheRequest(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		f := newFakeBedrock(t)
		f.answer = func(_ int, b invokeBody) (int, http.Header, string) {
			return 200, nil, `{"response_type":"embeddings_floats","embeddings":[[` + strings.TrimSuffix(strings.Repeat("0,", 1024), ",") + `]]}`
		}
		e := newTest(t, f, "cohere.embed-multilingual-v3", 0, 0, nil)
		if _, err := e.Embed(context.Background(), texts(2)); err == nil || !strings.Contains(err.Error(), "1 embeddings for 2 inputs") {
			t.Fatalf("Embed = %v, want a count mismatch", err)
		}
	})
	t.Run("width", func(t *testing.T) {
		f := newFakeBedrock(t)
		f.width = 512
		e := newTest(t, f, "cohere.embed-v4:0", 1024, 0, nil)
		if _, err := e.Embed(context.Background(), texts(1)); err == nil || !strings.Contains(err.Error(), "512-dimensional, expected 1024") {
			t.Fatalf("Embed = %v, want a width mismatch", err)
		}
	})
}

// TestEmbed_BatchesAtMost96InOrder — Cohere takes 96 texts per call; a smaller
// BatchSize is honoured; the vectors come back in input order.
func TestEmbed_BatchesAtMost96InOrder(t *testing.T) {
	for _, tc := range []struct {
		batch int
		want  []int
	}{
		{0, []int{96, 96, 58}},
		{500, []int{96, 96, 58}},
		{40, []int{40, 40, 40, 40, 40, 40, 10}},
	} {
		t.Run(strconv.Itoa(tc.batch), func(t *testing.T) {
			f := newFakeBedrock(t)
			e := newTest(t, f, "cohere.embed-multilingual-v3", 0, tc.batch, nil)
			vecs, err := e.Embed(context.Background(), texts(250))
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			var sizes []int
			for _, r := range f.requests() {
				sizes = append(sizes, len(r.body.Texts))
			}
			if fmt.Sprint(sizes) != fmt.Sprint(tc.want) {
				t.Errorf("batch sizes = %v, want %v", sizes, tc.want)
			}
			for i, v := range vecs {
				if v[0] != float32(i) {
					t.Fatalf("vector %d belongs to text %v: order lost", i, v[0])
				}
			}
		})
	}
}

func awsError(errType, msg string) (http.Header, string) {
	return http.Header{"X-Amzn-Errortype": {errType + ":http://internal.amazon.com/coral/com.amazon.bedrock/"}}, `{"message":"` + msg + `"}`
}

// TestEmbed_RetryAfterIsHonoured — a throttle's Retry-After sets the wait.
func TestEmbed_RetryAfterIsHonoured(t *testing.T) {
	f := newFakeBedrock(t)
	f.answer = func(n int, _ invokeBody) (int, http.Header, string) {
		if n == 1 {
			h, body := awsError("ThrottlingException", "slow down")
			h.Set("Retry-After", "1")
			return 429, h, body
		}
		return 0, nil, ""
	}
	e := newTest(t, f, "cohere.embed-v4:0", 0, 0, nil)
	start := time.Now()
	if _, err := e.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Errorf("retried after %v, want the 1 s Retry-After", waited)
	}
}

// TestEmbed_RetryNeverOutlivesTheTimeout — a wait that would pass the call's
// deadline is not taken: the call fails with the answer it has.
func TestEmbed_RetryNeverOutlivesTheTimeout(t *testing.T) {
	f := newFakeBedrock(t)
	f.answer = func(int, invokeBody) (int, http.Header, string) {
		h, body := awsError("ThrottlingException", "slow down")
		h.Set("Retry-After", "5")
		return 429, h, body
	}
	e := newTest(t, f, "cohere.embed-v4:0", 0, 0, nil)
	e.timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Embed = %v, want the 429", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v, want it bounded by the 200 ms timeout", took)
	}
	if got := len(f.requests()); got != 1 {
		t.Errorf("%d calls, want 1", got)
	}
}

// TestNew_RefusesWhatBedrockCannotServe — construction refuses a model,
// width, missing auth or host the embedder cannot use, without a network call.
func TestNew_RefusesWhatBedrockCannotServe(t *testing.T) {
	auth := keyAuth{}
	ok := providers.EmbedderOptions{Model: "eu.cohere.embed-v4:0", BaseURL: testBaseURL, Auth: auth}
	if _, err := New(ok); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
	for _, tc := range []struct {
		name string
		mod  func(*providers.EmbedderOptions)
		want string
	}{
		{"no model", func(o *providers.EmbedderOptions) { o.Model = "" }, "Model is required"},
		{"titan", func(o *providers.EmbedderOptions) { o.Model = "amazon.titan-embed-text-v2:0" }, "not a Cohere Embed"},
		{"application profile ARN", func(o *providers.EmbedderOptions) {
			o.Model = "arn:aws:bedrock:eu-central-1:123456789012:application-inference-profile/abc"
		}, "ARNs are not supported"},
		{"v4 width", func(o *providers.EmbedderOptions) { o.Dimensions = 1000 }, "256, 512, 1024, 1536"},
		{"v3 width", func(o *providers.EmbedderOptions) { o.Model, o.Dimensions = "cohere.embed-multilingual-v3", 512 }, "0 or 1024"},
		{"no auth", func(o *providers.EmbedderOptions) { o.Auth = nil }, "auth:"},
		{"http", func(o *providers.EmbedderOptions) { o.BaseURL = "http://bedrock-runtime.eu-central-1.amazonaws.com" }, "https"},
		{"mantle", func(o *providers.EmbedderOptions) { o.BaseURL = "https://bedrock-mantle.eu-central-1.api.aws" }, "runtime host"},
		{"other host", func(o *providers.EmbedderOptions) {
			o.BaseURL = "https://bedrock-runtime.eu-central-1.amazonaws.com.evil.example"
		}, "runtime host"},
		{"port", func(o *providers.EmbedderOptions) {
			o.BaseURL = "https://bedrock-runtime.eu-central-1.amazonaws.com:8443"
		}, "runtime host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := ok
			tc.mod(&o)
			if _, err := New(o); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestNew_TakesOnlyTheHostOfTheProvidersEndpoint — the provider's chat route
// (/anthropic, /openai/v1) is dropped, and Dimension is fixed up front.
func TestNew_TakesOnlyTheHostOfTheProvidersEndpoint(t *testing.T) {
	for _, tc := range []struct {
		model   string
		dims    int
		wantDim int
	}{
		{"cohere.embed-v4:0", 0, 1536},
		{"eu.cohere.embed-v4:0", 512, 512},
		{"cohere.embed-multilingual-v3", 0, 1024},
		{"cohere.embed-english-v3", 1024, 1024},
	} {
		e, err := New(providers.EmbedderOptions{Model: tc.model, Dimensions: tc.dims,
			BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com/openai/v1", Auth: keyAuth{}})
		if err != nil {
			t.Fatalf("New(%s): %v", tc.model, err)
		}
		if want := "https://bedrock-runtime.eu-central-1.amazonaws.com/model/" + escapeModelID(tc.model) + "/invoke"; e.url != want {
			t.Errorf("url = %q, want %q", e.url, want)
		}
		if e.Dimension() != tc.wantDim || e.Provider() != "bedrock" || e.Model() != tc.model {
			t.Errorf("%s: dim=%d provider=%s model=%s", tc.model, e.Dimension(), e.Provider(), e.Model())
		}
	}
}

// TestValidator_IsRegisteredBesideTheConstructor — config load reaches the
// model and width rules through the registry.
func TestValidator_IsRegisteredBesideTheConstructor(t *testing.T) {
	if err := providers.ValidateEmbedder(Key, "eu.cohere.embed-v4:0", 1024); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for _, tc := range []struct {
		model string
		dims  int
		want  string
	}{
		{"cohere.embed-v4:0", 1000, "allowed: 0 = 1536, 256, 512, 1024, 1536"},
		{"cohere.embed-multilingual-v3", 512, "allowed: 0 or 1024"},
		{"amazon.titan-embed-text-v2:0", 0, "cohere.embed-v4:0, cohere.embed-multilingual-v3"},
		{"cohere.embed-v4:1", 0, "not a Cohere Embed"},
	} {
		if err := providers.ValidateEmbedder(Key, tc.model, tc.dims); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateEmbedder(%s, %d) = %v, want %q", tc.model, tc.dims, err, tc.want)
		}
	}
}

// TestParseModel_AcceptsProfilePrefixes — base ids and geography or global
// profile ids.
func TestParseModel_AcceptsProfilePrefixes(t *testing.T) {
	for id, want := range map[string]family{
		"cohere.embed-v4:0":               familyV4,
		"eu.cohere.embed-v4:0":            familyV4,
		"us.cohere.embed-v4:0":            familyV4,
		"apac.cohere.embed-v4:0":          familyV4,
		"us-gov.cohere.embed-v4:0":        familyV4,
		"global.cohere.embed-v4:0":        familyV4,
		"cohere.embed-multilingual-v3":    familyV3,
		"eu.cohere.embed-multilingual-v3": familyV3,
		"cohere.embed-english-v3":         familyV3,
	} {
		if got, err := parseModel(id); err != nil || got != want {
			t.Errorf("parseModel(%q) = %v, %v; want %v", id, got, err, want)
		}
	}
	for _, id := range []string{"cohere.embed-v4", "x.y.cohere.embed-v4:0", "EU.cohere.embed-v4:0", "cohere.command-r-v1:0", ""} {
		if _, err := parseModel(id); err == nil {
			t.Errorf("parseModel(%q) accepted", id)
		}
	}
}
