package http

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// listEventsResponse is the wire shape of GET /v1/_events.
//
// `total` is the unbounded match count for the filter — pagination
// UIs need it to render "page N of M". `events` is the limit-bounded
// slice for the requested window.
type listEventsResponse struct {
	Events []wireEvent `json:"events"`
	Total  int64       `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// wireEvent decouples the on-the-wire shape from the store row. We
// keep payload as raw json.RawMessage so the UI can re-decode it the
// same way the transcript endpoint does, without server-side
// re-marshalling.
type wireEvent struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id"`
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// handleListEvents serves GET /v1/_events?type=X&from=Y&to=Z&limit=N&offset=M
// — paginated cross-session event log for the v0.8.21 Audit view.
// Bearer-authed admin surface.
//
// Filter params (all optional):
//
//	type=tool_call      exact match on event.type
//	from=RFC3339        ts >= from
//	to=RFC3339          ts <= to
//	limit=50            page size (clamped to 1..500, default 50)
//	offset=0            page offset (default 0)
//
// Returns 503 when the server boots without a store.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; event audit requires a persistent store")
		return
	}
	q := r.URL.Query()

	var filter store.EventFilter
	filter.Type = q.Get("type")

	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"from must be RFC3339: "+err.Error())
			return
		}
		filter.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"to must be RFC3339: "+err.Error())
			return
		}
		filter.To = t
	}

	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"limit must be a positive integer")
			return
		}
		limit = n
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"offset must be a non-negative integer")
			return
		}
		offset = n
	}

	// RFC AS: tenant-scope the audit. admin / legacy / open see all (honoring an
	// optional ?tenant= focus); a substrate:tenant principal sees only its own
	// tenant's events (ListEvents filters via the owning session's tenant).
	if tenantID, all := s.principalTenantScope(r.Context(), q.Get("tenant")); !all {
		filter.TenantID = tenantID
	}

	events, total, err := s.store.ListEvents(r.Context(), filter, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := listEventsResponse{
		Events: make([]wireEvent, 0, len(events)),
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}
	for _, ev := range events {
		resp.Events = append(resp.Events, wireEvent{
			Seq:       ev.Seq,
			SessionID: ev.SessionID,
			RunID:     ev.RunID,
			Timestamp: ev.Timestamp,
			Type:      ev.Type,
			Payload:   json.RawMessage(ev.Payload),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
