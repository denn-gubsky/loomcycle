// Package http — RFC CK runtime config reload.
//
// POST /v1/_config/reload re-loads the layered YAML config at runtime and applies
// the changes that CAN be applied without a restart, reporting the rest. It is
// modeled on the resolve-probe escape hatch (resolver.go): an admin endpoint that
// mutates a long-lived subsystem in place so an operator never has to restart
// (which would drop in-flight runs) to pick up a config edit.
//
// What reloads live: the resolver sections (providers/models/tiers/
// provider_priority) via an in-place provider-set + resolver-policy rebuild, plus
// every section read live off s.cfg() (user_tiers, agents, defaults) via the
// atomic config-holder swap. Everything consumed by a boot-built subsystem
// (memory, scheduler, skills, concurrency, host allowlists, the listen address,
// the store DSN) is reported restart_required until a later phase adds its reloader.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// holderLiveSections take effect on the atomic config-holder swap ALONE — read
// live off s.cfg() at run admission (user_tiers / agents / defaults) or per
// channel op (channels: scope / ACL / period / ttl, via s.cfg().Channels). No
// subsystem rebuild is needed, so a reload applies them for free. (Heartbeat
// CHANNEL timing is boot-fixed and stays restart_required — a niche carve-out.)
var holderLiveSections = map[string]bool{
	"user_tiers": true,
	"agents":     true,
	"defaults":   true,
	"channels":   true,
}

// SectionReloader rebuilds one or more boot-snapshot subsystems in place from a
// validated candidate config (RFC CK). cmd/loomcycle injects one per subsystem,
// declaring the config sections it owns; ReloadConfig calls its Reload when any
// of those sections changed, and reports the owned sections as applied. Reloaders
// mutate their subsystem in place (a mutex-guarded swap), so the Server's handle
// pointers never change and every reader keeps working without a swap.
//
// Sections consumed by a subsystem with NO reloader (memory — the embedder is
// captured by-value in many consumers and a model/dimension change invalidates
// stored vectors, needing the /v1/_memory/reembed migration; skills — three
// aliased holders plus runtime mutation; the listen address / store DSN) are
// reported restart_required. Host allowlists are ENV-only, so a YAML reload can
// never change them.
type SectionReloader struct {
	Sections []string
	Reload   func(*config.Config) error
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

// SetConfigReloader injects the reload hooks. load re-loads the layered config
// (config.LoadLayers, which validates — a bad candidate returns an error and the
// running config is kept); reloaders is the set of per-subsystem in-place
// rebuilders (provider/resolver, concurrency, scheduled_runs, …), each owning the
// config sections it applies. The diff baseline is the holder's current config
// (set in New) — no separate baseline pointer to seed. Called once by
// cmd/loomcycle after the Server + subsystems are wired.
func (s *Server) SetConfigReloader(load func() (*config.Config, error), reloaders ...SectionReloader) {
	s.reloadLoad = load
	s.reloaders = reloaders
	// The applied set = the free holder-live sections ∪ every reloader's owned
	// sections. Everything else is restart_required.
	set := make(map[string]bool, len(holderLiveSections))
	for sec := range holderLiveSections {
		set[sec] = true
	}
	for _, r := range reloaders {
		for _, sec := range r.Sections {
			set[sec] = true
		}
	}
	s.reloadableSet = set
}

// ReloadConfig re-loads the layered config and, unless dryRun, applies the
// reloadable subset. It is the canonical implementation behind
// POST /v1/_config/reload. Validate-before-apply: a candidate that fails to load
// or validate is rejected whole (error returned, running config untouched). The
// applied/restart-required split is computed by diffing the candidate against the
// running config; each reloader whose owned section changed runs in place.
// Concurrent calls are serialized so two reloads can't interleave a diff and an apply.
func (s *Server) ReloadConfig(ctx context.Context, dryRun bool) (ReloadResult, error) {
	if s.reloadLoad == nil {
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

	// (2) Diff against the running config (the holder's current value) and classify
	// each changed section into applied vs restart-required.
	changed, err := config.ChangedSections(s.cfg(), candidate)
	if err != nil {
		return ReloadResult{}, fmt.Errorf("%w: diffing config: %v", config.ErrReloadInvalid, err)
	}
	changedSet := make(map[string]bool, len(changed))
	res := ReloadResult{DryRun: dryRun, Applied: []string{}, RestartRequired: []string{}, Warnings: candidate.Warnings}
	for _, sec := range changed {
		changedSet[sec] = true
		if s.reloadableSet[sec] {
			res.Applied = append(res.Applied, sec)
		} else {
			res.RestartRequired = append(res.RestartRequired, sec)
		}
	}

	// (3) Dry run stops here — report the diff, apply nothing.
	if dryRun {
		return res, nil
	}

	// (4) Run each reloader whose owned section changed (each rebuilds its subsystem
	// in place). On a reloader error NOTHING further is swapped and the holder is
	// NOT advanced, so s.cfg() keeps the old config — a consistent rollback (the
	// candidate is already validated, so runtime reloader failures are rare).
	for _, r := range s.reloaders {
		if intersectsChanged(r.Sections, changedSet) {
			if err := r.Reload(candidate); err != nil {
				return ReloadResult{}, fmt.Errorf("%w: %v", config.ErrReloadApply, err)
			}
		}
	}
	// (5) Swap the holder so every live-read section (user_tiers, agents, defaults,
	// channels defs) reflects the candidate on the next read, and the baseline
	// advances for the next diff. Restart-required sections are reflected here for
	// display/intent, but their boot-built subsystems run the old values until a
	// restart — exactly what restart_required says.
	s.cfgHolder.Store(candidate)
	return res, nil
}

// ReloadConcurrency applies a changed `concurrency` (or `providers.max_concurrent`)
// section in place (RFC CK): the global semaphore's concurrency + queue-depth caps
// (SetCaps), the per-user cap (WithPerUserCap), and the per-provider gates (rebuilt
// from providers[*].max_concurrent). The queue TIMEOUTs stay boot-fixed (they are
// read unlocked on the wait path). In-flight runs keep their slot; the new caps
// apply to subsequent admissions. Nil-safe for a bare Server (tests).
func (s *Server) ReloadConcurrency(newCfg *config.Config) {
	if s.sem != nil {
		s.sem.SetCaps(newCfg.Concurrency.MaxConcurrentRuns, newCfg.Concurrency.MaxQueueDepth)
		s.sem.WithPerUserCap(newCfg.Concurrency.MaxConcurrentRunsPerUser)
	}
	caps := map[string]int{}
	for id, pc := range newCfg.Providers {
		if pc.MaxConcurrent > 0 {
			caps[id] = pc.MaxConcurrent
		}
	}
	s.providerGates.Reload(caps, newCfg.Concurrency.ProviderQueueDepthOrDefault())
}

// intersectsChanged reports whether any of a reloader's owned sections changed.
func intersectsChanged(owned []string, changed map[string]bool) bool {
	for _, sec := range owned {
		if changed[sec] {
			return true
		}
	}
	return false
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
