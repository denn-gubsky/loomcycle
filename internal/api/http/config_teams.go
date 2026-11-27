package http

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// ReconcileConfigTeams writes the yaml's teams into the store's shared tier
// (RFC GS P2) through the TeamDef tool, under the operator plane's own grants:
// the substrate admin ctx an operator's off-run create gets, so a configured
// team passes exactly the gates the same team created over the API would. ctx
// must carry no principal — it is the boot's, not a request's — which makes it
// the operator's (open mode) and leaves the promoter unrestricted.
//
// A server with no TeamDef tool reconciles nothing. What the reconcile did is
// kept for GET /v1/_config/teams.
func (s *Server) ReconcileConfigTeams(ctx context.Context, teams map[string]config.Team) (builtin.ConfigTeamsReport, error) {
	td, ok := s.teamDefTool.(*builtin.TeamDef)
	if !ok || td == nil {
		return builtin.ConfigTeamsReport{}, nil
	}
	rep, err := td.ReconcileConfigTeams(substrateAdminCtx(ctx), teams)
	run := &configTeamsRun{At: time.Now().UTC(), Report: rep}
	if err != nil {
		run.Err = err.Error()
	}
	s.configTeamsLast.Store(run)
	return rep, err
}

// configTeamsRun is one boot reconcile, as this replica ran it.
type configTeamsRun struct {
	At     time.Time
	Report builtin.ConfigTeamsReport
	Err    string
}

// The status of a configured team, as GET /v1/_config/teams reports it.
const (
	// configTeamWritten: the shared tier's active version is the one the
	// configuration wrote.
	configTeamWritten = "written"
	// configTeamConflict: the shared tier's active version of the name was
	// made over the API, and the boot left it alone (RFC GS D3).
	configTeamConflict = "conflict"
	// configTeamNotWritten: the shared tier has no live active version of the
	// name: the boot refused the team, or has not written it yet.
	configTeamNotWritten = "not_written"
)

// configTeamStatus is one row of GET /v1/_config/teams.
type configTeamStatus struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	ActiveDefID string `json:"active_def_id,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// handleConfigTeams serves GET /v1/_config/teams (admin): for each team in the
// configuration, whether the shared tier holds the version the configuration
// wrote. The boot only logs a name conflict or a refused team, and the log is
// where an operator looking a week later does not look; this is where the
// Settings health panel reads it.
//
// The status is read from the store, so every replica answers the same. The
// reason a team was refused exists only in the memory of the replica that ran
// the reconcile; another replica says so instead.
func (s *Server) handleConfigTeams(w http.ResponseWriter, r *http.Request) {
	teams := s.cfg().Teams
	rows, err := s.store.TeamDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	body := configTeamsResponse(teams, rows, s.configTeamsLast.Load())
	body["deprecations"] = deprecationsReport(s.deprecatedUse.snapshot())
	writeJSONOK(w, body)
}

// configTeamsResponse builds the GET /v1/_config/teams body from the
// configured teams, the store's name listing and this replica's reconcile.
func configTeamsResponse(teams map[string]config.Team, rows []store.TeamDefNameSummary, last *configTeamsRun) map[string]any {
	shared := make(map[string]store.TeamDefNameSummary, len(rows))
	for _, row := range rows {
		if row.TenantID == "" {
			shared[row.Name] = row
		}
	}
	refused := map[string]string{}
	if last != nil {
		for _, p := range last.Report.Refused {
			refused[p.Name] = p.Detail
		}
	}

	names := make([]string, 0, len(teams))
	for name := range teams {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]configTeamStatus, 0, len(names))
	for _, name := range names {
		st := configTeamStatus{Name: name}
		row, ok := shared[name]
		switch {
		case !ok || row.ActiveDefID == "" || row.ActiveRetired:
			st.Status = configTeamNotWritten
			switch {
			case refused[name] != "":
				st.Detail = refused[name]
			case last == nil:
				st.Detail = "no live version in the shared tier; this replica did not run the boot reconcile, so the reason is in the log of the one that did"
			default:
				st.Detail = "no live version in the shared tier"
			}
		case row.ActiveSource == store.TeamDefSourceConfig:
			st.Status = configTeamWritten
			st.ActiveDefID = row.ActiveDefID
		default:
			st.Status = configTeamConflict
			st.ActiveDefID = row.ActiveDefID
			st.Detail = "the shared tier's active version was made over the API, so the configured team was not written; retire that version or rename one of the two, then restart"
		}
		out = append(out, st)
	}
	body := map[string]any{"teams": out}
	if last != nil {
		body["reconciled_at"] = last.At
		if last.Err != "" {
			body["reconcile_error"] = last.Err
		}
	}
	return body
}
