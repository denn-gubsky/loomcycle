// Package http — RFC CK runtime config reload.
//
// POST /v1/_config/reload re-loads the layered YAML config at runtime and applies
// the changes that CAN be applied without a restart, reporting the rest. It is
// modeled on the resolve-probe escape hatch (resolver.go): an admin endpoint that
// mutates a long-lived subsystem in place so an operator never has to restart
// (which would drop in-flight runs) to pick up a config edit.
//
// Phase 2 scope (this file): the mechanism + the provider/resolver reloader. A
// changed `providers` / `models` / `tiers` / `provider_priority` section takes
// effect on the NEXT run (in-place provider-set rebuild + resolver-policy swap);
// every other changed section is reported as restart-required. Later phases move
// more sections from restart-required to applied (they need the s.cfg holder
// migration — user_tiers, auth, memory, scheduler, skills, concurrency, …).
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// reloadableSections are the top-level config sections a reload APPLIES in the
// current build (Phase 2). A changed section outside this set is reported under
// restart_required. user_tiers is deliberately absent: it is read live off
// s.cfg at run admission, so it needs the config-holder migration a later phase
// adds, not the resolver-policy swap.
var reloadableSections = map[string]bool{
	"providers":         true,
	"models":            true,
	"tiers":             true,
	"provider_priority": true,
}

// ReloadResult is the POST /v1/_config/reload response (and dry-run diff).
type ReloadResult struct {
	// DryRun is true when the caller asked only for the diff (?dry_run=1); no
	// change was applied.
	DryRun bool `json:"dry_run"`
	// Applied lists the changed sections this build reloaded in place (took effect
	// on the next run). Empty when nothing reloadable changed.
	Applied []string `json:"applied"`
	// RestartRequired lists changed sections this build cannot apply live; a
	// restart is needed to pick them up.
	RestartRequired []string `json:"restart_required"`
	// Warnings surfaces the candidate config's load-time warnings (cross-layer
	// overrides, etc.), the same ones logged at boot.
	Warnings []string `json:"warnings,omitempty"`
}

// errReloadUnavailable is returned when no reloader was wired (a Server built
// without SetConfigReloader — tests / embedded); the handler maps it to 503.
var errReloadUnavailable = errors.New("config reload not wired on this server")

// SetConfigReloader injects the two hooks the reload engine needs, and seeds the
// diff baseline with the boot config. load re-assembles + re-loads the layered
// config (config.LoadLayers, which validates — a bad candidate returns an error
// and the running config is kept); apply rebuilds the reloadable subsystems
// (provider set + resolver policy) in place from a validated candidate. Called
// once by cmd/loomcycle after the Server + resolver are wired.
func (s *Server) SetConfigReloader(load func() (*config.Config, error), apply func(*config.Config) error) {
	s.reloadLoad = load
	s.reloadApply = apply
	if s.reloadBaseline.Load() == nil {
		s.reloadBaseline.Store(s.cfg)
	}
}

// ReloadConfig re-loads the layered config and, unless dryRun, applies the
// reloadable subset. It is the canonical implementation behind
// POST /v1/_config/reload. Validate-before-apply: a candidate that fails to load
// or validate is rejected whole (error returned, running config untouched). The
// applied/restart-required split is computed by diffing the candidate against the
// last-applied baseline. Concurrent calls are serialized so two reloads can't
// interleave a diff with an apply.
func (s *Server) ReloadConfig(ctx context.Context, dryRun bool) (ReloadResult, error) {
	if s.reloadLoad == nil || s.reloadApply == nil {
		return ReloadResult{}, errReloadUnavailable
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	// (1) Load + validate the candidate. LoadLayers IS the validation gate; a bad
	// edit returns an error here and nothing is touched.
	candidate, err := s.reloadLoad()
	if err != nil {
		return ReloadResult{}, fmt.Errorf("%w: %v", config.ErrReloadInvalid, err)
	}

	// (2) Diff against the last-applied baseline to classify the changes.
	baseline := s.reloadBaseline.Load()
	changed, err := config.ChangedSections(baseline, candidate)
	if err != nil {
		return ReloadResult{}, fmt.Errorf("%w: diffing config: %v", config.ErrReloadInvalid, err)
	}
	res := ReloadResult{DryRun: dryRun, Applied: []string{}, RestartRequired: []string{}, Warnings: candidate.Warnings}
	for _, sec := range changed {
		if reloadableSections[sec] {
			res.Applied = append(res.Applied, sec)
		} else {
			res.RestartRequired = append(res.RestartRequired, sec)
		}
	}

	// (3) Dry run stops here — report the diff, apply nothing.
	if dryRun {
		return res, nil
	}

	// (4) Apply the reloadable subsystems only when a reloadable section actually
	// changed (an apply rebuilds the provider set + re-probes, so skip it when
	// only restart-required sections moved — no churn). On apply failure the
	// baseline is NOT advanced, so the running subsystems keep the old config.
	if len(res.Applied) > 0 {
		if err := s.reloadApply(candidate); err != nil {
			return ReloadResult{}, fmt.Errorf("%w: %v", config.ErrReloadApply, err)
		}
	}
	// Advance the baseline to the candidate regardless of whether a reloadable
	// section changed: a subsequent reload should diff against what the operator
	// most recently loaded, so an unchanged-then-changed sequence is detected.
	s.reloadBaseline.Store(candidate)
	return res, nil
}

// handleConfigReload is POST /v1/_config/reload (admin-gated by the /v1/_ route
// prefix). ?dry_run=1 returns the section diff without applying. Mirrors
// handleResolveProbe's shape: delegate to the canonical method, map typed errors
// to status codes, encode the result.
func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	res, err := s.ReloadConfig(r.Context(), dryRun)
	if err != nil {
		switch {
		case errors.Is(err, errReloadUnavailable):
			writeJSONError(w, http.StatusServiceUnavailable, "reload_unavailable",
				"runtime config reload is not enabled on this server")
		case errors.Is(err, config.ErrReloadInvalid):
			// The candidate failed to load/validate — the running config is
			// unchanged. 422 (not 500): the operator's edit is at fault.
			writeJSONError(w, http.StatusUnprocessableEntity, "config_invalid", err.Error())
		case errors.Is(err, config.ErrReloadApply):
			writeJSONError(w, http.StatusInternalServerError, "reload_apply_failed", err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}
