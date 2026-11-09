package http

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// TestWhoami_ReportsTheTraceURLTemplate: the Web UI learns the operator's
// trace link from /v1/_me, for an authenticated principal and in open mode,
// and nothing is said when none is configured.
func TestWhoami_ReportsTheTraceURLTemplate(t *testing.T) {
	const tmpl = "https://grafana.example/explore?traceId={trace_id}"
	for _, token := range []string{"", "secret-bearer"} {
		cfg := makeBaseConfig()
		cfg.Env.AuthToken = token
		cfg.Env.TraceURLTemplate = tmpl
		srv, _ := makeServer(t, completingProvider(), cfg)
		ts := httptest.NewServer(srv.Mux())
		code, body := bearerCall(t, ts, token, "GET", "/v1/_me", "")
		ts.Close()
		if code != 200 || jsonField(t, body, "trace_url_template") != tmpl {
			t.Errorf("auth token %q: /v1/_me = %d %s, want trace_url_template", token, code, body)
		}
	}
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	if _, body := bearerCall(t, ts, "", "GET", "/v1/_me", ""); strings.Contains(body, "trace_url_template") {
		t.Errorf("unset template reported: %s", body)
	}
}

// TestWhoami_NoTraceURLTemplateForAPendingUser: a user awaiting approval may
// learn that it waits, nothing about the deployment.
func TestWhoami_NoTraceURLTemplateForAPendingUser(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "secret-bearer"
	cfg.Env.TraceURLTemplate = "https://grafana.example/explore?traceId={trace_id}"
	srv, _ := makeServer(t, completingProvider(), cfg)
	for _, pending := range []bool{true, false} {
		ctx := auth.WithPrincipal(withUIAuth(context.Background(), uiAuth{pending: pending}), auth.Principal{TenantID: "acme", Subject: "pat"})
		rec := httptest.NewRecorder()
		srv.handleWhoami(rec, httptest.NewRequest("GET", "/v1/_me", nil).WithContext(ctx))
		if has := strings.Contains(rec.Body.String(), "trace_url_template"); has == pending {
			t.Errorf("pending=%v: trace_url_template present=%v: %s", pending, has, rec.Body.String())
		}
	}
}
