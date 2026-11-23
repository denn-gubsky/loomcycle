package config

import (
	"path/filepath"
	"reflect"
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
	want := WorkspaceConfig{Mode: WorkspaceModeOff, MaxPerRun: 4, Exec: WorkspaceExecConfig{DefaultTimeoutS: 120, MaxTimeoutS: 900}, LostAfterS: 300,
		DiskQuotaMB: 2048, Copy: WorkspaceCopyConfig{MaxFiles: 2000, MaxBytes: 1 << 30},
		Checkout: WorkspaceCheckoutConfig{MaxDocuments: 500, MaxBytes: 256 << 20}, Convert: WorkspaceConvertConfig{MemoryLimitMB: 1024},
		Kernel: WorkspaceKernelConfig{InterruptGraceSeconds: 5}, Checkpoint: WorkspaceCheckpointConfig{MaxPerWorkspace: 8},
		Egress: WorkspaceEgressConfig{Limits: WorkspaceEgressLimits{RatePerMinute: 600, MaxBytes: 4 << 30}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
	if got.RemoteWorkers() || got.LocalWorker() || got.Serves() {
		t.Fatal("an absent block attaches workers")
	}
}

func TestWorkspaceConfig_RemoteKeepsWhatIsWritten(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 127.0.0.1:7788\n"+
		"workspace:\n  mode: remote\n  max_per_run: 2\n  max_per_tenant: 10\n  exec:\n    default_timeout_s: 30\n    max_timeout_s: 60\n  lost_after_s: 90\n"+
		"  disk_quota_mb: 16777216\n  copy:\n    max_files: 50\n    max_bytes: 4096\n  checkout:\n    max_documents: 20\n    max_bytes: 9000\n  convert:\n    memory_limit_mb: 512\n"+
		"  kernel:\n    interrupt_grace_seconds: 12\n  checkpoint:\n    max_per_workspace: 3\n")
	if err != nil {
		t.Fatal(err)
	}
	want := WorkspaceConfig{Mode: WorkspaceModeRemote, MaxPerRun: 2, MaxPerTenant: 10, Exec: WorkspaceExecConfig{DefaultTimeoutS: 30, MaxTimeoutS: 60}, LostAfterS: 90,
		DiskQuotaMB: 16 << 20, Copy: WorkspaceCopyConfig{MaxFiles: 50, MaxBytes: 4096},
		Checkout: WorkspaceCheckoutConfig{MaxDocuments: 20, MaxBytes: 9000}, Convert: WorkspaceConvertConfig{MemoryLimitMB: 512},
		Kernel: WorkspaceKernelConfig{InterruptGraceSeconds: 12}, Checkpoint: WorkspaceCheckpointConfig{MaxPerWorkspace: 3},
		Egress: WorkspaceEgressConfig{Limits: WorkspaceEgressLimits{RatePerMinute: 600, MaxBytes: 4 << 30}}}
	if got := cfg.Workspace.Resolved(); !reflect.DeepEqual(got, want) {
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
		{"workspace:\n  mode: remote+local\n", `unknown mode "remote+local"`},
		{"workspace:\n  mode: remote\n  local:\n    tenant: acme\n", "starts no local worker"},
		{"workspace:\n  mode: local\n  local:\n    tenants: [acme]\n", `unknown key "tenants"`},
		{"conversion:\n  mode: inprocess\nworkspace:\n  mode: local+remote\n", "workspace mode local+remote needs the host:port"},
		{"workspace:\n  mode: cloud\n", `unknown mode "cloud"`},
		{"conversion:\n  mode: inprocess\nworkspace:\n  mode: remote\n", "workspace mode remote needs the host:port"},
		{"workspace:\n  mode: off\n  max_per_run: -1\n", "may not be negative"},
		{"workspace:\n  exec:\n    default_timeout_s: 1000\n", "is over max_timeout_s"},
		{"workspace:\n  mode: off\n  share: true\n", `unknown key "share"`},
		{"workspace:\n  exec:\n    timeout: 5\n", `unknown key "timeout"`},
		{"workspace:\n  disk_quota_mb: 16777217\n", "workspace.disk_quota_mb: 16777217 is out of range"},
		{"workspace:\n  disk_quota_mb: -1\n", "workspace.disk_quota_mb: -1 is out of range"},
		{"workspace:\n  copy:\n    max_files: -1\n", "workspace.copy.max_files: -1 is out of range"},
		{"workspace:\n  copy:\n    max_bytes: 17592186044417\n", "workspace.copy.max_bytes: 17592186044417 is out of range"},
		{"workspace:\n  convert:\n    memory_limit_mb: 32\n", "workspace.convert.memory_limit_mb: 32 is out of range"},
		{"workspace:\n  checkout:\n    max_documents: 100001\n", "workspace.checkout.max_documents: 100001 is out of range"},
		{"workspace:\n  checkout:\n    max_bytes: -1\n", "workspace.checkout.max_bytes: -1 is out of range"},
		{"workspace:\n  kernel:\n    interrupt_grace_seconds: 301\n", "workspace.kernel.interrupt_grace_seconds: 301 is out of range"},
		{"workspace:\n  kernel:\n    grace: 5\n", `unknown key "grace"`},
		{"workspace:\n  checkpoint:\n    max_per_workspace: 101\n", "workspace.checkpoint.max_per_workspace: 101 is out of range"},
		{"workspace:\n  checkpoint:\n    mechanism: overlay\n", `unknown key "mechanism"`},
		{"workspace:\n  convert:\n    memory_mb: 512\n", "memory_mb"},
		{"workspace:\n  copy:\n    files: 5\n", `unknown key "files"`},
		// The endpoint is checked as for conversion.
		{"conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 0.0.0.0:7788\nworkspace:\n  mode: remote\n", "set worker_endpoint.tls"},
	} {
		if _, err := loadConversion(t, c.block); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one saying %q", c.block, err, c.want)
		}
	}
}

