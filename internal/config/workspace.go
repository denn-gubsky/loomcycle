package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
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
	// Kernel configures the persistent interpreters (RFC GW §5.7).
	Kernel WorkspaceKernelConfig `yaml:"kernel,omitempty"`
	// Egress is the named routes a workspace reaches the network through
	// (RFC GW §6.3).
	Egress WorkspaceEgressConfig `yaml:"egress,omitempty"`
	// MCP configures Workspace on loomcycle's MCP server (RFC GW D35): off
	// unless enabled.
	MCP WorkspaceMCPConfig `yaml:"mcp,omitempty"`
	// Expose is `workspace.expose`: serving a workspace's port to its user
	// through the API (RFC GW §5.3). Off unless enabled.
	Expose WorkspaceExposeConfig `yaml:"expose,omitempty"`
	// Durable is `workspace.durable`: workspaces kept past their run under
	// the tenant, the user and a name (RFC GW §4.2, P12). Off unless
	// enabled.
	Durable WorkspaceDurableConfig `yaml:"durable,omitempty"`
	// PersonalWorkers lets a user's own machine attach as a worker of that
	// user's runs (RFC GW §4.5, D29). Off by default; on only for a local
	// deployment: the worker endpoint must listen on loopback (D32).
	PersonalWorkers bool `yaml:"personal_workers,omitempty"`
	// Checkpoint bounds a workspace's checkpoints (RFC GW §5.4).
	Checkpoint WorkspaceCheckpointConfig `yaml:"checkpoint,omitempty"`
	// Template bounds workspace templates (RFC GW §5.5).
	Template WorkspaceTemplateConfig `yaml:"template,omitempty"`
	// Push bounds a git workspace's push (RFC GW §6.2, P11).
	Push WorkspacePushConfig `yaml:"push,omitempty"`
}

// WorkspaceTemplateConfig is `workspace.template`: the size of one template's
// archive and how many a tenant holds (RFC GW §5.5).
type WorkspaceTemplateConfig struct {
	// MaxMB bounds one template's archive, in MiB. Default 1024.
	MaxMB int64 `yaml:"max_mb,omitempty"`
	// MaxPerTenant bounds the templates a tenant holds, user and tenant
	// ones together. Default 20.
	MaxPerTenant int `yaml:"max_per_tenant,omitempty"`
	// MaxPerUser bounds one user's user templates, so one member cannot
	// fill the tenant's bound alone. Default 5, or max_per_tenant when that
	// is lower; at most max_per_tenant.
	MaxPerUser int `yaml:"max_per_user,omitempty"`
}

// WorkspaceCheckpointConfig is `workspace.checkpoint`. A checkpoint's size
// counts against its workspace's disk quota; this bounds their number.
type WorkspaceCheckpointConfig struct {
	// MaxPerWorkspace is the checkpoints one workspace may hold. Default 8.
	MaxPerWorkspace int `yaml:"max_per_workspace,omitempty"`
}

// WorkspaceMCPConfig is `workspace.mcp`: the `workspace` tool of loomcycle's
// MCP server, whose workspaces belong to the session's token identity
// (tenant and subject), not to a run (RFC GW D35).
type WorkspaceMCPConfig struct {
	// Enabled offers the tool. Off by default.
	Enabled bool `yaml:"enabled,omitempty"`
	// Ops are what a session may do in its workspaces: read, write, exec.
	// Empty is read and write; exec only when listed. Opening is implied.
	Ops []string `yaml:"ops,omitempty"`
	// IdleReleaseS releases a workspace no op reached for this long.
	IdleReleaseS int `yaml:"idle_release_s,omitempty"`
	// MaxLifetimeS releases a workspace this long after it opened, however
	// busy.
	MaxLifetimeS int `yaml:"max_lifetime_s,omitempty"`
	// Egress are the operator routes (workspace.egress.routes) a session's
	// workspace may use; open may name a subset. Empty, none (P4 plan O28).
	Egress []string `yaml:"egress,omitempty"`
}

// The workspace.mcp defaults, and the bounds its two times may be set to.
const (
	DefaultWorkspaceMCPIdleReleaseS = 3600
	DefaultWorkspaceMCPMaxLifetimeS = 86400
	maxWorkspaceMCPSeconds          = 30 * 86400
)

