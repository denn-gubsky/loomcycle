package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/capabilities"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// retentionBootCfg is a config whose retention sweeper is on, as it was when
// the sweepers were started.
func retentionBootCfg() *config.Config {
	c := baselineCfg()
	c.Env.RetentionEnabled = true
	c.Env.RetentionInterval = 24 * time.Hour
	c.Env.RetentionChatsMode = "prune"
	c.Env.RetentionChatsMaxAge = 90 * 24 * time.Hour
	c.Env.RetentionDefsMode = "off"
	c.Env.RetentionDefsKeepLastN = 5
	c.Env.UsageDetailRetention = 60 * 24 * time.Hour
	return c
}

func getRetentionReport(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleRetentionReport(rec, httptest.NewRequest(http.MethodGet, "/v1/_retention", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/_retention = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The retention sweeper keeps the configuration it was started with: a reload
// swaps the config the server reads and starts no sweeper. The report says
// what is being deleted, so after a reload it must still describe the running
// sweeper, and say that the yaml now asks for something else.
func TestRetentionReport_DescribesTheRunningSweeperAfterAReload(t *testing.T) {
	boot := retentionBootCfg()
	s := &Server{cfgHolder: config.NewHolder(boot)}
	s.SetRetentionRunning(RetentionRunningFrom(boot.Env))

	before := getRetentionReport(t, s)
	if before["enabled"] != true || before["chats_mode"] != "prune" {
		t.Fatalf("report before the reload = %v, want the boot retention", before)
	}
	if _, has := before["pending_restart"]; has {
		t.Errorf("pending_restart is reported with nothing pending: %v", before["pending_restart"])
	}

	// The yaml now turns retention off and changes a family.
	edited := retentionBootCfg()
	edited.Retention.Enabled = false
	edited.Env.RetentionEnabled = false
	edited.Env.RetentionChatsMode = "off"
	edited.Env.RetentionChatsMaxAge = 0
	edited.Env.RetentionInterval = time.Hour
	s.SetConfigReloader(func() (*config.Config, error) { return edited, nil })
	if _, err := s.ReloadConfig(context.Background(), false); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if s.cfg().Env.RetentionEnabled {
		t.Fatal("the reload did not swap the config")
	}

	after := getRetentionReport(t, s)
	if after["enabled"] != true || after["chats_mode"] != "prune" ||
		after["chats_max_age_ms"] != float64((90*24*time.Hour).Milliseconds()) ||
		after["interval_ms"] != float64((24*time.Hour).Milliseconds()) {
		t.Errorf("report after the reload = %v, want the retention the sweeper still runs with", after)
	}
	if after["pending_restart"] != true {
		t.Errorf("pending_restart = %v, want true: the yaml differs from the running sweeper", after["pending_restart"])
	}

	// A reload back to what is running leaves nothing pending.
	s.SetConfigReloader(func() (*config.Config, error) { return retentionBootCfg(), nil })
	if _, err := s.ReloadConfig(context.Background(), false); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if again := getRetentionReport(t, s); again["pending_restart"] != nil {
		t.Errorf("pending_restart = %v after reloading the running retention, want it absent", again["pending_restart"])
	}
}

// A server that was never told what the sweepers run with (a test fixture, a
// command that starts none) reports the config, as before.
func TestRetentionReport_FallsBackToTheConfigWithNoRunningSweeper(t *testing.T) {
	s := &Server{cfgHolder: config.NewHolder(retentionBootCfg())}
	got := getRetentionReport(t, s)
	if got["enabled"] != true || got["chats_mode"] != "prune" || got["pending_restart"] != nil {
		t.Errorf("report = %v, want the config's retention and nothing pending", got)
	}
}

// The usage rollup is started with its detail age too, so the boundary the
// usage report gives ("per-call rows exist from here") follows the running
// sweeper, not an edited yaml.
func TestUsageDetailSince_FollowsTheRunningSweeperAfterAReload(t *testing.T) {
	boot := retentionBootCfg() // 60 days
	s := &Server{cfgHolder: config.NewHolder(boot)}
	s.SetRetentionRunning(RetentionRunningFrom(boot.Env))

	edited := retentionBootCfg()
	edited.Env.UsageDetailRetention = 24 * time.Hour
	s.SetConfigReloader(func() (*config.Config, error) { return edited, nil })
	if _, err := s.ReloadConfig(context.Background(), false); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if age := time.Since(s.UsageDetailSince()); age < 59*24*time.Hour {
		t.Errorf("detail boundary is %v back, want the running sweeper's 60 days", age.Round(time.Hour))
	}
	if getRetentionReport(t, s)["pending_restart"] != true {
		t.Error("a changed usage detail age is not reported as pending a restart")
	}
}

// The capability map answers "is retention running", which is the sweeper's
// state, not the yaml's.
func TestConfigView_RetentionAvailabilityFollowsTheRunningSweeper(t *testing.T) {
	boot := retentionBootCfg()
	s := &Server{cfgHolder: config.NewHolder(boot)}
	s.SetRetentionRunning(RetentionRunningFrom(boot.Env))
	edited := retentionBootCfg()
	edited.Env.RetentionEnabled = false
	s.cfgHolder.Store(edited)

	feat := capabilities.Deployment(s.capabilityInputs(true))
	if got := feat["retention"].(map[string]any)["available"]; got != true {
		t.Errorf("retention.available = %v after a reload that turned it off in the yaml, want true: the sweeper still runs", got)
	}
}
