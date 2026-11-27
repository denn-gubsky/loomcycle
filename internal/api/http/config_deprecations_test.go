package http

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC GW P14 Q5, kept by P15 Q3: the endpoint the Settings health panel reads
// carries this replica's counts of runs whose definition names what v1.172.0
// removed; its config list is always empty, since such a config does not
// load. It is an admin route, so a tenant operator never reads another
// tenant's agents through it.
func TestConfigTeams_ConfigListIsEmptyRunsListsDropped(t *testing.T) {
	if rt, ok := authz.HTTPRoute("GET", "/v1/_config/teams"); !ok || rt.Scope != authz.ScopeAdmin {
		t.Fatalf("GET /v1/_config/teams route = %+v (found %v), want scope %s", rt, ok, authz.ScopeAdmin)
	}

	h := newTriggerWalkHarness(t)
	type report struct {
		Config []string           `json:"config"`
		Runs   []deprecatedUseRow `json:"runs"`
	}
	read := func() report {
		t.Helper()
		_, body := configTeamsGet(t, h.srv)
		var r report
		if err := json.Unmarshal(body["deprecations"], &r); err != nil {
			t.Fatalf("deprecations = %s: %v", body["deprecations"], err)
		}
		if r.Config == nil || r.Runs == nil {
			t.Errorf("deprecations = %s, want both lists present", body["deprecations"])
		}
		return r
	}

	if r := read(); len(r.Config) != 0 || len(r.Runs) != 0 {
		t.Errorf("nothing used: %+v, want empty lists", r)
	}

	h.srv.deprecatedUse.note(context.Background(), "acme", "stored", config.AgentDef{
		Tools:         []string{"Bashbox"},
		RemovedFields: config.RemovedFieldsInJSON([]byte(`{"shell_credentials":["GITHUB_TOKEN"]}`)),
	}, true)

	r := read()
	if len(r.Config) != 0 {
		t.Errorf("config = %v, want always empty: a config naming a removed item does not load", r.Config)
	}
	want := []deprecatedUseRow{{"field", "shell_credentials", 1}, {"tool", "Bashbox", 1}}
	if !reflect.DeepEqual(r.Runs, want) {
		t.Errorf("runs = %+v, want %+v", r.Runs, want)
	}
	_, body := configTeamsGet(t, h.srv)
	if strings.Contains(string(body["deprecations"]), "GITHUB_TOKEN") {
		t.Errorf("deprecations name a listed credential: %s", body["deprecations"])
	}
}
