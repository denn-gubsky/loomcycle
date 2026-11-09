package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// TestRemotePeerPrivateHosts_OperatorPeerAtALinkLocalIPStaysReachable: an
// operator-declared peer keeps its own host. When that host is an IP literal
// the operator named the exact address, so it is vouched as that address (a
// CIDR entry); a host-name entry would not cover a link-local or metadata
// address and the peer would be unreachable.
func TestRemotePeerPrivateHosts_OperatorPeerAtALinkLocalIPStaysReachable(t *testing.T) {
	cfg := &config.Config{}
	for _, c := range []struct{ baseURL, host string }{
		{"http://169.254.10.10:8787", "169.254.10.10"},
		{"http://[fd00:ec2::254]:8787", "fd00:ec2::254"},
	} {
		list := remotePeerPrivateHosts(cfg, c.baseURL, lookup.OriginOperator)
		if !netguard.IPLiteralAllowed(c.host, list) {
			t.Errorf("%s: allowlist %v does not admit the operator's own peer address", c.baseURL, list)
		}
	}
	// A name stays a name entry: whoever controls its DNS still cannot point
	// it at a link-local address.
	if got := remotePeerPrivateHosts(cfg, "https://peer.example:8787", lookup.OriginOperator); len(got) != 1 || got[0] != "peer.example" {
		t.Errorf("hostname peer allowlist = %v, want [peer.example]", got)
	}
}

// TestRemotePeerPrivateHosts_CIDROnlyVouchesNoPeerName: under cidr_only no
// private address is admitted on the say-so of a DNS answer, so an operator
// peer named by host name is not vouched for by that name; it gets the
// operator's CIDR list instead. A peer named by IP literal keeps its address.
func TestRemotePeerPrivateHosts_CIDROnlyVouchesNoPeerName(t *testing.T) {
	cfg := &config.Config{}
	cfg.Env.HTTPPrivateHostAllowlistMode = netguard.PrivateHostAllowlistModeCIDROnly
	cfg.Env.HTTPPrivateHostAllowlist = []string{"10.0.0.0/8"}
	got := remotePeerPrivateHosts(cfg, "https://peer.example:8787", lookup.OriginOperator)
	for _, e := range got {
		if e == "peer.example" {
			t.Fatalf("cidr_only: allowlist %v vouches for the peer by name", got)
		}
	}
	if len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("cidr_only: allowlist = %v, want the operator's CIDR list", got)
	}
	if list := remotePeerPrivateHosts(cfg, "http://10.1.2.3:8787", lookup.OriginOperator); !netguard.IPLiteralAllowed("10.1.2.3", list) {
		t.Errorf("cidr_only: an IP-literal peer lost its address: %v", list)
	}
}
