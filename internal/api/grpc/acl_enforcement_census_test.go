package grpc

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// The RPC gate asks exactly the pairs the enforcement table credits to it
// (RFC FY Part E): each RPC's own question, and for an RPC that carries its
// op, each op's HTTP twin's (enforceOp).
func TestACLEnforcement_RPCsAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	for _, name := range serviceRPCs() {
		if s, ok := authz.GRPCSurface(name); ok {
			asked[s.Action+" "+s.Type] = true
		}
		if !memberpath.ByOp(name) {
			continue
		}
		for _, tw := range memberpath.Twins(name) {
			if s, ok := authz.HTTPSurface(tw.Method, tw.Path); ok {
				asked[s.Action+" "+s.Type] = true
			}
		}
	}
	if len(asked) == 0 {
		t.Fatal("no RPC asked anything: the census is not reading the service")
	}
	table := map[string]bool{}
	for _, p := range authz.PairsAskedAt(authz.SiteGRPCRPC) {
		table[p] = true
		if !asked[p] {
			t.Errorf("the enforcement table credits %q to the RPCs, which never ask it", p)
		}
	}
	var missing []string
	for p := range asked {
		if !table[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("an RPC asks %q, which the enforcement table does not credit to the RPCs", p)
	}
}

// The client address a source_cidr condition reads on gRPC is the
// connection's peer (RFC FY Part E); with no peer there is none.
func TestGRPCPeerIP_IsTheConnectionsRemoteAddress(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 4242}})
	if got := grpcPeerIP(ctx); !got.Equal(net.ParseIP("198.51.100.7")) {
		t.Errorf("grpcPeerIP = %v", got)
	}
	if got := grpcPeerIP(context.Background()); got != nil {
		t.Errorf("no peer: %v, want nil", got)
	}
}

// A peer in auth.grpc_trusted_proxies may name its client in the call's metadata,
// as on HTTP; any other peer is the client whatever its metadata says.
func TestGRPCCallerIP_ReadsATrustedProxysForwardedAddress(t *testing.T) {
	trusted := netip.MustParsePrefix("10.0.0.0/8")
	s := &Server{clientIP: func(p netip.Addr, header func(string) []string) net.IP {
		if !trusted.Contains(p) {
			return net.ParseIP(p.String())
		}
		if v := header("X-Forwarded-For"); len(v) == 1 {
			return net.ParseIP(v[0])
		}
		return net.ParseIP(p.String())
	}}
	call := func(peerIP string) context.Context {
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(peerIP), Port: 1}})
		return metadata.NewIncomingContext(ctx, metadata.Pairs("x-forwarded-for", "198.51.100.9"))
	}
	if got := s.callerIP(call("10.1.2.3")); !got.Equal(net.ParseIP("198.51.100.9")) {
		t.Errorf("behind a trusted proxy: %v, want the forwarded client", got)
	}
	if got := s.callerIP(call("203.0.113.5")); !got.Equal(net.ParseIP("203.0.113.5")) {
		t.Errorf("an untrusted peer: %v, want the peer itself", got)
	}
}
