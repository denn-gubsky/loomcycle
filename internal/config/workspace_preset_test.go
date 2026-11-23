package config

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/embedded"
)

// The embedded workspace preset (D28 as amended) loads, validates, and
// declares exactly the three public read-only routes, with no credential, and
// turns nothing else on.
func TestWorkspacePreset_DeclaresThePublicRegistryRoutes(t *testing.T) {
	data, err := embedded.Show("workspace")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(transcriptionBase)}, Layer{Name: "workspace", Data: data, Embedded: true})
	if err != nil {
		t.Fatalf("the workspace preset must load: %v", err)
	}
	ws := cfg.Workspace.Resolved()
	want := map[string]EgressRoute{
		"pypi":       {URL: "https://pypi.org/", Methods: []string{"GET", "HEAD"}},
		"pypi-files": {URL: "https://files.pythonhosted.org/", Methods: []string{"GET", "HEAD"}},
		"npm":        {URL: "https://registry.npmjs.org/", Methods: []string{"GET", "HEAD"}},
	}
	if !reflect.DeepEqual(ws.Egress.Routes, want) {
		t.Errorf("routes = %+v, want %+v", ws.Egress.Routes, want)
	}
	if ws.Mode != WorkspaceModeOff || ws.Egress.TenantRoutes || len(ws.Egress.TenantHosts) != 0 {
		t.Errorf("the preset turned something on: mode %q, tenant_routes %v, tenant_hosts %v", ws.Mode, ws.Egress.TenantRoutes, ws.Egress.TenantHosts)
	}
	for name, r := range ws.Egress.Routes {
		if r.Writes() {
			t.Errorf("route %s writes", name)
		}
	}
}
