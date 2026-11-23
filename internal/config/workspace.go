package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"

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
	// WorkspaceModeLocal starts one workspace worker as a child of the
	// runtime, on a private Unix socket, serving workspace.local.tenant
	// (RFC GW D9).
	WorkspaceModeLocal = "local"
	// WorkspaceModeLocalRemote is both: the local child, and workers of
	// their own on the worker endpoint.
	WorkspaceModeLocalRemote = "local+remote"
	// LegacyTenant is the tenant every run carries under the legacy
	// LOOMCYCLE_AUTH_TOKEN, whatever its request names (the synthetic
	// principal the API resolves that secret to).
	LegacyTenant = "default"
)

// WorkspaceConfig is the `workspace:` block: whether the runtime serves
// workspaces on workers (RFC GW), and its bounds. Policy and tuning, so yaml
// only (RFC GN). Resolved fills its defaults.
type WorkspaceConfig struct {
	// Mode is off (default), local, remote or local+remote.
	Mode string `yaml:"mode,omitempty"`
	// Local configures the worker of mode local.
	Local WorkspaceLocalConfig `yaml:"local,omitempty"`
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
	// DiskQuotaMB bounds what one workspace may hold on its worker's disk,
	// in MiB (RFC GW P2). Default 2048; at most 16 TiB.
	DiskQuotaMB int64 `yaml:"disk_quota_mb,omitempty"`
	// Copy bounds a folder copy into or out of a workspace.
	Copy WorkspaceCopyConfig `yaml:"copy,omitempty"`
	// Checkout bounds File op=checkout of Documents into a workspace (RFC
	// GW P7).
	Checkout WorkspaceCheckoutConfig `yaml:"checkout,omitempty"`
	// Convert configures the conversion child of the local worker.
	Convert WorkspaceConvertConfig `yaml:"convert,omitempty"`
}

// WorkspaceConvertConfig is `workspace.convert`: the conversion child a
// workspace worker converts its workspaces' files in (RFC GW D34). It reaches
// the local worker only; a worker of its own reads
// LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB from its environment, as it
// loads no config.
type WorkspaceConvertConfig struct {
	// MemoryLimitMB is the child's memory limit in MiB. Default 1024.
	MemoryLimitMB int `yaml:"memory_limit_mb,omitempty"`
}

// WorkspaceCopyConfig is `workspace.copy`: the most a folder copy may move.
type WorkspaceCopyConfig struct {
	// MaxFiles is the files a folder copy may hold. Default 2000.
	MaxFiles int `yaml:"max_files,omitempty"`
	// MaxBytes is the bytes a folder copy may hold. Default 1 GiB.
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
}

// WorkspaceCheckoutConfig is `workspace.checkout`: the most one checkout of
// Documents into a workspace writes. What passes either bound is left out
// and listed, never cut part way through a document.
type WorkspaceCheckoutConfig struct {
	// MaxDocuments is the documents one checkout writes. Default 500.
	MaxDocuments int `yaml:"max_documents,omitempty"`
	// MaxBytes is the bytes one checkout writes. Default 256 MiB.
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
}

// WorkspaceLocalConfig is `workspace.local`: the one tenant the local worker
// serves, and where it keeps its workspaces.
type WorkspaceLocalConfig struct {
	// Tenant is the tenant whose runs may open workspaces on the local
	// worker. Unset, Config.WorkspaceLocalTenant works it out.
	Tenant string `yaml:"tenant,omitempty"`
	// Root is the directory the local worker makes its workspaces in.
	// Default <LOOMCYCLE_DATA_DIR>/workspaces (Config.WorkspaceLocalRoot).
	Root string `yaml:"root,omitempty"`
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
	DefaultWorkspaceDiskQuotaMB    = 2048
	DefaultWorkspaceCopyMaxFiles   = 2000
	DefaultWorkspaceCopyMaxBytes   = 1 << 30
	DefaultWorkspaceConvertMemMB   = 1024
	DefaultWorkspaceCheckoutDocs   = 500
	DefaultWorkspaceCheckoutBytes  = 256 << 20
	// The bounds of workspace.convert.memory_limit_mb.
	minWorkspaceConvertMemMB = 64
	maxWorkspaceConvertMemMB = 1 << 20
	// MaxWorkspaceDiskQuotaMB is 16 TiB in MiB.
	MaxWorkspaceDiskQuotaMB = 16 << 20
	// maxWorkspaceCopyFiles and maxWorkspaceCopyBytes bound what the copy
	// bounds may be set to.
	maxWorkspaceCopyFiles = 1_000_000
	maxWorkspaceCopyBytes = 16 << 40
	// maxWorkspaceCheckoutDocs bounds what workspace.checkout.max_documents
	// may be set to; max_bytes shares the copy bound's 16 TiB.
	maxWorkspaceCheckoutDocs = 100_000
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
	if w.DiskQuotaMB == 0 {
		w.DiskQuotaMB = DefaultWorkspaceDiskQuotaMB
	}
	if w.Copy.MaxFiles == 0 {
		w.Copy.MaxFiles = DefaultWorkspaceCopyMaxFiles
	}
	if w.Copy.MaxBytes == 0 {
		w.Copy.MaxBytes = DefaultWorkspaceCopyMaxBytes
	}
	if w.Convert.MemoryLimitMB == 0 {
		w.Convert.MemoryLimitMB = DefaultWorkspaceConvertMemMB
	}
	if w.Checkout.MaxDocuments == 0 {
		w.Checkout.MaxDocuments = DefaultWorkspaceCheckoutDocs
	}
	if w.Checkout.MaxBytes == 0 {
		w.Checkout.MaxBytes = DefaultWorkspaceCheckoutBytes
	}
	return w
}

