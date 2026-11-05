package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// auditExportRow is one line of GET /v1/_audit: a row of the store-backed
// audit chain (RFC FZ Part E) with everything an offline check needs to
// re-link it — the exact nanosecond time and the payload as the string that
// was hashed, not a re-encoding of it.
type auditExportRow struct {
	Tenant   string    `json:"tenant"`
	Seq      int64     `json:"seq"`
	TS       time.Time `json:"ts"`
	TSNanos  int64     `json:"ts_ns"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Subject  string    `json:"subject"`
	Payload  string    `json:"payload"`
	PrevHash string    `json:"prev_hash"`
	Hash     string    `json:"hash"`
	MAC      string    `json:"mac"`
}

// auditExportPage is how many rows the export reads at a time.
const auditExportPage = 500

// handleAuditExport serves GET /v1/_audit?tenant=&since=&after_seq=&format=jsonl:
// one tenant's audit chain as JSON Lines, oldest first.
//
// Which chain: a tenant operator's own tenant, whatever ?tenant= names (as
// every tenant-scoped report does). An admin (or the legacy token, or open
// mode) names it with ?tenant=; none is the operator chain — the events that
// belong to no tenant, such as failed sign-ins before a tenant is known. A
// principal of the "" tenant without admin is refused: its chain is the
// operator chain.
//
// since (RFC3339) keeps rows recorded at or after it; after_seq resumes after
// a row already read. format is jsonl, the only one. Rows are read in pages
// and streamed; a read failure after the first page ends the stream early
// (logged) — a reader can tell by the last seq against the head it expects.
//
// Not to be confused with GET /v1/_events: that is the run event log.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "the audit export needs a persistent store")
		return
	}
	q := r.URL.Query()
	if f := q.Get("format"); f != "" && f != "jsonl" {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "format must be jsonl")
		return
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
			return
		}
		since = t
	}
	var after int64
	if v := q.Get("after_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "after_seq must be a non-negative integer")
			return
		}
		after = n
	}
	tenant, _ := s.principalTenantScope(r.Context(), q.Get("tenant"))
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && tenant == "" && !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "the operator audit chain is the admin's to read")
		return
	}

	ctx := r.Context()
	rows, err := s.store.AuditEventsList(ctx, tenant, after, auditExportPage)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "reading the audit chain failed")
		slog.ErrorContext(ctx, "audit export: read failed", "tenant", tenant, "err", err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	for len(rows) > 0 {
		for _, row := range rows {
			after = row.Seq
			if !since.IsZero() && row.TS.Before(since) {
				continue
			}
			if err := enc.Encode(auditExportRow{
				Tenant: row.TenantID, Seq: row.Seq, TS: row.TS, TSNanos: row.TS.UnixNano(),
				Actor: row.Actor, Action: row.Action, Subject: row.Subject, Payload: row.Payload,
				PrevHash: row.PrevHash, Hash: row.Hash, MAC: row.MAC,
			}); err != nil {
				return // the client went away
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		if len(rows) < auditExportPage {
			return
		}
		if rows, err = s.store.AuditEventsList(ctx, tenant, after, auditExportPage); err != nil {
			slog.ErrorContext(ctx, "audit export: read failed mid-stream; the export ends early", "tenant", tenant, "after_seq", after, "err", err)
			return
		}
	}
}
