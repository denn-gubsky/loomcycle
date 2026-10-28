package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Key is the registry key (memory.embedder.api) this embedder serves.
const Key = "bedrock"

const (
	// maxBatch is Cohere's limit on texts per InvokeModel call, v3 and v4.
	maxBatch = 96
	// maxResponseBytes bounds a response read. 96 vectors of 1536 floats plus
	// the texts Cohere echoes back stay far below it.
	maxResponseBytes = 64 << 20
	// maxErrorBytes bounds an error body read; maxErrorMessage the part of it
	// an error carries.
	maxErrorBytes   = 16 << 10
	maxErrorMessage = 256
	// maxRetryAfter caps a server's Retry-After, so one throttled call cannot
	// park an embedding for minutes; the call's own deadline still wins.
	maxRetryAfter = 10 * time.Second
)

// retryWaits is the backoff between attempts: 3 attempts in all. Short on
// purpose: an embedding sits in front of a search or a write, and the shared
// ratelimit schedule (429 only, starting at 10 s) is built for long chat turns.
var retryWaits = []time.Duration{250 * time.Millisecond, time.Second}

func init() {
	providers.RegisterEmbedder(Key, func(opts providers.EmbedderOptions) (providers.Embedder, error) {
		return New(opts)
	})
	providers.RegisterEmbedderValidator(Key, validate)
}

// Embedder is a providers.Embedder (with a query mode) for Cohere Embed on
// Bedrock:
//
//	POST https://bedrock-runtime.{region}.amazonaws.com/model/{modelId}/invoke
//	{ "texts": [...], "input_type": "search_document", "truncate": "RIGHT",
//	  "output_dimension": 1024 }
//
// Embed sends search_document (stored text) and EmbedQuery search_query.
type Embedder struct {
	model     string
	family    family
	outputDim int // v4 output_dimension; 0 = omit
	dim       int
	batch     int
	region    string
	url       string // the invoke URL, model id escaped for the wire
	auth      providers.RequestAuth
	http      *http.Client
	timeout   time.Duration

	// waits overrides retryWaits; tests shorten it.
	waits []time.Duration
}

var (
	_ providers.Embedder      = (*Embedder)(nil)
	_ providers.QueryEmbedder = (*Embedder)(nil)
)

// New builds an embedder. It makes no network call. opts.BaseURL must be the
// https bedrock-runtime endpoint of the provider whose credentials opts.Auth
// carries; only its scheme and host are used.
func New(opts providers.EmbedderOptions) (*Embedder, error) {
	if opts.Model == "" {
		return nil, errors.New("bedrock embedder: opts.Model is required")
	}
	f, err := parseModel(opts.Model)
	if err != nil {
		return nil, fmt.Errorf("bedrock embedder: %w", err)
	}
	if err := checkDimensions(opts.Model, f, opts.Dimensions); err != nil {
		return nil, fmt.Errorf("bedrock embedder: %w", err)
	}
	if opts.Auth == nil {
		return nil, errors.New("bedrock embedder: no AWS authentication; memory.embedder.api: bedrock must name a provider with an auth: block")
	}
	host, region, err := runtimeHost(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("bedrock embedder: %w", err)
	}
	batch := opts.BatchSize
	if batch <= 0 || batch > maxBatch {
		batch = maxBatch
	}
	e := &Embedder{
		model:   opts.Model,
		family:  f,
		dim:     nativeDimension(f, opts.Dimensions),
		batch:   batch,
		region:  region,
		url:     "https://" + host + "/model/" + escapeModelID(opts.Model) + "/invoke",
		auth:    opts.Auth,
		http:    opts.HTTPClientOrDefault(),
		timeout: opts.Timeout,
	}
	if f == familyV4 {
		e.outputDim = opts.Dimensions
	}
	return e, nil
}

