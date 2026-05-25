package http

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEmbedder is a deterministic test embedder for the v0.11.4
// shim tests. Distinct from memory_admin_test.go's adminFakeEmbedder
// (which is tied to the reembed fixture) so the shim tests stay
// independent of that machinery.
//
// Returns vectors of `dim` length where each value is a function of
// the input text — gives the test deterministic content to assert
// against without flake.
type fakeEmbedder struct {
	provider  string
	model     string
	dim       int
	failNext  bool
	lastTexts []string
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.lastTexts = append([]string(nil), texts...)
	if e.failNext {
		e.failNext = false
		return nil, errors.New("fake embedder injected failure")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dim)
		for j := 0; j < e.dim; j++ {
			v[j] = float32(len(t)) + float32(j)*0.1
		}
		out[i] = v
	}
	return out, nil
}
func (e *fakeEmbedder) Provider() string { return e.provider }
func (e *fakeEmbedder) Model() string    { return e.model }
func (e *fakeEmbedder) Dimension() int   { return e.dim }

// makeEmbeddingsServer builds a Server with the fake embedder wired
// in. Mirrors makeServer's shape but with an additional SetEmbedder
// call.
func makeEmbeddingsServer(t *testing.T) (*Server, *fakeEmbedder, *httptest.Server) {
	emb := &fakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4}
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	srv.SetEmbedder(emb)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, emb, ts
}

// TestEmbeddings_HappyPath_SingleString — the most common shape:
// one string in, one embedding out. Result is a JSON array of
// numbers (default encoding_format=float).
func TestEmbeddings_HappyPath_SingleString(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	body := `{"model":"text-embedding-3-small","input":"hello"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out openaiEmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" {
		t.Errorf("object=%q; want list", out.Object)
	}
	if len(out.Data) != 1 {
		t.Fatalf("data len=%d; want 1", len(out.Data))
	}
	if out.Data[0].Object != "embedding" {
		t.Errorf("data[0].object=%q; want embedding", out.Data[0].Object)
	}
	if out.Data[0].Index != 0 {
		t.Errorf("data[0].index=%d; want 0", out.Data[0].Index)
	}
	// fakeEmbedder produces vectors of dim=4; "hello"=5 chars; first
	// element should be float32(5).
	var vec []float32
	if err := json.Unmarshal(out.Data[0].Embedding, &vec); err != nil {
		t.Fatalf("embedding should decode as []float32: %v (raw=%s)", err, string(out.Data[0].Embedding))
	}
	if len(vec) != 4 || vec[0] != 5.0 {
		t.Errorf("vec=%v; want len=4 + first elem 5.0", vec)
	}
	if out.Model != "text-embedding-3-small" {
		t.Errorf("model=%q; want consumer-requested model echoed back", out.Model)
	}
}

// TestEmbeddings_HappyPath_ArrayOfStrings — batch input. Each
// element gets one embedding entry; indices match input order.
func TestEmbeddings_HappyPath_ArrayOfStrings(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	body := `{"model":"text-embedding-3-small","input":["one","two","three"]}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out openaiEmbeddingsResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 3 {
		t.Fatalf("data len=%d; want 3", len(out.Data))
	}
	for i, item := range out.Data {
		if item.Index != i {
			t.Errorf("data[%d].index=%d; want %d", i, item.Index, i)
		}
	}
}

// TestEmbeddings_Base64Encoding — encoding_format:"base64" packs
// each float32 little-endian then base64-encodes. Verify the
// round-trip: base64 decode + LE unpack should reconstruct the
// original vector.
func TestEmbeddings_Base64Encoding(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	body := `{"model":"text-embedding-3-small","input":"abc","encoding_format":"base64"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	var out openaiEmbeddingsResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 1 {
		t.Fatalf("data len=%d; want 1", len(out.Data))
	}
	// Base64 mode: the `embedding` field is a JSON string, not array.
	var b64 string
	if err := json.Unmarshal(out.Data[0].Embedding, &b64); err != nil {
		t.Fatalf("base64-mode embedding should be a JSON string: %v (raw=%s)", err, string(out.Data[0].Embedding))
	}
	// Decode + unpack and verify vs the fakeEmbedder's deterministic
	// output for "abc" (len=3 → vec=[3.0, 3.1, 3.2, 3.3]).
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if len(raw) != 16 { // 4 floats * 4 bytes
		t.Errorf("decoded len=%d; want 16 (4 floats × 4 bytes)", len(raw))
	}
	got := make([]float32, 4)
	for i := 0; i < 4; i++ {
		got[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	want := []float32{3.0, 3.1, 3.2, 3.3}
	for i := range want {
		// Float32 precision tolerance.
		if math.Abs(float64(got[i]-want[i])) > 1e-5 {
			t.Errorf("vec[%d]=%v; want %v", i, got[i], want[i])
		}
	}
}

// TestEmbeddings_NoEmbedderConfigured — when the operator hasn't
// wired memory.embedder.{provider,model}, the endpoint returns 503
// with a clear pointer at the yaml config.
func TestEmbeddings_NoEmbedderConfigured(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	// Deliberately don't call SetEmbedder.
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"model":"text-embedding-3-small","input":"hi"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d; want 503 (no embedder)", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("memory.embedder")) {
		t.Errorf("body should point at memory.embedder yaml; got %s", string(raw))
	}
}

// TestEmbeddings_TokenizedInputRefused — sending tokenized input
// (number arrays) is OpenAI-specific; loomcycle's substrate
// embedders all accept text strings. Refuse with a clear error
// pointing at the right shape.
func TestEmbeddings_TokenizedInputRefused(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	body := `{"model":"text-embedding-3-small","input":[42, 17, 8]}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d; want 400 (tokenized input)", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("tokenized")) {
		t.Errorf("body should mention tokenized; got %s", string(raw))
	}
}

