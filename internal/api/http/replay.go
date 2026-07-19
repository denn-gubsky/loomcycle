package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// replayErr is the typed failure ReplaySession returns; the HTTP handler maps
// status/code, and the gRPC adapter maps its own status. Mirrors compactErr.
type replayErr struct {
	status int
	code   string
	msg    string
}

func (e *replayErr) Error() string { return e.msg }

// HTTPStatus lets the gRPC adapter map this error to a status code without
// importing the unexported type (mirrors compactErr).
func (e *replayErr) HTTPStatus() int { return e.status }

// ReplaySession copies a SOURCE session's transcript into a NEW session bound to
// the target agent, so that agent continues from the same context (RFC BJ Phase
// 4). It is the connector-surface op behind POST /v1/sessions/{id}/replay + the
// gRPC Replay RPC + the adapters.
//
// The new session is DURABLE: the carried transcript is persisted as the new
// session's own opening events (a "seed" run), so every future continuation
// replays it via the normal path — the caller just continues the returned
// new_session_id with POST /v1/sessions/{id}/messages. No model turn is run here.
//
// Two safety properties: provider-specific reasoning (Anthropic thinking seals /
// DeepSeek reasoning_content) is STRIPPED from the carried assistant turns so the
// history is safe to replay under a different-provider target agent; and the
// source is read tenant-gated (a cross-tenant / missing id folds to an opaque
// not-found — session ids aren't secrets). `compress` appends a compaction marker
// after the copied events so the carried history replays as [summary + recent
// tail]; it's computed BEFORE any write so a summarization failure never orphans
// a half-seeded session.
func (s *Server) ReplaySession(ctx context.Context, req connector.ReplaySessionRequest) (connector.ReplaySessionResult, error) {
	if s.store == nil {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusServiceUnavailable, msg: "replay requires persistence"}
	}
	if err := agents.ValidateName(req.Agent); err != nil {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusBadRequest, msg: "invalid target agent name: " + err.Error()}
	}

	// Source session — tenant-gated read; a cross-tenant or missing id both fold
	// into the same opaque not-found (session ids are returned/logged/shown, so
	// the gate must not become an existence oracle).
	src, serr := s.tenantStore(ctx).GetSession(ctx, req.SourceSessionID)
	if serr != nil || !sessionOwnershipOK(ctx, src) {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusNotFound, msg: "no session for that id"}
	}

	// The new session belongs to the AUTHORITATIVE caller (never the wire).
	tenant, subject := s.applyPrincipal(ctx, "", "")

	// The target agent must resolve in the caller's tenant so the new session
	// binds a usable agent (static or dynamic; the clone flow's typical target).
	agentDef, ok := s.lookupAgent(ctx, tenant, req.Agent)
	if !ok {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusConflict, code: "agent_gone", msg: "target agent not found: " + req.Agent}
	}

	events, terr := s.store.GetTranscript(ctx, req.SourceSessionID)
	if terr != nil {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusInternalServerError, msg: "read transcript: " + terr.Error()}
	}
	if len(events) == 0 {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusBadRequest, code: "empty_source", msg: "source session has no transcript to replay"}
	}

	// Compress FIRST (compute-then-write): summarize before creating anything so a
	// summarization failure returns cleanly without orphaning a seeded session.
	var summary string
	var keepN int
	var keepFirst, compacted bool
	if req.Compress {
		var cerr error
		summary, keepN, keepFirst, compacted, cerr = s.computeReplayCompaction(ctx, agentDef, tenant, subject, req.Agent, events)
		if cerr != nil {
			return connector.ReplaySessionResult{}, &replayErr{status: http.StatusBadGateway, msg: cerr.Error()}
		}
	}

	// Mint the new session (bound to the target agent) + a seed run to hold the
	// carried transcript. Reuses the same create path as a fresh run.
	identity := store.RunIdentity{AgentID: newAgentID(), UserID: subject, TenantID: tenant, ReplicaID: s.replicaID}
	newSessionID, seedRunID, cErr := s.openOrCreateSessionAndRun(ctx, "", req.Agent, tenant, subject, identity)
	if cErr != nil {
		return connector.ReplaySessionResult{}, &replayErr{status: http.StatusInternalServerError, msg: "create session: " + cErr.Error()}
	}

	// Copy the source transcript into the seed run, stripping provider-specific
	// reasoning from assistant turns (cross-provider safety).
	copied := 0
	for _, ev := range events {
		payload := ev.Payload
		if ev.Type == "done" {
			payload = stripReasoning(ev.Payload)
		}
		if aerr := s.store.AppendEvent(ctx, seedRunID, ev.Type, payload); aerr != nil {
			return connector.ReplaySessionResult{}, &replayErr{status: http.StatusInternalServerError, msg: "seed transcript: " + aerr.Error()}
		}
		copied++
	}

	// Compress: append the marker after the copied events, so a continuation
	// collapses them to [summary + last-keepN] via replayTranscript's existing
	// context_compaction handling.
	if compacted {
		payload, merr := json.Marshal(providers.Event{
			Type: providers.EventContextCompaction,
			ContextCompaction: &providers.ContextCompactionEventInfo{
				Summary: summary, KeepN: keepN, KeepFirst: keepFirst, Trigger: "self",
			},
		})
		if merr == nil {
			if aerr := s.store.AppendEvent(ctx, seedRunID, string(providers.EventContextCompaction), payload); aerr != nil {
				return connector.ReplaySessionResult{}, &replayErr{status: http.StatusInternalServerError, msg: "seed compaction marker: " + aerr.Error()}
			}
		} else {
			compacted = false
		}
	}

	return connector.ReplaySessionResult{
		NewSessionID: newSessionID, SeedRunID: seedRunID, EventsCopied: copied, Compacted: compacted,
	}, nil
}

