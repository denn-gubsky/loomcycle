package egress

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
)

// cratesConfigBody is the crates.io sparse index's config.json as it is served.
const cratesConfigBody = `{"dl":"https://static.crates.io/crates","api":"https://crates.io","auth-required":false}`

// cratesProxy is a proxy whose every dial lands on an upstream serving
// cratesConfigBody at /config.json and an index line anywhere else, whatever
// host the route names.
func cratesProxy(t *testing.T, opt func(*Options)) (*Proxy, *upstream) {
	t.Helper()
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/config.json" {
			_, _ = io.WriteString(w, cratesConfigBody)
			return
		}
		_, _ = io.WriteString(w, `{"name":"serde","dl":"https://static.crates.io/crates"}`)
	})
	addr := up.Listener.Addr().String()
	p, _ := newTestProxy(t, nil, func(o *Options) {
		o.TLSConfig.ServerName = "example.com"
		o.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		if opt != nil {
			opt(o)
		}
	})
	return p, up
}

func cratesBody(t *testing.T, p *Proxy, g Grant, path string) map[string]any {
	t.Helper()
	resp, err := do(t, p, g, Request{Route: "crates", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(readAll(t, resp)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var (
	cratesRoute   = Route{Name: "crates", URL: "https://index.crates.io/"}
	cratesDLRoute = Route{Name: "crates-dl", URL: "https://static.crates.io/"}
)

// The crates index names its download host in config.json, which a session
// cannot reach by name: on an operator's route the core points it at the
// operator's route to that host (RFC GW §6.3, D28), and drops the API, which
// no route publishes through.
func TestProxy_RewritesCratesDLOnAnOperatorRoute(t *testing.T) {
	p, _ := cratesProxy(t, nil)
	m := cratesBody(t, p, grantOf(cratesRoute, cratesDLRoute), "config.json")
	if m["dl"] != "http://egress/crates-dl/crates" {
		t.Fatalf("dl = %v, want the crates-dl route", m["dl"])
	}
	if _, ok := m["api"]; ok {
		t.Fatalf("api kept: %v", m)
	}
	if m["auth-required"] != false {
		t.Fatalf("another field changed: %v", m)
	}
}

// As for PyPI (P4 plan O34): the operator's download route is the target
// whether the workspace holds it or not, so a download without it is refused
// naming the route rather than sent to a host the session cannot resolve.
func TestProxy_RewritesCratesDLToTheOperatorsRouteWhenNotHeld(t *testing.T) {
	p, _ := cratesProxy(t, func(o *Options) { o.CratesDLRoute = "crates-dl" })
	m := cratesBody(t, p, grantOf(cratesRoute), "config.json")
	if m["dl"] != "http://egress/crates-dl/crates" {
		t.Fatalf("dl = %v, want the operator's crates-dl route", m["dl"])
	}
}

// The core changes bytes only for registries it knows, on routes an operator
// wrote: never on a tenant's route, never towards a tenant's route, never an
// answer other than the index's config.json, and never for a grant whose
// links are relative (the host engine: cargo takes no relative dl).
func TestProxy_NeverRewritesCratesOutsideTheOperatorsConfig(t *testing.T) {
	p, _ := cratesProxy(t, nil)
	relative := grantOf(cratesRoute, cratesDLRoute)
	relative.RelativeBase = true
	for _, c := range []struct {
		name  string
		grant Grant
		path  string
	}{
		{"a tenant's index route", grantOf(Route{Name: "crates", URL: "https://index.crates.io/", Tenant: true}, cratesDLRoute), "config.json"},
		{"a tenant's download route only", grantOf(cratesRoute, Route{Name: "crates-dl", URL: "https://static.crates.io/", Tenant: true}), "config.json"},
		{"no download route", grantOf(cratesRoute), "config.json"},
		{"an index file", grantOf(cratesRoute, cratesDLRoute), "se/rd/serde"},
		{"config.json below the root", grantOf(cratesRoute, cratesDLRoute), "x/config.json"},
		{"a relative grant", relative, "config.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := cratesBody(t, p, c.grant, c.path)
			if m["dl"] != "https://static.crates.io/crates" {
				t.Fatalf("rewritten: %v", m)
			}
		})
	}
}

// A config.json the core cannot read is refused, not passed half-understood.
func TestProxy_RefusesAnUnreadableCratesConfig(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"dl": [`)
	})
	addr := up.Listener.Addr().String()
	p, _ := newTestProxy(t, nil, func(o *Options) {
		o.TLSConfig.ServerName = "example.com"
		o.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	})
	_, err := do(t, p, grantOf(cratesRoute, cratesDLRoute), Request{Route: "crates", Path: "config.json"})
	wantRefusal(t, err, http.StatusBadGateway)
}

func TestCratesDLRoute_IsTheOperatorsRouteToTheDownloadHost(t *testing.T) {
	for _, c := range []struct {
		routes []Route
		want   string
	}{
		{nil, ""},
		{[]Route{cratesRoute}, ""},
		{[]Route{cratesDLRoute}, "crates-dl"},
		{[]Route{{Name: "dl", URL: "https://static.crates.io/", Tenant: true}}, ""},
		{[]Route{{Name: "zz", URL: "https://static.crates.io/"}, {Name: "dl", URL: "https://static.crates.io/"}}, "dl"},
	} {
		if got := CratesDLRoute(c.routes); got != c.want {
			t.Errorf("%+v: %q, want %q", c.routes, got, c.want)
		}
	}
}
