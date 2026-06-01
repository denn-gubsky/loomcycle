package http

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// embeddings_compat.go — v0.11.4 OpenAI Embeddings compatibility
// shim. Serves POST /v1/embeddings in the exact wire shape the
// OpenAI Python / TypeScript SDKs expect.
//
// Architecture: simpler than the chat-completions shim because
// embeddings have no resolver / tier / streaming complexity.
//
//   1. Parse the OpenAI-shaped request.
//   2. Validate (configured embedder present; non-tokenized input;
//      non-empty texts).
//   3. Acquire per-user semaphore slot (same as the chat-completions
//      gateway when `user` is set).
//   4. Call s.embedder.Embed() — the same instance the Memory tool
//      uses internally for embed:true.
//   5. Translate the [][]float32 result back into OpenAI's shape;
//      honor encoding_format = float|base64.
//
// Consumers benefit identically to v0.11.3: every OpenAI-SDK
// embeddings consumer (LangChain OpenAIEmbeddings, every RAG tool,
// vector DBs, custom search code, every "use OpenAI embeddings"
// tutorial) can route through loomcycle by changing only the base
// URL + auth token.

const embeddingsCompatMaxRequestBytes = 8 << 20 // 8 MiB — embedding batches can be large

// handleEmbeddings serves POST /v1/embeddings.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, embeddingsCompatMaxRequestBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "empty body")
		return
	}

	var req openaiEmbeddingsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}

	if s.embedder == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "embedder_not_configured",
			"no embedder configured; set memory.embedder.{provider,model} in loomcycle.yaml")
		return
	}

	texts, err := parseEmbeddingsInput(req.Input)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(texts) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request",
			"input must be a non-empty string or array of strings")
		return
	}

	encoding := req.EncodingFormat
	if encoding == "" {
		encoding = "float"
	}
	if encoding != "float" && encoding != "base64" {
		writeJSONError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("encoding_format %q not supported (must be \"float\" or \"base64\")", encoding))
		return
	}

	// Per-subject quota acquisition. RFC L: the authenticated
	// principal's Subject is the authoritative fairness key when
	// present; the wire user is the open/un-authed fallback. Empty
	// key bypasses the per-user cap. Mirrors the chat-completions
	// gateway exactly.
	release, err := s.sem.AcquireForUser(r.Context(), auth.SubjectForFairness(r.Context(), req.User))
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	defer release()

	startedAt := time.Now()
	vectors, err := s.embedder.Embed(r.Context(), texts)
	if err != nil {
		logEmbeddingsRequest(req.Model, s.embedder.Model(), req.User, len(texts), 0, time.Since(startedAt), err)
		writeJSONError(w, http.StatusBadGateway, "embed_failed", err.Error())
		return
	}

	resp, err := vectorsToOpenAIResponse(vectors, req.Model, encoding)
	if err != nil {
		logEmbeddingsRequest(req.Model, s.embedder.Model(), req.User, len(texts), 0, time.Since(startedAt), err)
		writeJSONError(w, http.StatusInternalServerError, "encode_failed", err.Error())
		return
	}

	dim := 0
	if len(vectors) > 0 {
		dim = len(vectors[0])
	}
	logEmbeddingsRequest(req.Model, s.embedder.Model(), req.User, len(texts), dim, time.Since(startedAt), nil)
	writeJSONOK(w, resp)
}

// parseEmbeddingsInput handles OpenAI's polymorphic `input` field.
//
// Accepted: `"single string"` OR `["string", ...]`.
//
// Refused with a clear error:
//   - `[42, 17, ...]` (single tokenized input) — embedders in
//     loomcycle's substrate (OpenAI, Gemini, Voyage) all accept
//     text strings; tokenized inputs are an OpenAI-specific
//     optimization we don't pass through.
//   - `[[42, 17], [3, 9]]` (batched tokenized inputs) — same.
//   - `null` / missing — refused as bad request.
func parseEmbeddingsInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("input is required (string or array of strings)")
	}
	// Try single string first — most common case for "compute one
	// embedding" calls.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, nil
	}
	// Try array of strings.
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	// Sniff for tokenized inputs to give a friendly error rather
	// than the generic "cannot unmarshal number into string"
	// goose chase. JSON numbers can start with a digit (`42`) or
	// a minus (`-5`); BPE tokenizers occasionally emit negative
	// token IDs. The `[` catches nested batched-tokens
	// (`[[42, 17], [3, 9]]`).
	var maybeTokens []json.RawMessage
	if err := json.Unmarshal(raw, &maybeTokens); err == nil && len(maybeTokens) > 0 {
		first := string(maybeTokens[0])
		if len(first) > 0 && (first[0] == '[' || first[0] == '-' || (first[0] >= '0' && first[0] <= '9')) {
			return nil, errors.New("tokenized input (number arrays) is not supported; send text strings instead")
		}
	}
	return nil, errors.New("input must be a string or array of strings")
}

// vectorsToOpenAIResponse builds the response envelope. `encoding`
// is "float" (default) or "base64" — already validated by the caller.
//
// Model is the consumer's requested model id (echoed back per OpenAI
// drop-in compatibility, not the configured embedder's model). The
// audit log records what was actually served so operators can spot
// drift.
func vectorsToOpenAIResponse(vectors [][]float32, model, encoding string) (openaiEmbeddingsResponse, error) {
	items := make([]openaiEmbeddingItem, 0, len(vectors))
	for i, vec := range vectors {
		raw, err := marshalEmbeddingVector(vec, encoding)
		if err != nil {
			return openaiEmbeddingsResponse{}, fmt.Errorf("encode vector %d: %w", i, err)
		}
		items = append(items, openaiEmbeddingItem{
			Object:    "embedding",
			Embedding: raw,
			Index:     i,
		})
	}
	return openaiEmbeddingsResponse{
		Object: "list",
		Data:   items,
		Model:  model,
		Usage:  openaiEmbeddingsUsage{PromptTokens: 0, TotalTokens: 0},
	}, nil
}

// marshalEmbeddingVector serialises one vector in the requested
// encoding format.
//
//   - "float": JSON array of numbers — `[0.1, 0.2, ...]`. The
//     standard Embedded encoding consumers default to.
//   - "base64": JSON string — float32 → LE bytes → base64. Saves
//     ~25% wire size on 1536-dim vectors. Same packing v0.9.0's
//     snapshot vector round-trip uses (`memory_embeddings.go`).
func marshalEmbeddingVector(vec []float32, encoding string) (json.RawMessage, error) {
	if encoding == "base64" {
		buf := make([]byte, 4*len(vec))
		for i, v := range vec {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
		}
		s := base64.StdEncoding.EncodeToString(buf)
		return json.Marshal(s)
	}
	return json.Marshal(vec)
}

// logEmbeddingsRequest emits a structured audit line per request.
// Same posture as the chat-completions gateway's logGatewayRequest:
// always-on, scrapable, no transcript bodies. v0.11.x ships
// log-only; the dedicated gateway_events table from the v0.11.0 RFC
// would cover both chat + embeddings on the same surface.
func logEmbeddingsRequest(
	requestedModel, servedModel, userID string,
	inputCount, outputDim int,
	latency time.Duration,
	runErr error,
) {
	status := "ok"
	if runErr != nil {
		status = "error"
	}
	log.Printf(
		"embeddings: model=%q served_model=%q user_id=%q input_count=%d output_dim=%d latency_ms=%d status=%s err=%q",
		requestedModel, servedModel, userID,
		inputCount, outputDim,
		latency.Milliseconds(),
		status,
		errStringForLog(runErr),
	)
}

func errStringForLog(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