// computeReplayCompaction summarizes the source transcript for the compress path,
// mirroring compactRunWithSource's summarize chain but sourced from another
// session and targeted at the NEW agent's compaction settings. Returns ok=false
// (no error) when the transcript is too short to compact; a non-nil error is a
// hard failure (resolve/summarize) surfaced before any write.
func (s *Server) computeReplayCompaction(ctx context.Context, agentDef config.AgentDef, tenant, subject, agentName string, events []store.Event) (summary string, keepN int, keepFirst, ok bool, err error) {
	msgs := replayTranscript(events)
	if len(msgs) < minCompactMessages {
		return "", 0, false, false, nil
	}
	restricted := s.operatorKeyRestrictedForCtx(ctx)
	providerID, model, _, rerr := s.resolveAgentDef(ctx, agentDef, tenant, subject, agentName, "", restricted)
	if rerr != nil {
		return "", 0, false, false, fmt.Errorf("resolve provider/model: %w", rerr)
	}
	provider, perr := s.providers.Get(providerID)
	if perr != nil {
		return "", 0, false, false, fmt.Errorf("provider unavailable: %w", perr)
	}
	keepLastN, keepFirstCfg, targetPct := config.CompactionDefaultKeepLastN, config.CompactionDefaultKeepFirst, config.CompactionDefaultTargetPct
	summaryModel := model
	if c := agentDef.Compaction; c != nil {
		if c.KeepLastN != nil {
			keepLastN = *c.KeepLastN
		}
		if c.KeepFirst != nil {
			keepFirstCfg = *c.KeepFirst
		}
		if c.TargetPercentage != nil {
			targetPct = *c.TargetPercentage
		}
		if c.Model != nil && *c.Model != "" {
			summaryModel = *c.Model
		}
	}
	firstIdx, cut, splitOK := loop.CompactionSplit(msgs, keepLastN, keepFirstCfg)
	if !splitOK {
		return "", 0, false, false, nil
	}
	// RFC AX: the summarize provider.Call runs outside the loop, so the ctx must
	// carry the same credential context a run's loopCtx does (mirror compact).
	summCtx := providers.WithCredentialResolver(ctx, s.credResolver)
	summCtx = providers.WithOperatorKeyAllowed(summCtx, !restricted)
	summCtx = tools.WithRunIdentity(summCtx, tools.RunIdentityValue{TenantID: tenant, UserID: subject})
	summCtx = tools.WithAgentName(summCtx, agentName)
	sum, serr := loop.Summarize(summCtx, provider, summaryModel, msgs[firstIdx:cut], targetPct)
	if serr != nil {
		return "", 0, false, false, fmt.Errorf("summarize: %w", serr)
	}
	sum = strings.TrimSpace(sum)
	if sum == "" {
		return "", 0, false, false, fmt.Errorf("summarization produced no text")
	}
	return sum, len(msgs) - cut, firstIdx > 0, true, nil
}

// stripReasoning removes the provider-specific reasoning fields from a `done`
// event payload (Anthropic thinking-block seals / DeepSeek reasoning_content).
// Replaying those verbatim under a DIFFERENT-provider target agent can 400 — the
// very failure the fields exist to prevent for same-provider resume. Uses a
// generic map so every other field round-trips byte-for-byte; leaves the payload
// untouched if it doesn't parse or carries no reasoning.
func stripReasoning(payload []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	_, a := m["reasoning"]
	_, b := m["reasoning_signature"]
	if !a && !b {
		return payload
	}
	delete(m, "reasoning")
	delete(m, "reasoning_signature")
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// handleReplay serves POST /v1/sessions/{id}/replay — {id} is the SOURCE session,
// the body carries {agent, compress?}. Scope: runs:create (it mints a new run).
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("id")
	if !validIdent(sourceID) {
		http.Error(w, "session id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	var body struct {
		Agent    string `json:"agent"`
		Compress bool   `json:"compress"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Agent) == "" {
		http.Error(w, "agent is required", http.StatusBadRequest)
		return
	}
	res, err := s.ReplaySession(r.Context(), connector.ReplaySessionRequest{
		SourceSessionID: sourceID, Agent: strings.TrimSpace(body.Agent), Compress: body.Compress,
	})
	if err != nil {
		var re *replayErr
		if errors.As(err, &re) {
			status := re.status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			if re.code != "" {
				writeJSONError(w, status, re.code, re.msg)
			} else {
				http.Error(w, re.msg, status)
			}
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
