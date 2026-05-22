// channels_admin.go — admin/list endpoints + v0.9.x Channel CRUD
// (publish / subscribe / peek / ack) for the operator-declared
// `channels:` yaml block.
//
// Phase 0 of the n8n integration RFC: GET /v1/_channels returns the
// operator-declared channel set joined with cheap aggregate stats
// from channel_messages, so n8n's credential-picker (and any other
// operator dashboard) can render a channel-name dropdown without
// re-parsing the loomcycle yaml.
//
// v0.9.x adds two parallel route families for the CRUD ops (Phase 1
// of the n8n integration RFC):
//
//   - /v1/_channels/{name}/{op}                — global-scope (operator
//     admin surface)
//   - /v1/users/{user_id}/channels/{name}/{op} — user-scope (per-end-
//     user surface)
//
// Both gate on the same operator-bearer token; the per-user route's
// only addition is that scope_id is server-derived from the URL path
// (callers can't forge a different user_id by lying in the body).
//
// All eight HTTP handlers delegate to the Connector methods in
// connector_impl_channels.go — same business logic for HTTP, gRPC,
// MCP, and the in-band Channel tool (via the shared store helpers).
package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// handleListChannels serves GET /v1/_channels. Bearer-authed.
// Dispatches through the Connector (the canonical impl lives in
// connector_impl_n8n.go) so MCP + gRPC and the HTTP handler return
// the same shape from the same code path.
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	resp, err := s.ListChannels(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- v0.9.x CRUD request body shapes ----------------------------

type channelPublishBody struct {
	Payload   json.RawMessage `json:"payload"`
	DeliverAt string          `json:"deliver_at,omitempty"`
}

type channelSubscribeBody struct {
	FromCursor  string `json:"from_cursor,omitempty"`
	MaxMessages int    `json:"max_messages,omitempty"`
	WaitMS      int    `json:"wait_ms,omitempty"`
}

type channelAckBody struct {
	Cursor string `json:"cursor"`
}

// ---- error mapping ----------------------------------------------

// writeChannelError maps the typed Connector errors to HTTP status +
// JSON error code. One place; every Channel CRUD handler funnels
// through it so the wire mapping stays consistent. The `code` field
// matches the strings the TS adapter / MCP tool will pattern-match on.
func writeChannelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connector.ErrChannelNotDeclared):
		writeJSONError(w, http.StatusNotFound, "channel_not_declared", err.Error())
	case errors.Is(err, connector.ErrChannelScopeInvalid):
		writeJSONError(w, http.StatusBadRequest, "channel_scope_invalid", err.Error())
	case errors.Is(err, connector.ErrChannelCursorRegression):
		writeJSONError(w, http.StatusConflict, "channel_cursor_regression", err.Error())
	case errors.Is(err, connector.ErrSystemPublisherUnwired):
		writeJSONError(w, http.StatusServiceUnavailable, "system_publisher_unwired", err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "channel_op_failed", err.Error())
	}
}

// ---- admin handlers (scope=global) ------------------------------

// handleAdminChannelPublish serves POST /v1/_channels/{name}/publish.
// Bearer-authed. scope=global, scope_id="". The existing system-only
// publish route (POST /v1/_channels/{name...}) keeps its semantics;
// the trailing `/publish` segment makes this pattern strictly more
// specific so Go 1.22+ mux picks it for matching URLs.
func (s *Server) handleAdminChannelPublish(w http.ResponseWriter, r *http.Request) {
	s.handleChannelPublish(w, r, r.PathValue("name"), "global", "")
}

// handleAdminChannelSubscribe serves POST /v1/_channels/{name}/subscribe.
// Bearer-authed long-poll. scope=global, scope_id="".
func (s *Server) handleAdminChannelSubscribe(w http.ResponseWriter, r *http.Request) {
	s.handleChannelSubscribe(w, r, r.PathValue("name"), "global", "")
}

// handleAdminChannelPeek serves GET /v1/_channels/{name}/peek. Bearer-
// authed non-destructive read. scope=global, scope_id="". Query
// params: ?from_cursor=&max_messages=.
func (s *Server) handleAdminChannelPeek(w http.ResponseWriter, r *http.Request) {
	s.handleChannelPeek(w, r, r.PathValue("name"), "global", "")
}

