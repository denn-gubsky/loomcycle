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
	case errors.Is(err, connector.ErrChannelYamlImmutable):
		writeJSONError(w, http.StatusConflict, "channel_yaml_immutable", err.Error())
	case errors.Is(err, connector.ErrChannelAlreadyExists):
		writeJSONError(w, http.StatusConflict, "channel_name_in_use", err.Error())
	case errors.Is(err, connector.ErrChannelNotFound):
		writeJSONError(w, http.StatusNotFound, "channel_not_found", err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "channel_op_failed", err.Error())
	}
}

// ---- v0.11.5 channel admin CRUD handlers ------------------------

// handleCreateChannel serves POST /v1/_channels.
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req connector.ChannelCreateRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	desc, err := s.CreateChannel(r.Context(), req)
	if err != nil {
		// Preserve the channel-specific code mapping; fall back to
		// generic 400 for the create-only validation strings (which
		// don't wrap typed sentinels).
		if errors.Is(err, connector.ErrChannelYamlImmutable) ||
			errors.Is(err, connector.ErrChannelAlreadyExists) ||
			errors.Is(err, connector.ErrChannelNotFound) {
			writeChannelError(w, err)
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(desc)
}

// handleUpdateChannel serves PATCH /v1/_channels/{name}.
func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req connector.ChannelUpdateRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
			return
		}
	}
	desc, err := s.UpdateChannel(r.Context(), name, req)
	if err != nil {
		if errors.Is(err, connector.ErrChannelYamlImmutable) ||
			errors.Is(err, connector.ErrChannelAlreadyExists) ||
			errors.Is(err, connector.ErrChannelNotFound) {
			writeChannelError(w, err)
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(desc)
}

// handleDeleteChannel serves DELETE /v1/_channels/{name}.
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.DeleteChannel(r.Context(), name); err != nil {
		if errors.Is(err, connector.ErrChannelYamlImmutable) ||
			errors.Is(err, connector.ErrChannelNotFound) {
			writeChannelError(w, err)
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChannelPurge serves POST /v1/_channels/{name}/purge — clears
// buffered messages without deleting the channel. Allowed on yaml
// channels (unlike DELETE), which is the whole point: drain a yaml
// channel that accumulated junk without a restart or a raw DB delete.
func (s *Server) handleChannelPurge(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.PurgeChannel(r.Context(), name)
	if err != nil {
		if errors.Is(err, connector.ErrChannelNotFound) {
			writeChannelError(w, err)
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
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

// handleAdminChannelAwait serves POST /v1/_channels/_await — the
// multi-channel fan-in twin of subscribe. The whole request is the body
// (channels[] + scope + mode + …), not a per-name path, so it decodes
// straight into the Connector request. scope/scope_id come from the body
// (default global). Long-poll up to wait_ms; a timeout is a 200 with
// timed_out:true, not an error.
func (s *Server) handleAdminChannelAwait(w http.ResponseWriter, r *http.Request) {
	var req connector.ChannelAwaitRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	out, err := s.AwaitChannels(r.Context(), req)
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleAdminChannelBroadcast serves POST /v1/_channels/_broadcast — the
// multi-channel fan-out twin of publish. Atomic at the declare pre-flight
// (one undeclared channel → 404, nothing published).
func (s *Server) handleAdminChannelBroadcast(w http.ResponseWriter, r *http.Request) {
	var req connector.ChannelBroadcastRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	out, err := s.BroadcastChannels(r.Context(), req)
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---- per-user handlers (scope=user) -----------------------------

// userChannelScopeID validates the {user_id} path arg and enforces the
// per-user channel ownership gate, returning (scopeID, true) when the request
// may proceed. On a bad id or a cross-subject access it writes the response and
// returns ("", false).
//
// Ownership gate: channel_messages carry NO tenant column (the whole-tenant
// isolation that runs/sessions get would need the deferred tenant_id
// denormalisation), so the safe no-schema mitigation is per-SUBJECT — a
// non-admin principal may act ONLY on its own subject's channels. Without it a
// channel-scoped tenant token could read/write any user's channel by changing
// the path (user_ids aren't secret). Opaque 404 on mismatch (no existence
// oracle — same posture sessionOwnershipOK gives). Admin / legacy / open mode
// are unrestricted.
func (s *Server) userChannelScopeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID := r.PathValue("user_id")
	if !validIdent(userID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_user_id", `user_id must match [A-Za-z0-9_-]{1,128}`)
		return "", false
	}
	if !requirePrincipalOwnsPathUser(r.Context(), userID) {
		writeJSONError(w, http.StatusNotFound, "unknown_channel", "no such channel")
		return "", false
	}
	return userID, true
}

// handleUserChannelPublish serves POST /v1/users/{user_id}/channels/{name}/publish.
// scope=user, scope_id resolved from the URL path so the bearer
// can't forge a different user_id in the body.
func (s *Server) handleUserChannelPublish(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userChannelScopeID(w, r)
	if !ok {
		return
	}
	s.handleChannelPublish(w, r, r.PathValue("name"), "user", userID)
}

// handleUserChannelSubscribe serves POST /v1/users/{user_id}/channels/{name}/subscribe.
func (s *Server) handleUserChannelSubscribe(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userChannelScopeID(w, r)
	if !ok {
		return
	}
	s.handleChannelSubscribe(w, r, r.PathValue("name"), "user", userID)
}

// handleUserChannelPeek serves GET /v1/users/{user_id}/channels/{name}/peek.
func (s *Server) handleUserChannelPeek(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userChannelScopeID(w, r)
	if !ok {
		return
	}
	s.handleChannelPeek(w, r, r.PathValue("name"), "user", userID)
}

// handleUserChannelAck serves POST /v1/users/{user_id}/channels/{name}/ack.
func (s *Server) handleUserChannelAck(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userChannelScopeID(w, r)
	if !ok {
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
