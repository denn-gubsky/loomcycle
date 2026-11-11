package otel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// What a tenant learns of a failed export is a coarse category: never an
// address the name resolved to, the guard's wording, a network error's text or
// the collector's answer. Otherwise the test is a network oracle into
// wherever the endpoint points.
func TestTestTenantExport_AnswersWithCoarseCategoriesOnly(t *testing.T) {
	clearOTELEnv(t)
	refusing := newFakeCollector(t)
	refusing.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "internal host db-7.corp 10.1.2.3 says no")
		return true
	}
	hang := newFakeCollector(t)
	release := make(chan struct{})
	defer close(release)
	hang.handler = func(http.ResponseWriter, *http.Request) bool { <-release; return true }
	closed := newFakeCollector(t)
	closedURL := closed.srv.URL
	closed.srv.Close()

	cases := map[string]struct {
		d    TenantDestination
		want string
	}{
		"http status": {dest(refusing, nil), "http_401"},
		"guard": {TenantDestination{Endpoint: "http://intranet.example:4318", Protocol: ProtocolHTTPProtobuf,
			Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("blocked: intranet.example has no public addresses (got 1 private)")
			}}, "blocked"},
		"dns": {TenantDestination{Endpoint: "http://nope.example:4318", Protocol: ProtocolHTTPProtobuf,
			Dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.DNSError{Err: "no such host", Name: "nope.example", IsNotFound: true}
			}}, "dns"},
		"connect": {TenantDestination{Endpoint: closedURL, Protocol: ProtocolHTTPProtobuf, Dial: plainDial}, "connect"},
		"timeout": {dest(hang, nil), "timeout"},
	}
	for name, c := range cases {
		err := TestTenantExport(context.Background(), "acme", c.d, "1", 500*time.Millisecond)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want exactly %q", name, err, c.want)
		}
	}
}

// The status's last error is the same coarse category.
func TestTenantRouter_StatusLastErrorIsACategory(t *testing.T) {
	clearOTELEnv(t)
	c := newFakeCollector(t)
	c.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "secret body 10.9.8.7")
		return true
	}
	r := fastRouter(TenantRouterConfig{})
	r.SetDestination("acme", dest(c, nil))
	tp := routedProvider(t, r)
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "acme")))
	s.End()
	waitFor(t, "the failure", func() bool { return r.Status("acme").ExportFailures == 1 })
	if got := r.Status("acme").LastError; got != "http_403" {
		t.Errorf("last_error = %q, want http_403", got)
	}
}

// A test export takes one of the export slots every tenant shares, so tests
// cannot add exports beyond the cap: with every slot held it answers busy.
func TestTenantRouter_ATestExportTakesASharedSlot(t *testing.T) {
	clearOTELEnv(t)
	hole := newFakeCollector(t)
	release := make(chan struct{})
	hole.handler = func(http.ResponseWriter, *http.Request) bool { <-release; return true }
	ok := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{MaxConcurrentExports: 1, ExportTimeout: 5 * time.Second})
	r.SetDestination("hole", dest(hole, nil))
	tp := routedProvider(t, r)
	t.Cleanup(func() { close(release) })
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, "hole")))
	s.End()
	time.Sleep(100 * time.Millisecond) // the hole's export holds the one slot
	err := r.Test(context.Background(), "ok", dest(ok, nil), 300*time.Millisecond)
	if err == nil || err.Error() != "busy" {
		t.Errorf("a test with every slot held: %v, want busy", err)
	}
	if n := len(ok.got()); n != 0 {
		t.Errorf("the test export went out past the cap: %d spans", n)
	}
	if strings.Contains(r.Status("ok").LastError, "busy") {
		t.Error("a test touched the status of a tenant")
	}
}
