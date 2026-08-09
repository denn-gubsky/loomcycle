package http

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
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
	// RFC BX P2a — the first-class users-table fields, merged over the
	// run-derived activity above. `Registered` distinguishes a managed user
	// row (true) from a subject seen only in runs (false); the record fields
	// are empty for the latter. Additive — the base four fields are
	// unchanged so the Web UI's picker keeps working.
	Registered  bool   `json:"registered"`
	DisplayName string `json:"display_name"`
	AccessMode  string `json:"access_mode"`
	Status      string `json:"status"`
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
	// Tenant scope: a tenant principal sees only its own tenant's users;
	// super-admin sees all, or focuses one via ?tenant= (the UI's tenant
	// switcher). all=true → "" filter (every tenant).
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	if all {
		tenantID = ""
	}
	users, err := s.store.ListUsers(r.Context(), tenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// RFC BX P2a: merge the first-class users-table rows over the run-derived
	// activity so the response is authoritative about who is REGISTERED while
	// staying an activity lens. Both sources are scoped by the SAME tenantID:
	//   - a subject with runs but no record → registered:false, empty fields;
	//   - a registered subject with no runs → appears with zero activity;
	//   - a subject in both               → activity + record fields merged.
	records, err := s.store.UserList(r.Context(), tenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := listUsersResponse{Users: make([]wireUserSummary, 0, len(users)+len(records))}
	// Index by user_id to fold records onto activity rows. In the admin
	// all-tenants view (tenantID=="") ListUsers already collapses a subject
	// across tenants (GROUP BY user_id), so this lens does too; the exact
	// per-tenant view is the tenant-scoped read.
	idx := map[string]int{}
	for _, u := range users {
		idx[u.UserID] = len(resp.Users)
		resp.Users = append(resp.Users, toWireUser(u))
	}
	for _, rec := range records {
		if i, ok := idx[rec.Subject]; ok {
			resp.Users[i].Registered = true
			resp.Users[i].DisplayName = rec.DisplayName
			resp.Users[i].AccessMode = rec.AccessMode
			resp.Users[i].Status = rec.Status
			continue
		}
		idx[rec.Subject] = len(resp.Users)
		resp.Users = append(resp.Users, wireUserSummary{
			UserID:      rec.Subject,
			Registered:  true,
			DisplayName: rec.DisplayName,
			AccessMode:  rec.AccessMode,
			Status:      rec.Status,
		})
	}
	// RFC BX P2b: an ISOLATED member (substrate:user) may see ONLY its own user
	// record — never the tenant roster (that is tenant-shared information an
	// isolated member has no authority over). Server-derived from the principal;
	// the roster fetched above was already tenant-scoped, so this narrows to self
	// before anything is written. Admin / tenant / open-mode are unaffected.
	if s.isolatedForCtx(r.Context()) {
		p, _ := auth.PrincipalFromContext(r.Context())
		selfOnly := make([]wireUserSummary, 0, 1)
		for _, u := range resp.Users {
			if u.UserID == p.Subject {
				selfOnly = append(selfOnly, u)
			}
		}
		resp.Users = selfOnly
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
