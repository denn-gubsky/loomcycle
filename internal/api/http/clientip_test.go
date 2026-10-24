package http

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func TestSourceIP_TrustsOnlyAConfiguredProxysWord(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.1/32")}
	cf := config.AuthPolicy{TrustedProxies: proxies, ClientIPHeader: "Cf-Connecting-Ip"}
	xff := config.AuthPolicy{TrustedProxies: proxies}
	none := config.AuthPolicy{}
	cases := []struct {
		name   string
		policy config.AuthPolicy
		peer   string
		hdr    map[string][]string
		want   string
	}{
		{"no proxies: the peer, whatever it says", none, "203.0.113.9:1234", map[string][]string{"X-Forwarded-For": {"1.1.1.1"}, "Cf-Connecting-Ip": {"1.1.1.1"}}, "203.0.113.9"},
		{"untrusted peer spoofing the client header", cf, "203.0.113.9:1234", map[string][]string{"Cf-Connecting-Ip": {"1.1.1.1"}}, "203.0.113.9"},
		{"untrusted peer spoofing X-Forwarded-For", xff, "203.0.113.9:1234", map[string][]string{"X-Forwarded-For": {"1.1.1.1"}}, "203.0.113.9"},
		{"trusted proxy's client header", cf, "10.1.2.3:1234", map[string][]string{"Cf-Connecting-Ip": {"198.51.100.7"}}, "198.51.100.7"},
		{"trusted proxy, header sent twice", cf, "10.1.2.3:1234", map[string][]string{"Cf-Connecting-Ip": {"198.51.100.7", "1.1.1.1"}}, "10.1.2.3"},
		{"trusted proxy, header not an address", cf, "10.1.2.3:1234", map[string][]string{"Cf-Connecting-Ip": {"1.1.1.1, 2.2.2.2"}}, "10.1.2.3"},
		{"X-Forwarded-For: right-most hop that is not a proxy", xff, "127.0.0.1:1234", map[string][]string{"X-Forwarded-For": {"6.6.6.6, 198.51.100.7, 10.9.9.9"}}, "198.51.100.7"},
		{"X-Forwarded-For across two header lines", xff, "127.0.0.1:1234", map[string][]string{"X-Forwarded-For": {"6.6.6.6", "198.51.100.8"}}, "198.51.100.8"},
		{"X-Forwarded-For with a malformed hop", xff, "127.0.0.1:1234", map[string][]string{"X-Forwarded-For": {"198.51.100.7, junk"}}, "127.0.0.1"},
		{"trusted proxy with no header", xff, "127.0.0.1:1234", nil, "127.0.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = c.peer
		for k, vs := range c.hdr {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		if got := sourceIP(c.policy, r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
