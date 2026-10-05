package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A team's own channels are stored under "_team/". A yaml channel under that
// prefix would be a declaration from outside the team of a name inside it,
// readable and writable by any agent granted it.
func TestValidation_RefusesReservedTeamChannelPrefix(t *testing.T) {
	for _, name := range []string{"_team/sdlc/events", "_team/x"} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(`
defaults: { provider: anthropic, model: x }
channels:
  `+name+`:
    scope: user
`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), "reserved for a team's own channels") {
			t.Errorf("channel %q: want the reserved-prefix refusal, got %v", name, err)
		}
	}
	// The bare word is not the prefix.
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(`
defaults: { provider: anthropic, model: x }
channels:
  _team:
    scope: user
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("a channel named \"_team\" is outside the prefix and must load: %v", err)
	}
}
