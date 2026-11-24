package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// A host workspace's egress (RFC GW §6.3; P4 plan O13).
//
// The host engine is not a sandbox: its commands run with the host's own
// network. The loopback port here is a convenience, so a script and the
// package managers reach a workspace's routes the same way on both engines
// (http://<base>/<route>/…), with the core adding a route's credential. It is
// NOT a boundary: the port is open to every process on this host, of any
// user, for as long as the workspace is held here, and a request to it is
// sent as that workspace's (its routes, its rate and byte budget, its
// credentials). It is served only when the operator turns it on
// (LOOMCYCLE_WORKSPACE_HOST_EGRESS).

// hostEgress is one workspace's loopback listener.
type hostEgress struct {
	srv  *http.Server
	base string
}

// egressEnv is what a workspace's commands get to reach its routes: its
// address, and pip and npm pointed at the `workspace` preset's routes. A
// workspace that does not hold one of them is refused by the core with the
// route's name (P4 plan O22).
func egressEnv(base, pipTrustedHost string) []string {
	return []string{
		"LOOMCYCLE_EGRESS=" + base,
		"PIP_INDEX_URL=" + base + "pypi/simple/",
		"PIP_TRUSTED_HOST=" + pipTrustedHost,
		"npm_config_registry=" + base + "npm/",
		// Else every install also POSTs to the audit endpoint (spike C).
		"npm_config_audit=false",
		"npm_config_fund=false",
		"npm_config_update_notifier=false",
	}
}

// egressEnvFor opens the workspace's port on its first command and returns
// the environment its commands reach it with; none without Egress.
func (h *Host) egressEnvFor(workspace string) ([]string, error) {
	if h.Egress == nil || workspace == "" {
		return nil, nil
	}
	h.egressMu.Lock()
	defer h.egressMu.Unlock()
	if e := h.egress[workspace]; e != nil {
		return egressEnv(e.base, "127.0.0.1"), nil
	}
	// 127.0.0.1, never a wildcard: the port is for this host's processes.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("opening the workspace's egress port: %w", err)
	}
	srv := &http.Server{
		Handler:           h.Egress(workspace),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	e := &hostEgress{srv: srv, base: "http://" + l.Addr().String() + "/"}
	if h.egress == nil {
		h.egress = map[string]*hostEgress{}
	}
	h.egress[workspace] = e
	safego.Go(context.Background(), "workspace.engine.host-egress.serve", func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("workspace egress port stopped", "workspace", workspace, "err", err)
		}
	})
	return egressEnv(e.base, "127.0.0.1"), nil
}

// forgetEgress closes a workspace's port.
func (h *Host) forgetEgress(workspace string) {
	h.egressMu.Lock()
	e := h.egress[workspace]
	delete(h.egress, workspace)
	h.egressMu.Unlock()
	if e != nil {
		_ = e.srv.Close()
	}
}

// closeEgress closes every workspace's port.
func (h *Host) closeEgress() {
	h.egressMu.Lock()
	all := h.egress
	h.egress = nil
	h.egressMu.Unlock()
	for _, e := range all {
		_ = e.srv.Close()
	}
}