// handleAdminChannelAck serves POST /v1/_channels/{name}/ack. Bearer-
// authed cursor advance. scope=global, scope_id="".
func (s *Server) handleAdminChannelAck(w http.ResponseWriter, r *http.Request) {
	s.handleChannelAck(w, r, r.PathValue("name"), "global", "")
}

// ---- per-user handlers (scope=user) -----------------------------

// handleUserChannelPublish serves POST /v1/users/{user_id}/channels/{name}/publish.
// scope=user, scope_id resolved from the URL path so the bearer
// can't forge a different user_id in the body.
func (s *Server) handleUserChannelPublish(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_user_id", `user_id must match [A-Za-z0-9_-]{1,128}`)
		return
	}
	s.handleChannelPublish(w, r, r.PathValue("name"), "user", userID)
}

// handleUserChannelSubscribe serves POST /v1/users/{user_id}/channels/{name}/subscribe.
func (s *Server) handleUserChannelSubscribe(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_user_id", `user_id must match [A-Za-z0-9_-]{1,128}`)
		return
	}
	s.handleChannelSubscribe(w, r, r.PathValue("name"), "user", userID)
}

// handleUserChannelPeek serves GET /v1/users/{user_id}/channels/{name}/peek.
func (s *Server) handleUserChannelPeek(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_user_id", `user_id must match [A-Za-z0-9_-]{1,128}`)
		return
	}
	s.handleChannelPeek(w, r, r.PathValue("name"), "user", userID)
}

// handleUserChannelAck serves POST /v1/users/{user_id}/channels/{name}/ack.
func (s *Server) handleUserChannelAck(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_user_id", `user_id must match [A-Za-z0-9_-]{1,128}`)
		return
	}
	s.handleChannelAck(w, r, r.PathValue("name"), "user", userID)
}

// ---- shared helpers (the 4 op impls; scope-agnostic) ------------

func (s *Server) handleChannelPublish(w http.ResponseWriter, r *http.Request, name, scope, scopeID string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", "missing channel name in URL path")
		return
	}
	var body channelPublishBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	out, err := s.PublishChannel(r.Context(), connector.ChannelPublishRequest{
		Channel:   name,
		Scope:     scope,
		ScopeID:   scopeID,
		Payload:   body.Payload,
		DeliverAt: body.DeliverAt,
	})
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleChannelSubscribe(w http.ResponseWriter, r *http.Request, name, scope, scopeID string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", "missing channel name in URL path")
		return
	}
	var body channelSubscribeBody
	// Subscribe body is optional — empty body means "poll once from
	// committed cursor, no wait." JSON-decode failure on an empty
	// body is expected (io.EOF); only surface real parse errors.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
			return
		}
	}
	out, err := s.SubscribeChannel(r.Context(), connector.ChannelSubscribeRequest{
		Channel:     name,
		Scope:       scope,
		ScopeID:     scopeID,
		FromCursor:  body.FromCursor,
		MaxMessages: body.MaxMessages,
		WaitMS:      body.WaitMS,
	})
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleChannelPeek(w http.ResponseWriter, r *http.Request, name, scope, scopeID string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", "missing channel name in URL path")
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("max_messages"); v != "" {
		// Manual int parse — leave 0 (Connector defaults to 10) on
		// malformed input rather than 400ing. Same lenient posture
		// as the in-band tool's peek op.
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
			if n > 1000 {
				break
			}
		}
		limit = n
	}
	out, err := s.PeekChannel(r.Context(), connector.ChannelPeekRequest{
		Channel:     name,
		Scope:       scope,
		ScopeID:     scopeID,
		FromCursor:  q.Get("from_cursor"),
		MaxMessages: limit,
	})
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleChannelAck(w http.ResponseWriter, r *http.Request, name, scope, scopeID string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", "missing channel name in URL path")
		return
	}
	var body channelAckBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	out, err := s.AckChannel(r.Context(), connector.ChannelAckRequest{
		Channel: name,
		Scope:   scope,
		ScopeID: scopeID,
		Cursor:  body.Cursor,
	})
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
