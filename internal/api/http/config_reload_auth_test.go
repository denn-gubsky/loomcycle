package http

import (
	"context"
	"errors"
	"strings"
	"testing"

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
