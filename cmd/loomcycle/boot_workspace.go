package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"google.golang.org/grpc"

	"github.com/denn-gubsky/loomcycle/internal/conversion/local"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	wsdispatch "github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// localWorkspaceID is the local workspace worker's --id. It is the same on
// every start: the rows of the workspaces it holds name it, so a child
// restarted after a crash, or by the next boot, reconciles the directories
// it finds under its root instead of reading them as unknown.
const localWorkspaceID = "local"

// localWorkspacePassEnv are the core's workspace-worker settings a local
// child takes when the operator set them: its capacity, its idle streams and
// the programs an exec may start with, and the host egress ports.
var localWorkspacePassEnv = []string{"LOOMCYCLE_WORKSPACE_CAPACITY", "LOOMCYCLE_WORKSPACE_IDLE_STREAMS", "LOOMCYCLE_WORKSPACE_COMMANDS", "LOOMCYCLE_WORKSPACE_HOST_EGRESS"}

// startLocalWorkspace starts workspace.mode local (RFC GW D9): one child
// `loomcycle worker --role workspace` on a Unix socket in a private (0700)
// directory, presenting a per-boot token the dispatcher accepts on that
// socket only, as a worker serving tenant alone (Config.WorkspaceLocalTenant;
// why is its reason, logged). The socket has a gRPC
// server of its own, which serves nothing but the workspace service, so the
// token never reaches the network endpoint's. The child is restarted with
// backoff, exits when this process does (--parent-pid), and is stopped at
// shutdown before the socket closes.
func (b *boot) startLocalWorkspace(d *wsdispatch.Dispatcher, token, tenant, why string) {
	bin, err := os.Executable()
	if err != nil {
		fatal("workspace: cannot find this binary to start the local worker", "err", err)
	}
	l, socket, closeSocket, err := local.Listen()
	if err != nil {
		fatal("workspace: cannot open the local worker's socket", "err", err)
	}
	root := b.cfg.WorkspaceLocalRoot()
	srv := grpc.NewServer(workerlink.ServerOptions()...)
	d.RegisterLocal(srv)
	safego.Go(context.Background(), "workspace.local.socket", func() {
		if err := srv.Serve(l); err != nil {
			slog.Warn("workspace: the local worker's socket stopped", "err", err)
		}
	})
	env := []string{"LOOMCYCLE_WORKSPACE_ROOT=" + root,
		fmt.Sprintf("LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB=%d", b.cfg.Workspace.Resolved().Convert.MemoryLimitMB),
		// RFC GW P11: the local worker's mirrors end per workspace.git.
		fmt.Sprintf("LOOMCYCLE_WORKSPACE_MIRROR_IDLE_S=%d", b.cfg.Workspace.Resolved().Git.MirrorIdleS)}
	// The worker's conversion child (RFC GW D34) runs the helper commands
	// this runtime's own conversions run, so a workspace's file converts
	// as an inbox file does here.
	h := conversionHelpers(b.cfg)
	for _, kv := range [][2]string{
		{"LOOMCYCLE_WORKER_RASTERIZE_COMMAND", h.rasterize}, {"LOOMCYCLE_WORKER_MEDIA_COMMAND", h.media},
		{"LOOMCYCLE_WORKER_ENCODE_COMMAND", h.encode}, {"LOOMCYCLE_WORKER_WRITE_COMMAND", b.cfg.Env.RenderPDFCommand},
		{"LOOMCYCLE_WORKER_MERMAID_COMMAND", b.cfg.Env.RenderDiagramCommand}, {"LOOMCYCLE_HELPER_WRAPPER", b.cfg.Env.HelperWrapper},
		{"LOOMCYCLE_WORKER_DJVU_TEXT_COMMAND", h.djvuText}, {"LOOMCYCLE_WORKER_DJVU_INSPECT_COMMAND", h.djvuInspect},
		{"LOOMCYCLE_WORKER_DJVU_RASTERIZE_COMMAND", h.djvuRasterize},
	} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	for _, k := range localWorkspacePassEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	sup := local.Start(local.Options{Binary: bin, Socket: socket, Token: token, Role: "workspace", ID: localWorkspaceID, Env: env, Logger: slog.Default()})
	b.addCleanup(func() {
		sup.Stop()
		srv.Stop()
		closeSocket()
	})
	// Said plainly, once: local mode is for a trusted single-user machine.
	// The child's environment is scrubbed, but it runs as this process's
	// user, so a command can read whatever this user can: the data
	// directory, the config and secrets files, and on Linux this process's
	// own environment (/proc/<pid>/environ), provider keys included.
	slog.Warn("workspace: mode local runs every agent's workspace commands on this host as this runtime's own user, with no isolation and no network restriction: a command can read anything this user can, including the runtime's data directory, its config and secrets files, and its environment with the provider keys. Use it only on a machine where the agents are trusted",
		"tenant", tenant, "tenant_from", why, "root", root)
}