// TestEmbeddings_NegativeTokenizedInputRefused — regression for the
// v0.11.4 review finding: the tokenized-input sniffer must also
// catch negative integer token IDs (BPE tokenizers occasionally
// emit them). Without the `-` check, the sniffer fell through to
// the generic "string or array of strings" error instead of the
// specific tokenized-input hint the comment promised.
func TestEmbeddings_NegativeTokenizedInputRefused(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	body := `{"model":"text-embedding-3-small","input":[-42, 17, 8]}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d; want 400 (negative-int tokenized input)", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("tokenized")) {
		t.Errorf("body should mention tokenized (sniffer must catch negative integers); got %s", string(raw))
	}
}

// TestEmbeddings_EmptyInputRefused — empty string, empty array,
// null all rejected as bad request.
func TestEmbeddings_EmptyInputRefused(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	for _, input := range []string{
		`null`,
		`[]`,
	} {
		body := `{"model":"text-embedding-3-small","input":` + input + `}`
		resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("input=%s: status=%d; want 400 (body=%s)", input, resp.StatusCode, string(raw))
		}
	}
}

// TestEmbeddings_UnknownEncodingFormat — anything other than
// "float" / "base64" is rejected before the embedder is invoked.
func TestEmbeddings_UnknownEncodingFormat(t *testing.T) {
	_, _, ts := makeEmbeddingsServer(t)

	body := `{"model":"text-embedding-3-small","input":"hi","encoding_format":"protobuf"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d; want 400", resp.StatusCode)
	}
}

// TestEmbeddings_AuthRequired — bearer enforced same as every
// other /v1/* endpoint.
func TestEmbeddings_AuthRequired(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "secret"
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	srv.SetEmbedder(&fakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"model":"text-embedding-3-small","input":"hi"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d; want 401", resp.StatusCode)
	}
}

// TestEmbeddings_ModelEchoesConsumerRequest — the response.model
// field carries what the consumer requested, not what the
// configured embedder uses. Drop-in OpenAI SDK compatibility relies
// on this — consumers pass "text-embedding-3-small" and want to see
// it echoed.
func TestEmbeddings_ModelEchoesConsumerRequest(t *testing.T) {
	srv, _ := makeServer(t, &scriptedProvider{}, makeBaseConfig())
	// Configure an embedder whose Model() differs from what the
	// request asks for; the response must echo the requested model,
	// not the served one.
	srv.SetEmbedder(&fakeEmbedder{provider: "anthropic", model: "voyage-3", dim: 4})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"model":"text-embedding-3-small","input":"hi"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out openaiEmbeddingsResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Model != "text-embedding-3-small" {
		t.Errorf("response.model=%q; want consumer-requested 'text-embedding-3-small' echoed back (got the served model)", out.Model)
	}
}

// TestEmbeddings_EmbedFailureSurfacesAs502 — when the underlying
// embedder errors (rate limit, auth, etc.), the shim translates to
// a 502 Bad Gateway with the error message in the JSON body.
func TestEmbeddings_EmbedFailureSurfacesAs502(t *testing.T) {
	srv, emb, ts := makeEmbeddingsServer(t)
	emb.failNext = true
	_ = srv

	body := `{"model":"text-embedding-3-small","input":"hi"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status=%d; want 502", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("injected failure")) {
		t.Errorf("body should surface the embedder error; got %s", string(raw))
	}
}
