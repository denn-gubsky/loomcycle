package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// handleRunsBatch implements the RFC Y external fan-out: POST /v1/runs:batch
// spawns every child in `spawns` concurrently and returns the combined
// index-aligned envelope — once all settle (mode "join"), or once all have
// started (mode "detach", whose runs go on after the response). Per-child failures are
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

// spawnDetached starts one child of a mode "detach" batch and returns its
// handle as soon as the run exists — admitted, registered, about to run — or
// the refusal that kept it from starting, reported in its slot the way a join
// child's failure is.
//
// The run outlives this call: the HTTP handler, the MCP tool call and the gRPC
// RPC all end when the batch returns. So it runs under context.WithoutCancel —
// keeping the caller's values (principal, tenant, confinement) — and stops only
// through the cancel registry, like an interactive run. It is an ordinary
// RunOnce on that ctx, not RunInput.Detached: a Detached RunOnce releases its
// admission slots when it returns, which here would be at once, leaving the
// batch's runs outside the per-user gate. This way each child holds its slots
// until it ends, exactly as a join child does.
//
// Until the run is registered the caller can still withdraw it: a caller that
// leaves while the child waits for admission cancels that wait, so no run
// starts that nobody holds a handle to.
func (s *Server) spawnDetached(ctx context.Context, req connector.SpawnRunRequest) connector.SpawnRunResult {
	req.ParentContext = connector.StripRuntimeParentContext(req.ParentContext)
	runCtx, abort := context.WithCancel(context.WithoutCancel(ctx))
	var (
		mu         sync.Mutex
		registered bool
	)
	handle := make(chan connector.SpawnRunResult, 1)
	ended := make(chan error, 1)
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, sessionID, _ string) {
			mu.Lock()
			registered = true
			mu.Unlock()
			handle <- connector.SpawnRunResult{
				AgentID:       agentID,
				RunID:         runID,
				SessionID:     sessionID,
				Status:        string(store.RunRunning),
				ParentContext: req.ParentContext,
			}
		},
	}
	go func() {
		defer abort() // releases runCtx once the run is over
		ended <- s.RunOnce(runCtx, spawnRequestToRunInput(req), cb)
	}()

	callerDone := ctx.Done()
	withdrawn := false
	for {
		select {
		case h := <-handle:
			return h
		case err := <-ended:
			// A run fast enough to finish before this select ran has a handle
			// waiting too; it started, so report it as started.
			select {
			case h := <-handle:
				return h
			default:
			}
			res := connector.SpawnRunResult{Status: string(store.RunFailed), ParentContext: req.ParentContext}
			switch {
			case withdrawn:
				// RunOnce reports the aborted admission wait as an internal
				// error; say what happened instead.
				res.Status = string(store.RunCancelled)
				res.Error = "not started: the caller left before the run was admitted"
			case err == nil:
				res.Error = "run did not start"
			default:
				res.Error = err.Error()
				if info, ok := errclassify.CategoryOf(err); ok {
					res.ErrorInfo = &info
				}
			}
			return res
		case <-callerDone:
			mu.Lock()
			if !registered {
				abort()
				withdrawn = true
			}
			mu.Unlock()
			callerDone = nil // wait for the handle or the refusal
		}
	}
}
