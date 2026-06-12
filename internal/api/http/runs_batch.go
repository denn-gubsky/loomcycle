package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// handleRunsBatch implements the RFC Y external fan-out: POST /v1/runs:batch
// spawns every child in `spawns` concurrently (mode "join") and returns the
// combined index-aligned envelope once all settle. Per-child failures are
// captured in that child's result; the call only 400s on a MALFORMED batch
// (empty / over-cap / unsupported mode).
//
// Authoritative tenant/principal flows from the auth middleware via the request
// ctx: SpawnRunBatch → SpawnRun → RunOnce re-applies the principal per child
// (server.go applyPrincipal), so a forged per-spawn `tenant_id` can never widen
// scope — every child runs under the batch caller's authoritative identity.
func (s *Server) handleRunsBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	// A batch carries up to MaxBatchSpawns prompts, so cap the body more
	// generously than a single /v1/runs (1 MiB) while still bounding abuse.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var req connector.BatchSpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	res, err := s.SpawnRunBatch(r.Context(), req)
	if err != nil {
		// SpawnRunBatch errors only on a malformed request (empty / over-cap /
		// unsupported mode) — a client error, not a 500. Per-child run failures
		// are reported inside res, not as an error here.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