// RemoteWorkers reports whether the mode listens for workspace workers.
func (w WorkspaceConfig) RemoteWorkers() bool {
	return w.Mode == WorkspaceModeRemote || w.Mode == WorkspaceModeLocalRemote
}

// LocalWorker reports whether the mode starts the local workspace worker.
func (w WorkspaceConfig) LocalWorker() bool {
	return w.Mode == WorkspaceModeLocal || w.Mode == WorkspaceModeLocalRemote
}

// Serves reports whether the mode serves workspaces at all.
func (w WorkspaceConfig) Serves() bool { return w.LocalWorker() || w.RemoteWorkers() }

// WorkspaceLocalTenant is the one tenant the local workspace worker serves,
// and a few words on why. workspace.local.tenant when written; otherwise the
// tenant a run on this install carries when its request names none, which is
// how a single-user install runs:
//
//   - under the legacy LOOMCYCLE_AUTH_TOKEN, LegacyTenant: the API resolves
//     that secret to a principal of tenant "default" and ignores the wire
//     tenant_id;
//   - with no authentication configured (open mode), the empty tenant: no
//     principal is stamped and a run keeps the tenant_id its request names,
//     none meaning "" (the dispatcher serves "" on the local worker only);
//   - with config principals and no legacy secret, their tenant when they
//     all share one; with several, LegacyTenant, and the operator should
//     write workspace.local.tenant (the reason says so).
//
// A token minted into the store is not seen here (config loads before the
// store): an install whose runs carry a minted token's tenant writes
// workspace.local.tenant.
func (c *Config) WorkspaceLocalTenant() (tenant, why string) {
	if t := c.Workspace.Local.Tenant; t != "" {
		return t, "workspace.local.tenant"
	}
	if c.Env.AuthToken != "" {
		return LegacyTenant, "the legacy LOOMCYCLE_AUTH_TOKEN runs every run under tenant " + LegacyTenant
	}
	if len(c.Principals) == 0 {
		return "", "no authentication is configured (open mode): a run that names no tenant_id has the empty tenant"
	}
	tenants := map[string]bool{}
	for _, p := range c.Principals {
		tenants[p.Tenant] = true
	}
	if len(tenants) == 1 {
		for t := range tenants {
			return t, "every config principal is of tenant " + t
		}
	}
	return LegacyTenant, "config principals of several tenants; set workspace.local.tenant to the one whose runs use the local worker"
}

// WorkspaceLocalRoot is where the local workspace worker keeps its
// workspaces: workspace.local.root, else a directory of the data dir, so they
// survive a restart of the runtime and the next attach reconciles them.
func (c *Config) WorkspaceLocalRoot() string {
	if r := c.Workspace.Local.Root; r != "" {
		return r
	}
	return filepath.Join(c.Env.DataDir, "workspaces")
}

