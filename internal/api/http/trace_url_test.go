package http

import (
	"net/http/httptest"
	"strings"
	"testing"
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
