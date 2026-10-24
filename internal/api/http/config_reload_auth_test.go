package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A reload that drops every principal must not move a running server into open
// mode: the boot refused open mode on a public address, and nothing re-checks.
func TestReloadConfig_RefusesDroppingPrincipalsIntoOpenMode(t *testing.T) {
	s := declaredOnlyServer(t)
	candidate := &config.Config{}
	candidate.Env = s.cfg().Env
	s.SetConfigReloader(func() (*config.Config, error) { return candidate, nil })
	for _, dry := range []bool{true, false} {
		_, err := s.ReloadConfig(context.Background(), dry)
		if !errors.Is(err, config.ErrReloadInvalid) || !strings.Contains(err.Error(), "open mode") {
			t.Fatalf("reload (dry=%v) dropping every principal: err = %v, want ErrReloadInvalid naming open mode", dry, err)
		}
	}
	if on, _ := s.AuthConfiguredErr(context.Background()); !on {
		t.Error("the refused reload still turned auth off")
	}
}

// Dropping principals is fine when something else keeps auth on.
func TestReloadConfig_DroppingPrincipalsAllowedWithLegacySecret(t *testing.T) {
	s := declaredOnlyServer(t)
	candidate := &config.Config{}
	candidate.Env = s.cfg().Env
	candidate.Env.AuthToken = "legacy"
	s.SetConfigReloader(func() (*config.Config, error) { return candidate, nil })
	if _, err := s.ReloadConfig(context.Background(), false); err != nil {
		t.Fatalf("reload keeping a legacy secret: %v", err)
	}
}

// A principal a reload removes stops authenticating at once, not when its
// cached resolution expires.
func TestReloadConfig_RemovedPrincipalStopsAuthenticatingAtOnce(t *testing.T) {
	s := declaredOnlyServer(t)
	s.EnableTokenCache(time.Hour)
	if _, ok := s.resolvePrincipal(context.Background(), declaredAliceSecret); !ok {
		t.Fatal("alice should resolve before the reload")
	}
	candidate := &config.Config{Principals: map[string]config.PrincipalDef{
		"bob": {Tenant: "acme", Subject: "bob", Scopes: []string{"runs:read"}, TokenEnv: "LOOMCYCLE_BOB_TOKEN"},
	}}
	candidate.Env = s.cfg().Env
	s.SetConfigReloader(func() (*config.Config, error) { return candidate, nil })
	if _, err := s.ReloadConfig(context.Background(), false); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := s.resolvePrincipal(context.Background(), declaredAliceSecret); ok {
		t.Error("a principal removed by a reload still authenticated from the token cache")
	}
}

// RFC FA §8 downgrade: a reload cannot lower auth.mode (strict → token); the
// refusal is audited and the running mode is kept. Staying strict, or raising
// token → strict, reloads.
func TestReloadConfig_RefusesLoweringTheAuthMode(t *testing.T) {
	s, _, sink := sessionTestServer(t, "legacy-secret")
	s.cfg().ResolvedAuth = strictPolicy()
	lowered := &config.Config{}
	lowered.Env = s.cfg().Env
	s.SetConfigReloader(func() (*config.Config, error) { return lowered, nil })
	for _, dry := range []bool{true, false} {
		_, err := s.ReloadConfig(context.Background(), dry)
		if !errors.Is(err, config.ErrReloadInvalid) || !strings.Contains(err.Error(), "lowers auth.mode") {
			t.Fatalf("reload (dry=%v) strict→token: err = %v, want ErrReloadInvalid naming the downgrade", dry, err)
		}
	}
	if !s.cfg().ResolvedAuth.Strict() {
		t.Fatal("the refused reload lowered the running mode")
	}
	if ev := lastEvent(t, sink, "auth_downgrade_refused"); !strings.Contains(ev.AuthReason, "lowers auth.mode") {
		t.Errorf("audit = %+v", ev)
	}

	kept := &config.Config{ResolvedAuth: strictPolicy()}
	kept.Env = s.cfg().Env
	s.SetConfigReloader(func() (*config.Config, error) { return kept, nil })
	if _, err := s.ReloadConfig(context.Background(), false); err != nil {
		t.Fatalf("reload staying strict: %v", err)
	}

	tok, _, _ := sessionTestServer(t, "legacy-secret")
	raised := &config.Config{ResolvedAuth: strictPolicy()}
	raised.Env = tok.cfg().Env
	tok.SetConfigReloader(func() (*config.Config, error) { return raised, nil })
	if _, err := tok.ReloadConfig(context.Background(), false); err != nil || !tok.cfg().ResolvedAuth.Strict() {
		t.Fatalf("reload token→strict: err=%v strict=%v", err, tok.cfg().ResolvedAuth.Strict())
	}
}

// A reload that drops strict mode's last interactive gate is refused: the
// candidate does not load.
func TestReloadConfig_StrictWithoutGateDoesNotLoad(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy-secret")
	s.cfg().ResolvedAuth = strictPolicy()
	s.SetConfigReloader(func() (*config.Config, error) {
		return config.LoadLayers(config.Layer{Name: "auth.yaml", Data: []byte("auth:\n  mode: strict\n")})
	})
	if _, err := s.ReloadConfig(context.Background(), false); !errors.Is(err, config.ErrReloadInvalid) ||
		!strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("err = %v, want the candidate refused for having no interactive gate", err)
	}
	if !s.cfg().ResolvedAuth.Strict() || len(s.cfg().ResolvedAuth.Gates) != 1 {
		t.Error("the refused reload changed the running gates")
	}
}