// validateWorkspace refuses a mode it does not know, a remote mode with no
// worker endpoint, a local block for a mode that starts no local worker, and
// values out of range. The endpoint's own values are validateConversion's,
// which runs first. Part of validate.
func validateWorkspace(c *Config) error {
	w := c.Workspace
	switch w.Mode {
	case "", WorkspaceModeOff, WorkspaceModeRemote, WorkspaceModeLocal, WorkspaceModeLocalRemote:
	default:
		return fmt.Errorf("workspace.mode: unknown mode %q (want %s, %s, %s or %s)", w.Mode,
			WorkspaceModeOff, WorkspaceModeLocal, WorkspaceModeRemote, WorkspaceModeLocalRemote)
	}
	if w.Local != (WorkspaceLocalConfig{}) && !w.LocalWorker() {
		return fmt.Errorf("workspace.local: set, but workspace.mode %q starts no local worker (want %s or %s)", w.Mode, WorkspaceModeLocal, WorkspaceModeLocalRemote)
	}
	if w.RemoteWorkers() && c.ResolvedWorkerEndpoint().Listen == "" {
		return fmt.Errorf("worker_endpoint.listen: workspace mode %s needs the host:port workers attach to", w.Mode)
	}
	if w.MaxPerRun < 0 || w.MaxPerTenant < 0 || w.LostAfterS < 0 || w.Exec.DefaultTimeoutS < 0 || w.Exec.MaxTimeoutS < 0 {
		return errors.New("workspace: max_per_run, max_per_tenant, lost_after_s and the exec timeouts may not be negative")
	}
	if w.DiskQuotaMB < 0 || w.DiskQuotaMB > MaxWorkspaceDiskQuotaMB {
		return fmt.Errorf("workspace.disk_quota_mb: %d is out of range (1 to %d, 16 TiB)", w.DiskQuotaMB, MaxWorkspaceDiskQuotaMB)
	}
	if w.Copy.MaxFiles < 0 || w.Copy.MaxFiles > maxWorkspaceCopyFiles {
		return fmt.Errorf("workspace.copy.max_files: %d is out of range (1 to %d)", w.Copy.MaxFiles, maxWorkspaceCopyFiles)
	}
	if w.Copy.MaxBytes < 0 || w.Copy.MaxBytes > maxWorkspaceCopyBytes {
		return fmt.Errorf("workspace.copy.max_bytes: %d is out of range (1 to %d, 16 TiB)", w.Copy.MaxBytes, int64(maxWorkspaceCopyBytes))
	}
	if w.Checkout.MaxDocuments < 0 || w.Checkout.MaxDocuments > maxWorkspaceCheckoutDocs {
		return fmt.Errorf("workspace.checkout.max_documents: %d is out of range (1 to %d)", w.Checkout.MaxDocuments, maxWorkspaceCheckoutDocs)
	}
	if w.Checkout.MaxBytes < 0 || w.Checkout.MaxBytes > maxWorkspaceCopyBytes {
		return fmt.Errorf("workspace.checkout.max_bytes: %d is out of range (1 to %d, 16 TiB)", w.Checkout.MaxBytes, int64(maxWorkspaceCopyBytes))
	}
	if m := w.Convert.MemoryLimitMB; m != 0 && (m < minWorkspaceConvertMemMB || m > maxWorkspaceConvertMemMB) {
		return fmt.Errorf("workspace.convert.memory_limit_mb: %d is out of range (%d to %d)", m, minWorkspaceConvertMemMB, maxWorkspaceConvertMemMB)
	}
	r := w.Resolved()
	if r.Exec.DefaultTimeoutS > r.Exec.MaxTimeoutS {
		return fmt.Errorf("workspace.exec.default_timeout_s (%d) is over max_timeout_s (%d)", r.Exec.DefaultTimeoutS, r.Exec.MaxTimeoutS)
	}
	return nil
}

// InertWorkspaceGrants names, sorted, the agents whose workspace grant
// reaches nothing: Workspace is not in their tools, or workspace.mode is off.
// The same agents workspaceWarnings advises on at load.
func (c *Config) InertWorkspaceGrants() []string {
	mode := c.Workspace.Resolved().Mode
	var out []string
	for name, a := range c.Agents {
		if len(workspaceWarnings(name, a, mode)) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (w *WorkspaceConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "workspace", nil, reflect.TypeOf(*w)); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "exec"), "workspace.exec", nil, reflect.TypeOf(WorkspaceExecConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "local"), "workspace.local", nil, reflect.TypeOf(WorkspaceLocalConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "copy"), "workspace.copy", nil, reflect.TypeOf(WorkspaceCopyConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "convert"), "workspace.convert", nil, reflect.TypeOf(WorkspaceConvertConfig{})); err != nil {
		return err
	}
	type plain WorkspaceConfig
	return n.Decode((*plain)(w))
}
