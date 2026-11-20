package config

import (
	"errors"
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Workspace modes (RFC GW §8).
const (
	// WorkspaceModeOff serves no workspaces: no worker attaches, and an
	// agent's workspace grant reaches nothing. The default.
	WorkspaceModeOff = "off"
	// WorkspaceModeRemote listens on the worker endpoint for workspace
	// workers that run as their own processes.
	WorkspaceModeRemote = "remote"
	// WorkspaceModeLocal is a workspace worker started as a child of the
	// runtime. It arrives with RFC GW P1's last step and is refused until
	// then.
	WorkspaceModeLocal = "local"
)

// WorkspaceConfig is the `workspace:` block: whether the runtime serves
// workspaces on workers (RFC GW), and its bounds. Policy and tuning, so yaml
// only (RFC GN). Resolved fills its defaults.
type WorkspaceConfig struct {
	// Mode is off (default) or remote.
	Mode string `yaml:"mode,omitempty"`
	// MaxPerRun bounds the workspaces one run holds at once. Default 4.
	MaxPerRun int `yaml:"max_per_run,omitempty"`
	// MaxPerTenant bounds a tenant's workspaces across its runs; 0 is
	// unlimited.
	MaxPerTenant int `yaml:"max_per_tenant,omitempty"`
	// Exec bounds a command's run time.
	Exec WorkspaceExecConfig `yaml:"exec,omitempty"`
	// LostAfterS is how long a worker may be away before its workspaces are
	// marked lost. Default 300.
	LostAfterS int `yaml:"lost_after_s,omitempty"`
}

// WorkspaceExecConfig is `workspace.exec`.
type WorkspaceExecConfig struct {
	// DefaultTimeoutS bounds a command that names no timeout. Default 120.
	DefaultTimeoutS int `yaml:"default_timeout_s,omitempty"`
	// MaxTimeoutS is the longest timeout a command may ask for. Default 900.
	MaxTimeoutS int `yaml:"max_timeout_s,omitempty"`
}

// Defaults of the workspace block.
const (
	DefaultWorkspaceMaxPerRun      = 4
	DefaultWorkspaceExecTimeoutS   = 120
	DefaultWorkspaceExecMaxTimeout = 900
	DefaultWorkspaceLostAfterS     = 300
)

// Resolved is the block with its defaults filled in.
func (w WorkspaceConfig) Resolved() WorkspaceConfig {
	if w.Mode == "" {
		w.Mode = WorkspaceModeOff
	}
	if w.MaxPerRun == 0 {
		w.MaxPerRun = DefaultWorkspaceMaxPerRun
	}
	if w.Exec.DefaultTimeoutS == 0 {
		w.Exec.DefaultTimeoutS = DefaultWorkspaceExecTimeoutS
	}
	if w.Exec.MaxTimeoutS == 0 {
		w.Exec.MaxTimeoutS = DefaultWorkspaceExecMaxTimeout
	}
	if w.LostAfterS == 0 {
		w.LostAfterS = DefaultWorkspaceLostAfterS
	}
	return w
}

// RemoteWorkers reports whether the mode listens for workspace workers.
func (w WorkspaceConfig) RemoteWorkers() bool { return w.Mode == WorkspaceModeRemote }

// validateWorkspace refuses a mode it does not know (and local, not built
// yet), a remote mode with no worker endpoint, and values out of range. The
// endpoint's own values are validateConversion's, which runs first. Part of
// validate.
func validateWorkspace(c *Config) error {
	w := c.Workspace
	switch w.Mode {
	case "", WorkspaceModeOff, WorkspaceModeRemote:
	case WorkspaceModeLocal:
		return errors.New("workspace.mode: local is not available in this release; run a workspace worker of its own (`loomcycle worker --role workspace`) and set mode: remote")
	default:
		return fmt.Errorf("workspace.mode: unknown mode %q (want %s or %s)", w.Mode, WorkspaceModeOff, WorkspaceModeRemote)
	}
	if w.RemoteWorkers() && c.ResolvedWorkerEndpoint().Listen == "" {
		return fmt.Errorf("worker_endpoint.listen: workspace mode %s needs the host:port workers attach to", w.Mode)
	}
	if w.MaxPerRun < 0 || w.MaxPerTenant < 0 || w.LostAfterS < 0 || w.Exec.DefaultTimeoutS < 0 || w.Exec.MaxTimeoutS < 0 {
		return errors.New("workspace: max_per_run, max_per_tenant, lost_after_s and the exec timeouts may not be negative")
	}
	r := w.Resolved()
	if r.Exec.DefaultTimeoutS > r.Exec.MaxTimeoutS {
		return fmt.Errorf("workspace.exec.default_timeout_s (%d) is over max_timeout_s (%d)", r.Exec.DefaultTimeoutS, r.Exec.MaxTimeoutS)
	}
	return nil
}

func (w *WorkspaceConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "workspace", nil, reflect.TypeOf(*w)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "exec"), "workspace.exec", nil, reflect.TypeOf(WorkspaceExecConfig{})); err != nil {
		return err
	}
	type plain WorkspaceConfig
	return n.Decode((*plain)(w))
}