// runtimeHost checks that baseURL is an https bedrock-runtime endpoint and
// returns its host and region. The path is dropped: a provider's base_url
// carries the route of its chat protocol (/anthropic, /openai/v1).
func runtimeHost(baseURL string) (host, region string, err error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", fmt.Errorf("base URL %q is not an https Bedrock endpoint", baseURL)
	}
	labels := strings.Split(u.Hostname(), ".")
	if u.Port() != "" || len(labels) != 4 || labels[0] != "bedrock-runtime" || labels[2] != "amazonaws" || labels[3] != "com" {
		return "", "", fmt.Errorf("base URL host %q is not bedrock-runtime.<region>.amazonaws.com (embeddings are served on the runtime host only)", u.Host)
	}
	return u.Hostname(), labels[1], nil
}

// escapeModelID escapes a model id for the request path. url.PathEscape leaves
// ':' alone, but AWS's own SDKs send it as %3A, and the SigV4 signature covers
// the path exactly as sent (encoded once more): cohere.embed-v4:0 must go on
// the wire as cohere.embed-v4%3A0 and be signed as cohere.embed-v4%253A0.
func escapeModelID(id string) string {
	return strings.ReplaceAll(url.PathEscape(id), ":", "%3A")
}

func (e *Embedder) Model() string    { return e.model }
func (e *Embedder) Provider() string { return Key }

// Dimension is fixed by the model and the configured width; every response is
// checked against it.
func (e *Embedder) Dimension() int { return e.dim }

// Embed embeds stored text (input_type search_document).
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts, "search_document")
}

// EmbedQuery embeds search queries (input_type search_query), which Cohere
// places to match search_document vectors.
func (e *Embedder) EmbedQuery(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts, "search_query")
}

