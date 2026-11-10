package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Unset, the listener is off: nothing is bound and no server is returned.
func TestStartMetricsListener_UnsetOpensNothing(t *testing.T) {
	srv, addr, err := startMetricsListener("", http.NotFoundHandler(), discardLog())
	if srv != nil || addr != nil || err != nil {
		t.Fatalf("got server=%v addr=%v err=%v, want all nil", srv, addr, err)
	}
}

// Set, it serves the handler on its own socket with no credential.
func TestStartMetricsListener_ServesTheHandlerOnItsOwnSocket(t *testing.T) {
	h := http.NewServeMux()
	h.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "up 1\n") })
	srv, addr, err := startMetricsListener("127.0.0.1:0", h, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	resp, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "up 1\n" {
		t.Errorf("GET /metrics: %d %q", resp.StatusCode, body)
	}
}

// A bind failure is the caller's error to fail the boot on, not a skip.
func TestStartMetricsListener_BindFailureIsAnError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	srv, _, err := startMetricsListener(held.Addr().String(), http.NotFoundHandler(), discardLog())
	if err == nil || srv != nil {
		t.Fatalf("binding a held port: server=%v err=%v, want an error", srv, err)
	}
}

// One Info names the address; a Warn follows only when the address reaches
// beyond loopback, since the port is unauthenticated.
func TestAnnounceMetricsListener_WarnsOnlyBeyondLoopback(t *testing.T) {
	for addr, wantWarn := range map[string]bool{
		"127.0.0.1:9464": false,
		"[::1]:9464":     false,
		"localhost:9464": false,
		"0.0.0.0:9464":   true,
		":9464":          true,
		"10.0.0.5:9464":  true,
		"metrics:9464":   true,
	} {
		var buf bytes.Buffer
		announceMetricsListener(slog.New(slog.NewTextHandler(&buf, nil)), addr)
		out := buf.String()
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, addr) {
			t.Errorf("%s: no Info naming the address: %s", addr, out)
		}
		if got := strings.Contains(out, "level=WARN"); got != wantWarn {
			t.Errorf("%s: warned=%v, want %v: %s", addr, got, wantWarn, out)
		}
	}
}
