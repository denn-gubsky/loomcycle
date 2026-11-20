package main

import (
	"context"
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
// the programs an exec may start with.
var localWorkspacePassEnv = []string{"LOOMCYCLE_WORKSPACE_CAPACITY", "LOOMCYCLE_WORKSPACE_IDLE_STREAMS", "LOOMCYCLE_WORKSPACE_COMMANDS"}

// startLocalWorkspace starts workspace.mode local (RFC GW D9): one child
// `loomcycle worker --role workspace` on a Unix socket in a private (0700)
// directory, presenting a per-boot token the dispatcher accepts on that
// socket only, as a worker serving tenant alone. The socket has a gRPC
// server of its own, which serves nothing but the workspace service, so the
// token never reaches the network endpoint's. The child is restarted with
// backoff, exits when this process does (--parent-pid), and is stopped at
// shutdown before the socket closes.
func (b *boot) startLocalWorkspace(d *wsdispatch.Dispatcher, token, tenant string) {
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
	env := []string{"LOOMCYCLE_WORKSPACE_ROOT=" + root}
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
		"tenant", tenant, "root", root)
}
