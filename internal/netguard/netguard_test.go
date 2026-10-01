package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsPrivateIP(t *testing.T) {
	private := []string{"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.169.254", "0.0.0.0"}
	for _, s := range private {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("IsPrivateIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1"} {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("IsPrivateIP(%s) = true, want false", s)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("IsPrivateIP(nil) = false, want true (fail-closed)")
	}
}

// TestNewGuardedClient_BlocksLoopback: the always-block client refuses a
// loopback (private) target unless the host is on the allowlist.
func TestNewGuardedClient_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	if resp, err := NewGuardedClient(5*time.Second, nil).Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("guarded client reached loopback %s — guard not applied", srv.URL)
	}
	host, _, _ := net.SplitHostPort(srv.Listener.Addr().String())
	resp, err := NewGuardedClient(5*time.Second, []string{host}).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowlisted host should dial through: %v", err)
	}
	_ = resp.Body.Close()
}

// TestIsPrivateIP_CoversCGNATBenchmarkAndEmbeddedIPv4 pins the ranges the stdlib
// classifiers miss: CGNAT (tailnet peers, Alibaba metadata), benchmarking,
// "this network", reserved, and IPv6 forms that carry a private IPv4 to a
// gateway. The public rows straddle each new range's edges so a too-wide
// prefix fails too.
func TestIsPrivateIP_CoversCGNATBenchmarkAndEmbeddedIPv4(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"100.100.100.200", true}, // Alibaba metadata, inside CGNAT
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"192.0.0.1", true},
		{"0.1.2.3", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"::ffff:100.100.100.200", true}, // v4-mapped
		{"64:ff9b::a9fe:a9fe", true},     // NAT64 of 169.254.169.254
		{"64:ff9b::a00:1", true},         // NAT64 of 10.0.0.1
		{"64:ff9b:1::1", true},           // local-use NAT64
		{"2002:a9fe:a9fe::1", true},      // 6to4 of 169.254.169.254
		{"::ffff:0:a9fe:a9fe", true},     // IPv4-translated
		{"::a9fe:a9fe", true},            // IPv4-compatible

		{"8.8.8.8", false},
		{"2001:4860:4860::8888", false},
		{"100.63.255.255", false}, // just below CGNAT
		{"100.128.0.0", false},    // just above CGNAT
		{"198.17.255.255", false},
		{"198.20.0.0", false},
		{"192.0.1.1", false},
		{"64:ff9b::808:808", false}, // NAT64 of a public IPv4 stays reachable
		{"2002:808:808::1", false},  // 6to4 of a public IPv4
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad fixture %q", c.ip)
		}
		if got := IsPrivateIP(ip); got != c.private {
			t.Errorf("IsPrivateIP(%s) = %v, want %v", c.ip, got, c.private)
		}
	}
}

// TestIsPrivateIP_CoversReservedIPv6AndDocumentationRanges pins the
// special-purpose ranges that are not globally reachable but that the stdlib
// calls global unicast. Each range has a row at both edges and a public
// neighbour just outside it, so a too-narrow or too-wide prefix fails.
func TestIsPrivateIP_CoversReservedIPv6AndDocumentationRanges(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		// Teredo, refused as a whole: a public relay with a private client
		// (here 10.0.0.1, XORed into the last 32 bits) still lands in a LAN.
		{"2001::1", true},
		{"2001:0:4136:e378:8000:63bf:f5ff:fffe", true},
		{"2001:0:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"2001:1::1", false}, // the next /32: port control anycast, public
		{"2000:ffff:ffff:ffff::1", false},

		// Site-local. Its neighbours are link-local below and multicast above,
		// both already private, so only the edges are pinned here.
		{"fec0::1", true},
		{"feff:ffff:ffff:ffff::1", true},

		{"100::1", true}, // discard-only
		{"100::ffff:ffff:ffff:ffff", true},
		{"100:0:0:1::1", false},

		{"2001:db8::1", true}, // documentation
		{"2001:db8:ffff:ffff::1", true},
		{"2001:db7:ffff::1", false},
		{"2001:db9::1", false},
		{"3fff::1", true},
		{"3fff:fff:ffff::1", true},
		{"3fff:1000::1", false},

		{"2001:2::1", true}, // benchmarking
		{"2001:2:0:ffff::1", true},
		{"2001:2:1::1", false},
		{"2001:10::1", true}, // ORCHID
		{"2001:1f:ffff::1", true},
		{"2001:20::1", false}, // ORCHIDv2 is globally reachable

		{"5f00::1", true}, // segment-routing SIDs
		{"5f00:ffff::1", true},
		{"5f01::1", false},

		{"192.0.2.1", true}, // documentation IPv4
		{"198.51.100.1", true},
		{"203.0.113.255", true},
		{"192.0.3.0", false},
		{"198.51.101.0", false},
		{"203.0.112.255", false},
		{"2002:cb00:7101::1", true}, // 6to4 of 203.0.113.1
		{"64:ff9b::c000:201", true}, // NAT64 of 192.0.2.1
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad fixture %q", c.ip)
		}
		if got := IsPrivateIP(ip); got != c.private {
			t.Errorf("IsPrivateIP(%s) = %v, want %v", c.ip, got, c.private)
		}
	}
}

