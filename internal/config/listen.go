package config

import (
	"net"
	"strings"
)

// IsLoopbackListenAddr reports whether a listen address ("host:port") binds
// only the loopback interface: a 127.0.0.0/8 or ::1 literal, or "localhost".
//
// An empty host (":8787"), 0.0.0.0 and :: bind every interface. Any other
// hostname is treated as reaching beyond loopback too, since what it resolves
// to is not known here. An address that does not parse is treated the same
// way, so a check built on this fails closed.
func IsLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NonLoopbackListeners names each listener the server opens on an address
// beyond loopback, as "ENV_VAR=addr": the HTTP listener always, the gRPC
// listener when it is set. These are the only sockets that serve the
// authenticated routes — the A2A, webhook and metrics surfaces ride the HTTP
// listener (A2A's gRPC binding rides the gRPC one), and the OAuth-dev callback
// binds 127.0.0.1 itself.
func (e *Env) NonLoopbackListeners() []string {
	var out []string
	if !IsLoopbackListenAddr(e.ListenAddr) {
		out = append(out, "LOOMCYCLE_LISTEN_ADDR="+e.ListenAddr)
	}
	if e.GrpcAddr != "" && !IsLoopbackListenAddr(e.GrpcAddr) {
		out = append(out, "LOOMCYCLE_GRPC_ADDR="+e.GrpcAddr)
	}
	return out
}
