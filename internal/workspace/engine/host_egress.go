package engine

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// A host workspace's egress (RFC GW §6.3; P4 plan O13, O31).
//
// The host engine is not a sandbox: its commands run with the host's own
// network. The loopback port here is a convenience, so a script and the
// package managers reach a workspace's routes the same way on both engines,
// with the core adding a route's credential. The port answers only a request
// that names it as its Host (127.0.0.1:<port>, so a web page that a rebound
// DNS name pointed here is refused) and whose path starts with a secret made
// fresh each time the port opens, handed to the workspace's commands only in
// their environment (LOOMCYCLE_EGRESS=http://127.0.0.1:<port>/<secret>/).
// That keeps out a browser and another user's processes. It does not keep out
// this user's own processes, which can read a command's environment: other
// workspaces on the same host engine included. It is served only when the
// operator turns it on (LOOMCYCLE_WORKSPACE_HOST_EGRESS).

// hostEgress is one workspace's loopback listener.
type hostEgress struct {
	srv  *http.Server
	base string
}

// egressSecretBytes is the size of a port's secret: 32 random bytes, 43
// characters of base64url.
const egressSecretBytes = 32

// guardEgress serves next only a request for host (the listener's own
// 127.0.0.1:<port>) whose path starts with /<secret>/, which it strips: next
// sees /<route>/<path> as a container session's request reads. Anything else
// is a bare 403 that names nothing, so it echoes no part of the secret.
func guardEgress(host, secret string, next http.Handler) http.Handler {
	want := []byte(secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, rest, ok := strings.Cut(strings.TrimPrefix(r.RequestURI, "/"), "/")
		if r.Host != host || !strings.HasPrefix(r.RequestURI, "/") || !ok ||
			subtle.ConstantTimeCompare([]byte(first), want) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r2 := r.Clone(r.Context())
		r2.RequestURI = "/" + rest
		u, err := url.ParseRequestURI(r2.RequestURI)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		r2.URL = u
		next.ServeHTTP(w, r2)
	})
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
	raw := make([]byte, egressSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("making the workspace's egress secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	addr := l.Addr().String()
	srv := &http.Server{
		Handler:           guardEgress(addr, secret, h.Egress(workspace)),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	e := &hostEgress{srv: srv, base: "http://" + addr + "/" + secret + "/"}
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