// WorkspaceDurableConfig is `workspace.durable` (RFC GW P12): a durable
// workspace outlives its run and keeps a worker's disk, so it is off unless
// the operator turns it on, and bounded per user.
type WorkspaceDurableConfig struct {
	// Enabled lets an agent with the durable grant open one. Off by
	// default.
	Enabled bool `yaml:"enabled,omitempty"`
	// MaxPerUser is how many durable workspaces, pinned or parked, one
	// user of a tenant may keep.
	MaxPerUser int `yaml:"max_per_user,omitempty"`
}

// The durable bound's default and ceiling.
const (
	DefaultWorkspaceDurableMaxPerUser = 8
	maxWorkspaceDurablePerUser        = 1024
)

// WorkspaceExposeConfig is `workspace.expose`: a workspace's server served
// to its user at /v1/workspaces/exposures/{id}/… (RFC GW §5.3, P9 plan
// §3.3). Every bound is per workspace; the connection and rate bounds are
// kept per replica.
type WorkspaceExposeConfig struct {
	// Enabled lets an agent with the expose grant publish a port. Off by
	// default.
	Enabled bool `yaml:"enabled,omitempty"`
	// MaxPerWorkspace is how many ports one workspace may expose at once.
	MaxPerWorkspace int `yaml:"max_per_workspace,omitempty"`
	// MaxConnections is how many requests to one workspace's exposures may
	// be open at once.
	MaxConnections int `yaml:"max_connections,omitempty"`
	// IdleTimeoutS closes an exposure no request used for this long.
	IdleTimeoutS int `yaml:"idle_timeout_s,omitempty"`
	// RequestsPerSecond and BytesPerSecond bound one workspace's exposures:
	// requests started, and response bytes sent.
	RequestsPerSecond int   `yaml:"requests_per_second,omitempty"`
	BytesPerSecond    int64 `yaml:"bytes_per_second,omitempty"`
	// MaxMessageBytes bounds one WebSocket message through a page, each
	// way (RFC GW §5.3: "operator setting").
	MaxMessageBytes int64 `yaml:"max_message_bytes,omitempty"`
	// SocketLifetimeS closes a page's WebSocket this long after it opened,
	// however busy. A quiet one closes at IdleTimeoutS.
	SocketLifetimeS int `yaml:"socket_lifetime_s,omitempty"`
}

// The workspace.expose defaults and bounds.
const (
	DefaultWorkspaceExposeMaxPerWorkspace = 2
	DefaultWorkspaceExposeMaxConnections  = 32
	DefaultWorkspaceExposeIdleTimeoutS    = 900
	DefaultWorkspaceExposeRequestsPerS    = 20
	DefaultWorkspaceExposeBytesPerS       = 8 << 20
	DefaultWorkspaceExposeMessageBytes    = 16 << 20
	DefaultWorkspaceExposeSocketLifetimeS = 14400
	maxWorkspaceExposeMessageBytes        = 64 << 20
	maxWorkspaceExposeSocketLifetimeS     = 86400
	maxWorkspaceExposePerWorkspace        = 16
	maxWorkspaceExposeConnections         = 1024
	maxWorkspaceExposeIdleTimeoutS        = 86400
	maxWorkspaceExposeRequestsPerS        = 10000
	maxWorkspaceExposeBytesPerS           = 1 << 30
)