// Mode local needs no worker endpoint: the runtime starts its worker on a
// private socket, keeping its workspaces in a directory of the data dir
// unless the block says otherwise.
func TestWorkspaceConfig_LocalNeedsNoEndpointAndHasDefaults(t *testing.T) {
	cfg, err := loadConversion(t, "conversion:\n  mode: inprocess\nworkspace:\n  mode: local\n")
	if err != nil {
		t.Fatal(err)
	}
	ws := cfg.Workspace.Resolved()
	if !ws.LocalWorker() || ws.RemoteWorkers() || !ws.Serves() {
		t.Fatalf("mode local: local=%v remote=%v serves=%v", ws.LocalWorker(), ws.RemoteWorkers(), ws.Serves())
	}
	if want := filepath.Join(cfg.Env.DataDir, "workspaces"); cfg.WorkspaceLocalRoot() != want {
		t.Fatalf("root = %q, want %q", cfg.WorkspaceLocalRoot(), want)
	}

	cfg, err = loadConversion(t, "conversion:\n  mode: inprocess\nworker_endpoint:\n  listen: 127.0.0.1:7788\n"+
		"workspace:\n  mode: local+remote\n  local:\n    tenant: acme\n    root: /srv/ws\n")
	if err != nil {
		t.Fatal(err)
	}
	ws = cfg.Workspace.Resolved()
	if !ws.LocalWorker() || !ws.RemoteWorkers() || ws.Local.Tenant != "acme" || cfg.WorkspaceLocalRoot() != "/srv/ws" {
		t.Fatalf("local+remote: %+v root %q", ws, cfg.WorkspaceLocalRoot())
	}
}

// Unset, the local worker serves the tenant a run of this install carries
// when its request names none: "" in open mode, "default" under the legacy
// secret, the principals' one tenant; written, what is written.
func TestWorkspaceLocalTenant_IsTheTenantARunWithNoneNamedCarries(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"open mode", Config{}, ""},
		{"legacy secret", Config{Env: Env{AuthToken: "s"}}, "default"},
		{"one principal tenant", Config{Principals: map[string]PrincipalDef{"a": {Tenant: "acme"}, "b": {Tenant: "acme"}}}, "acme"},
		{"principals of two tenants", Config{Principals: map[string]PrincipalDef{"a": {Tenant: "acme"}, "b": {Tenant: "zed"}}}, "default"},
		{"written", Config{Env: Env{AuthToken: "s"}, Workspace: WorkspaceConfig{Local: WorkspaceLocalConfig{Tenant: "lab"}}}, "lab"},
	} {
		if got, why := c.cfg.WorkspaceLocalTenant(); got != c.want || why == "" {
			t.Errorf("%s: tenant = %q (%q), want %q", c.name, got, why, c.want)
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

// The agents whose grant allows nothing are named, sorted: one without the
// tool whatever the mode, every granted one while the mode is off.
func TestInertWorkspaceGrants_NamesTheAgentsWhoseGrantAllowsNothing(t *testing.T) {
	grant := &AgentWorkspace{Ops: []string{"exec"}, Open: true}
	cfg := &Config{Agents: map[string]AgentDef{
		"zed":     {Tools: []string{"Read"}, Workspace: grant},
		"builder": {Tools: []string{"Workspace"}, Workspace: grant},
		"plain":   {Tools: []string{"Workspace"}},
	}}
	if got := strings.Join(cfg.InertWorkspaceGrants(), ","); got != "builder,zed" {
		t.Errorf("mode off: inert = %q, want builder,zed", got)
	}
	cfg.Workspace.Mode = WorkspaceModeLocal
	if got := strings.Join(cfg.InertWorkspaceGrants(), ","); got != "zed" {
		t.Errorf("mode local: inert = %q, want zed", got)
	}
}
