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