func (e *Embedder) embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += e.batch {
		batch := texts[i:min(i+e.batch, len(texts))]
		got, err := e.invoke(ctx, batch, inputType)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

type invokeBody struct {
	Texts           []string `json:"texts"`
	InputType       string   `json:"input_type"`
	Truncate        string   `json:"truncate"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

// statusError is a non-200 answer, kept typed so the retry loop can read the
// status and Retry-After.
type statusError struct {
	status     int
	retryAfter time.Duration
	err        error
}

func (s *statusError) Error() string { return s.err.Error() }
func (s *statusError) Unwrap() error { return s.err }

func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// invoke embeds one batch: up to 3 attempts for a throttle or a transient
// server error, all inside the embedder's Timeout and ctx.
func (e *Embedder) invoke(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	truncate := "END" // v3: NONE | START | END
	if e.family == familyV4 {
		truncate = "RIGHT" // v4: NONE | LEFT | RIGHT
	}
	// No embedding_types: without it both generations answer floats, in
	// either response shape (parseEmbeddings).
	body, err := json.Marshal(invokeBody{Texts: texts, InputType: inputType, Truncate: truncate, OutputDimension: e.outputDim})
	if err != nil {
		return nil, fmt.Errorf("bedrock embedder: marshal request: %w", err)
	}
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	waits := e.waits
	if waits == nil {
		waits = retryWaits
	}
	for attempt := 0; ; attempt++ {
		got, err := e.invokeOnce(ctx, body, len(texts))
		var se *statusError
		if err == nil || !errors.As(err, &se) || !retryable(se.status) || attempt >= len(waits) {
			return got, err
		}
		wait := jitter(waits[attempt])
		if se.retryAfter > 0 {
			wait = min(se.retryAfter, maxRetryAfter)
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			return nil, err // the wait would outlive the call
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
	}
}

// jitter spreads d by ±20%, so callers throttled together do not retry together.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (e *Embedder) invokeOnce(ctx context.Context, body []byte, n int) ([][]float32, error) {
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
	resp, err := providers.DoWithAuth(e.http, e.auth, body, build, setBearer)
	if err != nil {
		// Never the request: it may hold personal data. DoWithAuth's errors
		// name the auth failure or the transport, not the body.
		return nil, e.errorf("%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		return nil, &statusError{
			status:     resp.StatusCode,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			err:        e.errorf("status %d%s", resp.StatusCode, describeAWSError(resp.Header.Get("X-Amzn-Errortype"), raw)),
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, e.errorf("read response: %w", err)
	}
	// Bedrock reports what InvokeModel consumed in a header, not the body (a
	// Cohere answer carries no count of its own).
	if n, err := strconv.Atoi(resp.Header.Get("X-Amzn-Bedrock-Input-Token-Count")); err == nil && n >= 0 {
		providers.ReportEmbedTokens(ctx, n)
	}
	vecs, err := parseEmbeddings(raw)
	if err != nil {
		return nil, e.errorf("%w", err)
	}
	if len(vecs) != n {
		return nil, e.errorf("got %d embeddings for %d inputs", len(vecs), n)
	}
	for i, v := range vecs {
		if len(v) != e.dim {
			return nil, e.errorf("embedding %d is %d-dimensional, expected %d", i, len(v), e.dim)
		}
	}
	return vecs, nil
}

// errorf prefixes an error with what an operator needs to find the call: the
// model id and the region.
func (e *Embedder) errorf(format string, args ...any) error {
	return fmt.Errorf("bedrock embedder: model %q in %s: "+format, append([]any{e.model, e.region}, args...)...)
}

// setBearer sends a minted Bedrock key (auth type aws_bedrock_token).
func setBearer(req *http.Request, key string) {
	req.Header.Set("Authorization", "Bearer "+key)
}

// parseEmbeddings reads the float vectors from an InvokeModel answer. Cohere
// answers a list under embeddings_floats and a map of lists under
// embeddings_by_type; AWS's documentation and its own samples disagree on
// which one comes back without embedding_types, so both are read. An answer
// with no response_type is read by its shape.
func parseEmbeddings(raw []byte) ([][]float32, error) {
	var doc struct {
		ResponseType string          `json:"response_type"`
		Embeddings   json.RawMessage `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	shape := doc.ResponseType
	if shape == "" {
		switch t := bytes.TrimSpace(doc.Embeddings); {
		case len(t) > 0 && t[0] == '[':
			shape = "embeddings_floats"
		case len(t) > 0 && t[0] == '{':
			shape = "embeddings_by_type"
		}
	}
	switch shape {
	case "embeddings_floats":
		var vecs [][]float32
		if err := json.Unmarshal(doc.Embeddings, &vecs); err != nil {
			return nil, fmt.Errorf("decode embeddings: %w", err)
		}
		return vecs, nil
	case "embeddings_by_type":
		var byType struct {
			Float [][]float32 `json:"float"`
		}
		if err := json.Unmarshal(doc.Embeddings, &byType); err != nil {
			return nil, fmt.Errorf("decode embeddings: %w", err)
		}
		if byType.Float == nil {
			return nil, errors.New("response carries no float embeddings")
		}
		return byType.Float, nil
	}
	return nil, fmt.Errorf("unrecognised response_type %q", doc.ResponseType)
}

// describeAWSError renders an AWS error answer for an error message: the error
// type and the body's message field, capped. Nothing else from the body: no
// AWS error carries the request, and the cap keeps a misrouted answer short.
// raw may end with the auth layer's hint after the JSON, which is kept.
func describeAWSError(errType string, raw []byte) string {
	var out strings.Builder
	if t, _, _ := strings.Cut(errType, ":"); t != "" {
		out.WriteString(" " + t)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var doc struct {
		Message      string `json:"message"`
		MessageUpper string `json:"Message"`
	}
	msg := ""
	rest := raw
	if dec.Decode(&doc) == nil {
		msg = doc.Message
		if msg == "" {
			msg = doc.MessageUpper
		}
		rest = raw[dec.InputOffset():]
	} else {
		rest = nil // not JSON: no message to quote
	}
	// First line only: a signature failure's message can go on to quote the
	// canonical request AWS expected, and that carries the session token.
	msg, _, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	if msg = capBytes(strings.TrimSpace(msg), maxErrorMessage); msg != "" {
		out.WriteString(": " + msg)
	}
	if hint := capBytes(strings.TrimSpace(string(rest)), maxErrorMessage); hint != "" {
		out.WriteString(" " + hint)
	}
	return out.String()
}

// capBytes trims s to at most n bytes without splitting a UTF-8 sequence.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// parseRetryAfter reads a Retry-After header: seconds, or an HTTP date. 0 when
// absent or unreadable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
