package http

import (
	"net/http"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
)

// RFC AQ — read-only HTTP introspection of the embedded config presets/bundles
// and the env catalogue, mirroring the `loomcycle presets` / `env-template` CLI
// so they're reachable from the Web UI Settings hub (TrueNAS has no shell —
// RFC AR). All three fall under the /v1/_* operator-admin scope gate
// (requiredScopeFor): admin-only, like the rest of the Settings surface. The
// content is non-secret (only token_env *names*), so the gate is about keeping
// the operability hub coherent, not about secrecy.

// handleListPresets serves GET /v1/_presets — the embedded units (presets +
// bundles) with name/kind/description, for the Settings → Presets list.
func (s *Server) handleListPresets(w http.ResponseWriter, r *http.Request) {
	units := embedded.Units()
	out := make([]map[string]any, 0, len(units))
	for _, u := range units {
		out = append(out, map[string]any{
			"name":        u.Name,
			"kind":        u.Kind,
			"description": u.Description,
		})
	}
	writeJSONOK(w, map[string]any{"units": out})
}

// handleShowPreset serves GET /v1/_presets/{name} — one unit's YAML (read or
// fork it). Unknown name → 404.
func (s *Server) handleShowPreset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := embedded.Show(name)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "unknown_preset", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"name": name, "yaml": string(data)})
}

// handleEnvTemplate serves GET /v1/_env_template — the embedded
// .env.insecure.example (non-secret env catalogue) so an operator can scaffold
// .env.insecure from the Web UI without a source checkout.
func (s *Server) handleEnvTemplate(w http.ResponseWriter, r *http.Request) {
	writeJSONOK(w, map[string]any{"env": string(embedded.EnvTemplate())})
}
