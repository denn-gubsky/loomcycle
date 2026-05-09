package http

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// listUsersResponse is the wire shape of GET /v1/_users.
type listUsersResponse struct {
	Users []wireUserSummary `json:"users"`
}

type wireUserSummary struct {
	UserID        string    `json:"user_id"`
	RunningCount  int       `json:"running_count"`
	TotalCount    int       `json:"total_count"`
	LastStartedAt time.Time `json:"last_started_at"`
}

// handleListUsers serves GET /v1/_users — distinct user_ids with
// summary stats. Drives the Web UI's user picker so operators can
// see who has active runs without typing UUIDs.
//
// Returns 503 with `store_unavailable` when the server boots without
// a store (test harnesses; Memory-only configs). The empty case (no
// runs yet) returns 200 with an empty users array — the UI renders
// "no users yet" rather than treating empty as an error.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; user listing requires a persistent store")
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := listUsersResponse{Users: make([]wireUserSummary, 0, len(users))}
	for _, u := range users {
		resp.Users = append(resp.Users, toWireUser(u))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func toWireUser(u store.UserSummary) wireUserSummary {
	return wireUserSummary{
		UserID:        u.UserID,
		RunningCount:  u.RunningCount,
		TotalCount:    u.TotalCount,
		LastStartedAt: u.LastStartedAt,
	}
}
