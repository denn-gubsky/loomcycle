package http

import (
	"encoding/json"
	"net/http"
)

// modelAliasesResponse is the wire shape for GET /v1/_models: the operator's
// configured model aliases (the top-level `models:` map — RFC AN). A UI (e.g.
// loomboard) reads this to offer aliases in a model picker and STORE the alias
// on an agent/fork instead of a concrete model. Storing the alias means the
// agent tracks the operator's local override of that alias — retarget it once in
// yaml and every agent using it follows, no re-fork — whereas a concrete model
// pins a specific tag that only a new fork can change.
type modelAliasesResponse struct {
	Aliases map[string]modelAliasWire `json:"aliases"`
}

type modelAliasWire struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// handleListModels returns the configured model aliases (cfg.Models). Non-secret
// — provider + model names only, the same shape an operator writes in yaml.
// Tenant-readable: a substrate:tenant operator's UI needs the alias list to
// build a model picker; the aliases are global config (not tenant-scoped), so
// every authed caller sees the same set. json.Marshal emits the map with keys
// sorted, so the output is stable.
func (s *Server) handleListModels(w http.ResponseWriter, _ *http.Request) {
	resp := modelAliasesResponse{Aliases: make(map[string]modelAliasWire, len(s.cfg.Models))}
	for name, ref := range s.cfg.Models {
		resp.Aliases[name] = modelAliasWire{Provider: ref.Provider, Model: ref.Model}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}
