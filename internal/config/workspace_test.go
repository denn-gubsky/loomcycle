package config

import (
	"strings"
	"testing"
)

// Absent, the runtime serves no workspaces, at the documented defaults.
func TestWorkspaceConfig_AbsentIsOffAtTheDefaults(t *testing.T) {
	cfg, err := loadConversion(t, "")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Workspace.Resolved()
	want := WorkspaceConfig{Mode: WorkspaceModeOff, MaxPerRun: 4, Exec: WorkspaceExecConfig{DefaultTimeoutS: 120, MaxTimeoutS: 900}, LostAfterS: 300}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
	if got.RemoteWorkers() {
		t.Fatal("an absent block attaches workers")
	}
}

func TestWorkspaceConfig_RemoteKeepsWhatIsWritten(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 127.0.0.1:7788\n"+
		"workspace:\n  mode: remote\n  max_per_run: 2\n  max_per_tenant: 10\n  exec:\n    default_timeout_s: 30\n    max_timeout_s: 60\n  lost_after_s: 90\n")
	if err != nil {
		t.Fatal(err)
	}
	want := WorkspaceConfig{Mode: WorkspaceModeRemote, MaxPerRun: 2, MaxPerTenant: 10, Exec: WorkspaceExecConfig{DefaultTimeoutS: 30, MaxTimeoutS: 60}, LostAfterS: 90}
	if got := cfg.Workspace.Resolved(); got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}

// workspace.mode remote serves the endpoint, so the warning that nothing
// listens on it would be false and is not given.
func TestWorkspaceConfig_RemoteModeUsesTheEndpointWithoutTheUnusedWarning(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 127.0.0.1:7788\nworkspace:\n  mode: remote\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "worker_endpoint is set") {
			t.Fatalf("warned the endpoint is unused: %q", w)
		}
	}
}

func TestWorkspaceConfig_RefusesWhatItCannotServe(t *testing.T) {
	for _, c := range []struct{ block, want string }{
		{"workspace:\n  mode: local\n", "local is not available in this release"},
		{"workspace:\n  mode: cloud\n", `unknown mode "cloud"`},
		{"conversion:\n  mode: inprocess\nworkspace:\n  mode: remote\n", "workspace mode remote needs the host:port"},
		{"workspace:\n  mode: off\n  max_per_run: -1\n", "may not be negative"},
		{"workspace:\n  exec:\n    default_timeout_s: 1000\n", "is over max_timeout_s"},
		{"workspace:\n  mode: off\n  share: true\n", `unknown key "share"`},
		{"workspace:\n  exec:\n    timeout: 5\n", `unknown key "timeout"`},
		// The endpoint is checked as for conversion.
		{"conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 0.0.0.0:7788\nworkspace:\n  mode: remote\n", "set worker_endpoint.tls"},
	} {
		if _, err := loadConversion(t, c.block); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one saying %q", c.block, err, c.want)
		}
	}
}

// A grant on an agent holding the tool still reaches nothing while the
// runtime serves no workspaces: advised at boot, never refused.
func TestWorkspaceWarnings_AdviseAGrantWhileTheModeIsOff(t *testing.T) {
	def := AgentDef{Tools: []string{"Workspace"}, Workspace: &AgentWorkspace{Ops: []string{"exec"}, Open: true}}
	if got := strings.Join(workspaceWarnings("a", def, WorkspaceModeOff), "\n"); !strings.Contains(got, "workspace.mode is off") {
		t.Errorf("mode off: advisory = %q, want one naming workspace.mode", got)
	}
	if got := workspaceWarnings("a", def, WorkspaceModeRemote); len(got) != 0 {
		t.Errorf("mode remote: advisory = %q, want none", got)
	}
}
