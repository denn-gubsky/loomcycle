package config

import (
	"strings"
	"testing"
)

// A proxy list that trusts every address lets any peer name its own source
// address — which decides throttling, audit and ACL source_cidr rules — so
// it is refused at load, as the header gate's trusted_proxy_cidrs is.
func TestResolveAuth_ATrustedProxyCatchAllIsRefused(t *testing.T) {
	for name, a := range map[string]AuthConfig{
		"ipv4 catch-all":      {TrustedProxies: []string{"10.0.0.1", "0.0.0.0/0"}, ClientIPHeader: "CF-Connecting-IP"},
		"ipv6 catch-all":      {TrustedProxies: []string{"::/0"}},
		"v4-mapped catch-all": {TrustedProxies: []string{"::ffff:0.0.0.0/96"}},
		"grpc ipv4 catch-all": {GRPCTrustedProxies: []string{"0.0.0.0/0"}},
		"grpc ipv6 catch-all": {GRPCTrustedProxies: []string{"::/0"}},
	} {
		err := resolveAuth(&Config{Auth: a})
		if err == nil || !strings.Contains(err.Error(), "trusts every address") {
			t.Errorf("%s: err = %v, want a refusal naming the catch-all", name, err)
		}
	}
}

// A list broader than a /16 (IPv4) or a /48 (IPv6) loads, with a warning: it
// is usually a private range typed for a single proxy address.
func TestResolveAuth_ABroadTrustedProxyRangeWarns(t *testing.T) {
	c := &Config{Auth: AuthConfig{TrustedProxies: []string{"10.0.0.0/8", "172.18.0.0/16"}, GRPCTrustedProxies: []string{"2001:db8::/32"}}}
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.Warnings, "\n")
	for _, want := range []string{"auth.trusted_proxies", "10.0.0.0/8", "auth.grpc_trusted_proxies", "2001:db8::/32"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings do not mention %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "172.18.0.0/16") {
		t.Errorf("a /16 warned:\n%s", got)
	}
}
