package http

// trace_backfill.go — RFC CI P3: make the transcripts that predate the index
// searchable, on an operator's say-so.
//
// The index is forward-only by decision (CI open question 4): enabling it indexes new
// turns and leaves history alone, so nobody gets an unbounded embed storm on the day
// they flip the flag — worst on exactly the slow local embedders least able to absorb
// it. That leaves the archive unsearchable, which reads as a bug to anyone who
// enabled the feature expecting their history to become findable.
//
// So the backfill is explicit, bounded and re-runnable, the same posture
// backfill_embeddings takes. An operator runs it, sees what it did, and runs it again.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

type traceBackfillReport struct {
	Tenant     string `json:"tenant"`
	UserID     string `json:"user_id"`
	Sessions   int    `json:"sessions_scanned"`
	TurnsFound int    `json:"turns_found"`
	Indexed    int    `json:"indexed"`
	Skipped    int    `json:"already_indexed"`
	DryRun     bool   `json:"dry_run"`
	StopReason string `json:"stop_reason"`
	Note       string `json:"note,omitempty"`
}

// handleMemoryBackfillTraces serves
// POST /v1/_memory/backfill_traces?tenant=&user_id=&limit=&dry_run=&assistant=
func (s *Server) handleMemoryBackfillTraces(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"the trace backfill needs a persistent store")
		return
	}
	// GATED ON THE FEATURE, not only on the route. Backfilling into an index nobody
	// reads would write a user's whole conversation history into a second place for no
	// benefit — and the posture argument that made the index opt-in applies with more
	// force to a bulk write than to a single turn.
	if !s.cfg().Env.MemoryTraceIndexEnabled {
		writeJSONError(w, http.StatusConflict, "trace_index_disabled",
			"the trace index is off (LOOMCYCLE_MEMORY_TRACE_INDEX), so there is nothing to "+
				"backfill into — enable it first, then run this to make older chats searchable")
		return
	}
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if userID == "" {
		// A turn is indexed under the person who typed it, so a backfill with no user
		// has no scope to write into. Refused rather than defaulted to "everyone":
		// this writes personal data, and the blast radius of a wrong default is every
		// user in the tenant.
		writeJSONError(w, http.StatusBadRequest, "user_required",
			"user_id is required: a turn is indexed under the person who typed it, and a "+
				"backfill with no user would have no scope to write into")
		return
	}
	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: memory rows are keyed on it, so omitting "+
				"it silently targets the default tenant. Pass ?tenant=<id>, or ?tenant= for "+
				"the default tenant.")
		return
	}
	// dry_run defaults TRUE, like every other sweep here: this one embeds, so a
	// mistyped command costs an operator their embedder rather than a wrong answer.
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// The assistant half is opt-in HERE too, independently of the live flag. An
	// operator who runs the index on user turns only may still want one archive pass
	// with both, and the reverse — an operator with the live flag on who wants a
	// cheap user-only backfill — is just as reasonable.
	withAssistant := r.URL.Query().Get("assistant") == "1" ||
		strings.EqualFold(r.URL.Query().Get("assistant"), "true")

	rep := traceBackfillReport{Tenant: tenant, UserID: userID, DryRun: dryRun, StopReason: "complete"}
	sessions, _, err := s.store.ListSessions(r.Context(),
		store.SessionFilter{TenantID: tenant, UserID: userID}, limit, 0)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	if len(sessions) == limit {
		// BOUNDED WORK, NOT BOUNDED RESULTS. The limit caps chats examined, so a
		// truncated run has to say so — an operator reading "complete" against a
		// partial sweep would never run it again.
		rep.StopReason = "limit"
		rep.Note = "stopped at the chat limit; re-run to continue"
	}
	ttl := s.cfg().Env.MemoryTraceMaxAge
	if ttl <= 0 {
		ttl = defaultTraceMaxAge
	}

	for _, sess := range sessions {
		rep.Sessions++
		events, terr := s.store.GetTranscript(r.Context(), sess.SessionID)
		if terr != nil {
			continue
		}
		for _, turn := range builtin.ConversationTurns(events) {
			if turn.Speaker == "assistant" && !withAssistant {
				continue
			}
			text := s.redactor.String(turn.Text)
			if strings.TrimSpace(text) == "" {
				continue
			}
			if len(text) > traceTurnMaxBytes {
				text = text[:traceTurnMaxBytes]
			}
			rep.TurnsFound++
			// DETERMINISTIC KEYS, so a re-run is idempotent rather than a second copy
			// of the archive. The live user path cannot key this way — it has no seq
			// in hand — which is exactly why a backfilled turn and a live one can
			// coexist without either overwriting the other.
			key := store.TraceTurnKeyPrefix + sess.SessionID + ":b" + strconv.FormatInt(turn.Seq, 10)
			if _, gerr := s.store.MemoryGet(r.Context(), tenant, store.MemoryScopeUser, userID, key); gerr == nil {
				rep.Skipped++
				continue
			}
			if dryRun {
				continue
			}
			value, merr := json.Marshal(traceTurnValue{
				Text: text, Speaker: turn.Speaker, SessionID: sess.SessionID,
				At: time.Now().UTC().Format(time.RFC3339Nano),
			})
			if merr != nil {
				continue
			}
			if serr := s.store.MemorySet(r.Context(), tenant, store.MemoryScopeUser, userID, key, value, ttl); serr != nil {
				continue
			}
			s.embedTraceTurn(r.Context(), tenant, userID, key, text)
			rep.Indexed++
		}
	}
	if dryRun {
		rep.Note = strings.TrimSpace(rep.Note + " DRY RUN — nothing was written. Re-send with dry_run=false.")
	}
	writeJSON(w, http.StatusOK, rep)
}