// guardedDialErr dials addr through the guard with a short deadline. The caller
// tells a guard refusal ("blocked: …") from an ordinary network failure, which
// is all an address outside this machine can produce in a test.
func guardedDialErr(t *testing.T, allow []string, addr string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	conn, err := GuardedDialContext(false, allow)(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func isGuardRefusal(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "blocked:")
}

// TestGuardedDial_AllowlistedCGNATHostStillDials: now that CGNAT is private, an
// operator's exact-host entry for a tailnet peer must still exempt it.
func TestGuardedDial_AllowlistedCGNATHostStillDials(t *testing.T) {
	const addr = "100.100.100.200:9"
	if err := guardedDialErr(t, nil, addr); !isGuardRefusal(err) {
		t.Fatalf("unlisted CGNAT address: err = %v, want a guard refusal", err)
	}
	if err := guardedDialErr(t, []string{"100.100.100.200"}, addr); isGuardRefusal(err) {
		t.Fatalf("exact-host entry should exempt the CGNAT peer, got %v", err)
	}
}

// TestGuardedDial_CIDRAllowlistEntryAdmitsTailnetIP: a CIDR entry exempts the
// addresses inside it (a whole tailnet with one entry) and nothing outside it.
func TestGuardedDial_CIDRAllowlistEntryAdmitsTailnetIP(t *testing.T) {
	if err := guardedDialErr(t, []string{"100.64.0.0/10"}, "100.100.100.200:9"); isGuardRefusal(err) {
		t.Fatalf("CIDR entry 100.64.0.0/10 should admit a tailnet IP, got %v", err)
	}

	// A range the guard has always blocked proves the CIDR match end to end
	// with a real response, and that an entry is scoped to its own range.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	if resp, err := NewGuardedClient(5*time.Second, []string{"100.64.0.0/10"}).Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a CIDR entry for another range admitted loopback %s", srv.URL)
	}
	resp, err := NewGuardedClient(5*time.Second, []string{"127.0.0.0/8"}).Get(srv.URL)
	if err != nil {
		t.Fatalf("CIDR entry 127.0.0.0/8 should admit loopback: %v", err)
	}
	_ = resp.Body.Close()
}

// TestValidatePrivateHostAllowlist_RefusesMalformedCIDR: a typo'd range must
// fail at config load, not silently never match; host names pass untouched.
func TestValidatePrivateHostAllowlist_RefusesMalformedCIDR(t *testing.T) {
	if err := ValidatePrivateHostAllowlist([]string{"localhost", "100.64.0.0/10", "fd7a:115c:a1e0::/48", "100.101.102.103/32"}); err != nil {
		t.Fatalf("valid entries refused: %v", err)
	}
	for _, bad := range []string{"100.64.0.0/33", "100.64.0/10", "tailnet/10", "/10"} {
		if err := ValidatePrivateHostAllowlist([]string{"localhost", bad}); err == nil {
			t.Errorf("malformed CIDR %q accepted", bad)
		}
	}
}

// TestIPLiteralAllowed_MatchesOnlyIPLiteralsInCIDREntries: the authoring-time
// helper judges an IP literal the way the dial guard will, and never a name.
func TestIPLiteralAllowed_MatchesOnlyIPLiteralsInCIDREntries(t *testing.T) {
	allow := []string{"100.64.0.0/10", "peer.internal"}
	for host, want := range map[string]bool{
		"100.101.102.103":   true,
		"[100.101.102.103]": true,
		"100.128.0.1":       false,
		"peer.internal":     false, // a name entry is hostAllowed's job
		"10.0.0.1":          false,
	} {
		if got := IPLiteralAllowed(host, allow); got != want {
			t.Errorf("IPLiteralAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}
