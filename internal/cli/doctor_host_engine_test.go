package cli

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A workspace command on the local worker's host engine runs on this host as
// the loomcycle user and reaches the instance metadata service as Bash did
// (RFC GW P15 Q5): the modes that start the local worker are what doctor's
// AWS warning keys on, and no other.
func TestDoctor_HostEngineWithAWSAuthWarnsOfTheMetadataService(t *testing.T) {
	for mode, want := range map[string]bool{
		"":                              false,
		config.WorkspaceModeOff:         false,
		config.WorkspaceModeRemote:      false,
		config.WorkspaceModeLocal:       true,
		config.WorkspaceModeLocalRemote: true,
	} {
		r := &realConfig{cfg: &config.Config{Workspace: config.WorkspaceConfig{Mode: mode}}}
		if got := r.HostEngineCommands(); got != want {
			t.Errorf("workspace.mode %q: HostEngineCommands = %v, want %v", mode, got, want)
		}
	}
}