// WorkspaceKernelConfig is `workspace.kernel`.
type WorkspaceKernelConfig struct {
	// InterruptGraceSeconds is how long a cell interrupted at its timeout
	// has to stop before its kernel is killed and its variables lost: an
	// interrupt does not reach a long call in native code. Default 5.
	InterruptGraceSeconds int `yaml:"interrupt_grace_seconds,omitempty"`
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
	DefaultWorkspaceKernelGraceS   = 5
	DefaultWorkspaceCheckpoints    = 8
	DefaultWorkspaceTemplateMaxMB  = 1024
	DefaultWorkspaceTemplates      = 20
	DefaultWorkspaceUserTemplates  = 5
	// maxWorkspaceTemplateMB bounds workspace.template.max_mb (64 GiB), and
	// maxWorkspaceTemplates workspace.template.max_per_tenant.
	maxWorkspaceTemplateMB = 64 << 10
	maxWorkspaceTemplates  = 1000
	// maxWorkspaceCheckpoints bounds workspace.checkpoint.max_per_workspace.
	maxWorkspaceCheckpoints = 100
	// maxWorkspaceKernelGraceS bounds workspace.kernel.interrupt_grace_seconds.
	maxWorkspaceKernelGraceS = 300
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
	if w.Kernel.InterruptGraceSeconds == 0 {
		w.Kernel.InterruptGraceSeconds = DefaultWorkspaceKernelGraceS
	}
	if w.Checkpoint.MaxPerWorkspace == 0 {
		w.Checkpoint.MaxPerWorkspace = DefaultWorkspaceCheckpoints
	}
	if w.Template.MaxMB == 0 {
		w.Template.MaxMB = DefaultWorkspaceTemplateMaxMB
	}
	if w.Template.MaxPerTenant == 0 {
		w.Template.MaxPerTenant = DefaultWorkspaceTemplates
	}
	if w.Template.MaxPerUser == 0 {
		w.Template.MaxPerUser = min(DefaultWorkspaceUserTemplates, w.Template.MaxPerTenant)
	}
	w.Egress = w.Egress.Resolved()
	w.Push = w.Push.Resolved()
	if w.Expose.MaxPerWorkspace == 0 {
		w.Expose.MaxPerWorkspace = DefaultWorkspaceExposeMaxPerWorkspace
	}
	if w.Durable.MaxPerUser == 0 {
		w.Durable.MaxPerUser = DefaultWorkspaceDurableMaxPerUser
	}
	if w.Expose.MaxConnections == 0 {
		w.Expose.MaxConnections = DefaultWorkspaceExposeMaxConnections
	}
	if w.Expose.IdleTimeoutS == 0 {
		w.Expose.IdleTimeoutS = DefaultWorkspaceExposeIdleTimeoutS
	}
	if w.Expose.RequestsPerSecond == 0 {
		w.Expose.RequestsPerSecond = DefaultWorkspaceExposeRequestsPerS
	}
	if w.Expose.BytesPerSecond == 0 {
		w.Expose.BytesPerSecond = DefaultWorkspaceExposeBytesPerS
	}
	if w.Expose.MaxMessageBytes == 0 {
		w.Expose.MaxMessageBytes = DefaultWorkspaceExposeMessageBytes
	}
	if w.Expose.SocketLifetimeS == 0 {
		w.Expose.SocketLifetimeS = DefaultWorkspaceExposeSocketLifetimeS
	}
	if len(w.MCP.Ops) == 0 {
		w.MCP.Ops = []string{WorkspaceOpRead, WorkspaceOpWrite}
	}
	if w.MCP.IdleReleaseS == 0 {
		w.MCP.IdleReleaseS = DefaultWorkspaceMCPIdleReleaseS
	}
	if w.MCP.MaxLifetimeS == 0 {
		w.MCP.MaxLifetimeS = DefaultWorkspaceMCPMaxLifetimeS
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

// WorkspaceTemplateDir is where the core keeps template archives (RFC GW
// §5.5): a directory of the data dir.
func (c *Config) WorkspaceTemplateDir() string {
	return filepath.Join(c.Env.DataDir, "workspace-templates")
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
	if err := validatePersonalWorkers(c); err != nil {
		return err
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
	if g := w.Kernel.InterruptGraceSeconds; g < 0 || g > maxWorkspaceKernelGraceS {
		return fmt.Errorf("workspace.kernel.interrupt_grace_seconds: %d is out of range (1 to %d)", g, maxWorkspaceKernelGraceS)
	}
	if err := validateWorkspacePush(w.Push); err != nil {
		return err
	}
	if c := w.Checkpoint.MaxPerWorkspace; c < 0 || c > maxWorkspaceCheckpoints {
		return fmt.Errorf("workspace.checkpoint.max_per_workspace: %d is out of range (1 to %d)", c, maxWorkspaceCheckpoints)
	}
	if m := w.Template.MaxMB; m < 0 || m > maxWorkspaceTemplateMB {
		return fmt.Errorf("workspace.template.max_mb: %d is out of range (1 to %d)", m, maxWorkspaceTemplateMB)
	}
	if c := w.Template.MaxPerTenant; c < 0 || c > maxWorkspaceTemplates {
		return fmt.Errorf("workspace.template.max_per_tenant: %d is out of range (1 to %d)", c, maxWorkspaceTemplates)
	}
	if u, r := w.Template.MaxPerUser, w.Resolved().Template; u < 0 || u > r.MaxPerTenant {
		return fmt.Errorf("workspace.template.max_per_user: %d is out of range (1 to max_per_tenant, %d)", u, r.MaxPerTenant)
	}
	for _, op := range w.MCP.Ops {
		switch op {
		case WorkspaceOpRead, WorkspaceOpWrite, WorkspaceOpExec:
		default:
			return fmt.Errorf("workspace.mcp.ops: %q is not read, write or exec", op)
		}
	}
	if d := w.Durable.MaxPerUser; d < 0 || d > maxWorkspaceDurablePerUser {
		return fmt.Errorf("workspace.durable.max_per_user: %d is not from 1 to %d", d, maxWorkspaceDurablePerUser)
	}
	if err := validateWorkspaceExpose(w.Expose); err != nil {
		return err
	}
	if v := w.MCP.IdleReleaseS; v < 0 || v > maxWorkspaceMCPSeconds {
		return fmt.Errorf("workspace.mcp.idle_release_s: %d is out of range (1 to %d)", v, maxWorkspaceMCPSeconds)
	}
	if v := w.MCP.MaxLifetimeS; v < 0 || v > maxWorkspaceMCPSeconds {
		return fmt.Errorf("workspace.mcp.max_lifetime_s: %d is out of range (1 to %d)", v, maxWorkspaceMCPSeconds)
	}
	if err := validateWorkspaceEgress(w.Egress); err != nil {
		return err
	}
	// Only the operator's own routes: a session's workspace has no run, so
	// no tenant route is resolved for it (P4 plan O28).
	for i, name := range w.MCP.Egress {
		switch {
		case !ValidEgressRouteName(name):
			return fmt.Errorf("workspace.mcp.egress[%d]: %q is not a route name", i, name)
		case slices.Contains(w.MCP.Egress[:i], name):
			return fmt.Errorf("workspace.mcp.egress[%d]: %q is listed twice", i, name)
		}
		if _, ok := w.Egress.Routes[name]; !ok {
			return fmt.Errorf("workspace.mcp.egress[%d]: %q is not an operator route (workspace.egress.routes)", i, name)
		}
	}
	r := w.Resolved()
	if r.Exec.DefaultTimeoutS > r.Exec.MaxTimeoutS {
		return fmt.Errorf("workspace.exec.default_timeout_s (%d) is over max_timeout_s (%d)", r.Exec.DefaultTimeoutS, r.Exec.MaxTimeoutS)
	}
	return nil
}

// workspaceGrantWarnings is workspaceWarnings for agent name of c, less the
// mode-off advisory on a grant an embedded bundle gave and no operator layer
// changed. Under RFC GW P14 (owner ruling Q4 a) a bundle's agents gain
// Workspace and keep their old shell tool until P15, so on an install with
// workspaces off the grant is a prepared path, not an operator's mistake. An
// agent the operator took the tool from is still advised on.
func (c *Config) workspaceGrantWarnings(name string, a AgentDef) []string {
	mode := c.Workspace.Resolved().Mode
	if mode == WorkspaceModeOff && c.bundledWorkspace[name] && slices.Contains(a.Tools, "Workspace") {
		return nil
	}
	return workspaceWarnings(name, a, mode)
}

// bundledWorkspaceGrants names the agents whose merged workspace grant is
// the one the last embedded layer setting it gave. A layer that does not
// parse here gives none, so its agents keep every advisory.
func bundledWorkspaceGrants(cfg *Config, layers []Layer) map[string]bool {
	given := map[string]*AgentWorkspace{}
	for _, l := range layers {
		if !l.Embedded || l.Data == nil {
			continue
		}
		var doc struct {
			Agents map[string]struct {
				Workspace *AgentWorkspace `yaml:"workspace"`
			} `yaml:"agents"`
		}
		if yaml.Unmarshal(l.Data, &doc) != nil {
			continue
		}
		for name, a := range doc.Agents {
			if a.Workspace != nil {
				given[name] = a.Workspace
			}
		}
	}
	out := map[string]bool{}
	for name, w := range given {
		if a, ok := cfg.Agents[name]; ok && reflect.DeepEqual(a.Workspace, w) {
			out[name] = true
		}
	}
	return out
}

// bundledGitRemotes names the agents whose git_remote an embedded layer gave
// and the merged config still holds unchanged, as bundledWorkspaceGrants does
// for the workspace grant.
func bundledGitRemotes(cfg *Config, layers []Layer) map[string]bool {
	given := map[string][]string{}
	for _, l := range layers {
		if !l.Embedded || l.Data == nil {
			continue
		}
		var doc struct {
			Agents map[string]struct {
				GitRemote []string `yaml:"git_remote"`
			} `yaml:"agents"`
		}
		if yaml.Unmarshal(l.Data, &doc) != nil {
			continue
		}
		for name, a := range doc.Agents {
			if a.GitRemote != nil {
				given[name] = a.GitRemote
			}
		}
	}
	out := map[string]bool{}
	for name, g := range given {
		if a, ok := cfg.Agents[name]; ok && slices.Equal(a.GitRemote, g) {
			out[name] = true
		}
	}
	return out
}

// InertWorkspaceGrants names, sorted, the agents whose workspace grant
// reaches nothing: Workspace is not in their tools, or workspace.mode is off.
// The same agents workspaceWarnings advises on at load.
func (c *Config) InertWorkspaceGrants() []string {
	var out []string
	for name, a := range c.Agents {
		if len(c.workspaceGrantWarnings(name, a)) > 0 {
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
	if err := strictKeys(valueOf(n, "kernel"), "workspace.kernel", nil, reflect.TypeOf(WorkspaceKernelConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "push"), "workspace.push", nil, reflect.TypeOf(WorkspacePushConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "checkpoint"), "workspace.checkpoint", nil, reflect.TypeOf(WorkspaceCheckpointConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "template"), "workspace.template", nil, reflect.TypeOf(WorkspaceTemplateConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "mcp"), "workspace.mcp", nil, reflect.TypeOf(WorkspaceMCPConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "expose"), "workspace.expose", nil, reflect.TypeOf(WorkspaceExposeConfig{})); err != nil {
		return err
	}
	if err := strictKeys(valueOf(n, "durable"), "workspace.durable", nil, reflect.TypeOf(WorkspaceDurableConfig{})); err != nil {
		return err
	}
	type plain WorkspaceConfig
	return n.Decode((*plain)(w))
}

// validateWorkspaceExpose refuses a workspace.expose bound out of range. Zero
// is the default.
// validatePersonalWorkers holds workspace.personal_workers to a local
// deployment (RFC GW §4.5, D29, D32): a personal worker attaches over the
// worker endpoint, so the mode must accept workers there, and the endpoint
// must listen on loopback, so the worker and the core are one machine. It is
// checked, not assumed: a configuration that breaks it does not start.
func validatePersonalWorkers(c *Config) error {
	w := c.Workspace
	if !w.PersonalWorkers {
		return nil
	}
	if !w.RemoteWorkers() {
		return fmt.Errorf("workspace.personal_workers: a personal worker attaches over the worker endpoint, so workspace.mode must be %s or %s (it is %q)",
			WorkspaceModeRemote, WorkspaceModeLocalRemote, w.Mode)
	}
	if l := c.ResolvedWorkerEndpoint().Listen; !IsLoopbackListenAddr(l) {
		return fmt.Errorf("workspace.personal_workers: worker_endpoint.listen %q must be a loopback address (127.0.0.1, ::1 or localhost): a personal worker is for a local deployment only", l)
	}
	return nil
}

func validateWorkspaceExpose(e WorkspaceExposeConfig) error {
	for _, b := range []struct {
		name   string
		v, max int64
	}{
		{"max_per_workspace", int64(e.MaxPerWorkspace), maxWorkspaceExposePerWorkspace},
		{"max_connections", int64(e.MaxConnections), maxWorkspaceExposeConnections},
		{"idle_timeout_s", int64(e.IdleTimeoutS), maxWorkspaceExposeIdleTimeoutS},
		{"requests_per_second", int64(e.RequestsPerSecond), maxWorkspaceExposeRequestsPerS},
		{"bytes_per_second", e.BytesPerSecond, maxWorkspaceExposeBytesPerS},
		{"max_message_bytes", e.MaxMessageBytes, maxWorkspaceExposeMessageBytes},
		{"socket_lifetime_s", int64(e.SocketLifetimeS), maxWorkspaceExposeSocketLifetimeS},
	} {
		if b.v < 0 || b.v > b.max {
			return fmt.Errorf("workspace.expose.%s: %d is out of range (1 to %d)", b.name, b.v, b.max)
		}
	}
	return nil
}
