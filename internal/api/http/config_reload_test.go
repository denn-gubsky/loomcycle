package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func baselineCfg() *config.Config {
	return &config.Config{
		Providers:        map[string]config.ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://a:11434"}},
		ProviderPriority: []string{"ollama-local"},
	}
}

// providersChangedCfg is baselineCfg with the ollama-local base_url edited — a
// change in the reloadable `providers` section.
func providersChangedCfg() *config.Config {
	c := baselineCfg()
	c.Providers = map[string]config.ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://b:11434"}}
	return c
}

func TestReloadConfig_Unwired(t *testing.T) {
	s := &Server{}
	if _, err := s.ReloadConfig(context.Background(), false); !errors.Is(err, errReloadUnavailable) {
		t.Fatalf("err = %v, want errReloadUnavailable", err)
	}
}

// TestReloadConfig_DryRunReportsButDoesNotApply: dry-run classifies the diff and
// applies nothing (apply hook untouched, baseline unchanged).
func TestReloadConfig_DryRunReportsButDoesNotApply(t *testing.T) {
	s := &Server{cfg: baselineCfg()}
	applied := 0
	s.SetConfigReloader(
		func() (*config.Config, error) { return providersChangedCfg(), nil },
		func(*config.Config) error { applied++; return nil },
	)
	res, err := s.ReloadConfig(context.Background(), true)
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if !res.DryRun || len(res.Applied) != 1 || res.Applied[0] != "providers" {
		t.Errorf("dry-run result = %+v, want DryRun + Applied=[providers]", res)
	}
	if applied != 0 {
		t.Errorf("apply called %d times on dry-run, want 0", applied)
	}
}

// TestReloadConfig_AppliesReloadableSection: a changed reloadable section calls
// the apply hook once and advances the baseline (a second reload sees no change).
func TestReloadConfig_AppliesReloadableSection(t *testing.T) {
	s := &Server{cfg: baselineCfg()}
	applied := 0
	loaded := providersChangedCfg()
	s.SetConfigReloader(
		func() (*config.Config, error) { return loaded, nil },
		func(*config.Config) error { applied++; return nil },
	)
	res, err := s.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0] != "providers" || applied != 1 {
		t.Errorf("result=%+v applied=%d, want Applied=[providers] applied=1", res, applied)
	}
	// Baseline advanced: reloading the same config again is a no-op (no apply).
	res2, err := s.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("second ReloadConfig: %v", err)
	}
	if len(res2.Applied) != 0 || applied != 1 {
		t.Errorf("second reload result=%+v applied=%d, want no change (baseline advanced)", res2, applied)
	}
}

// TestReloadConfig_InvalidRejectedNotApplied: a candidate that fails to load is
// rejected whole (ErrReloadInvalid) and the apply hook is never called.
func TestReloadConfig_InvalidRejectedNotApplied(t *testing.T) {
	s := &Server{cfg: baselineCfg()}
	applied := 0
	s.SetConfigReloader(
		func() (*config.Config, error) { return nil, errors.New("bad yaml at line 7") },
		func(*config.Config) error { applied++; return nil },
	)
	_, err := s.ReloadConfig(context.Background(), false)
	if !errors.Is(err, config.ErrReloadInvalid) {
		t.Fatalf("err = %v, want ErrReloadInvalid", err)
	}
	if applied != 0 {
		t.Errorf("apply called %d times for an invalid candidate, want 0", applied)
	}
}

// TestReloadConfig_NonReloadableSectionRestartRequired: a changed section outside
// the reloadable set is reported under restart_required and the apply hook is
// NOT called (nothing reloadable changed).
func TestReloadConfig_NonReloadableSectionRestartRequired(t *testing.T) {
	s := &Server{cfg: baselineCfg()}
	applied := 0
	changed := baselineCfg()
	changed.Defaults = config.Defaults{Provider: "anthropic", Model: "claude"} // a non-reloadable section
	s.SetConfigReloader(
		func() (*config.Config, error) { return changed, nil },
		func(*config.Config) error { applied++; return nil },
	)
	res, err := s.ReloadConfig(context.Background(), false)
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Errorf("Applied = %v, want none (defaults is not reloadable)", res.Applied)
	}
	if len(res.RestartRequired) != 1 || res.RestartRequired[0] != "defaults" {
		t.Errorf("RestartRequired = %v, want [defaults]", res.RestartRequired)
	}
	if applied != 0 {
		t.Errorf("apply called %d times when only a restart-required section changed, want 0", applied)
	}
}

// TestHandleConfigReload_EndToEnd exercises the real POST /v1/_config/reload
// route: auth (open mode), ?dry_run parsing, the handler → ReloadConfig →
// JSON-encode path, and the 422 rejection of an invalid candidate.
func TestHandleConfigReload_EndToEnd(t *testing.T) {
	cfg := &config.Config{
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode
	cfg.Providers = map[string]config.ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://a:11434"}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "reload.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{}, []tools.Tool{}, sem, st)

	valid := true
	applied := 0
	srv.SetConfigReloader(
		func() (*config.Config, error) {
			if !valid {
				return nil, errors.New("providers[0]: unknown driver \"bogus\"")
			}
			c := &config.Config{Providers: map[string]config.ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: "http://b:11434"}}}
			return c, nil
		},
		func(*config.Config) error { applied++; return nil },
	)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	// (1) dry-run: reports providers changed, applies nothing.
	body := doReload(t, ts.URL+"/v1/_config/reload?dry_run=1", http.StatusOK)
	if !body.DryRun || len(body.Applied) != 1 || body.Applied[0] != "providers" || applied != 0 {
		t.Errorf("dry-run body=%+v applied=%d, want DryRun Applied=[providers] applied=0", body, applied)
	}

	// (2) real apply: providers applied, hook called.
	body = doReload(t, ts.URL+"/v1/_config/reload", http.StatusOK)
	if body.DryRun || len(body.Applied) != 1 || applied != 1 {
		t.Errorf("apply body=%+v applied=%d, want Applied=[providers] applied=1", body, applied)
	}

	// (3) invalid candidate → 422, hook not called again.
	valid = false
	doReloadStatus(t, ts.URL+"/v1/_config/reload", http.StatusUnprocessableEntity)
	if applied != 1 {
		t.Errorf("apply called %d times total, want 1 (invalid candidate must not apply)", applied)
	}
}

func doReload(t *testing.T, url string, wantStatus int) ReloadResult {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, wantStatus, b)
	}
	var out ReloadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func doReloadStatus(t *testing.T, url string, wantStatus int) {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
}
