package config

import (
	"reflect"
	"testing"
)

func TestIsLoopbackListenAddr_OnlyLoopbackLiteralsAndLocalhost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8787":        true,
		"127.1.2.3:8787":        true, // all of 127.0.0.0/8
		"[::1]:8787":            true,
		"localhost:8787":        true,
		"LOCALHOST:8787":        true,
		"[::ffff:127.0.0.1]:80": true,
		":8787":                 false, // empty host binds every interface
		"0.0.0.0:8787":          false,
		"[::]:8787":             false,
		"192.168.0.77:8787":     false,
		"loomcycle.local:8787":  false, // a hostname: what it resolves to is unknown here
		"127.0.0.1":             false, // no port: unparseable, fails closed
		"":                      false,
	}
	for addr, want := range cases {
		if got := IsLoopbackListenAddr(addr); got != want {
			t.Errorf("IsLoopbackListenAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestNonLoopbackListeners_NamesEachPublicListener(t *testing.T) {
	cases := []struct {
		name         string
		listen, grpc string
		want         []string
	}{
		{"both loopback", "127.0.0.1:8787", "127.0.0.1:8788", nil},
		{"gRPC off", "127.0.0.1:8787", "", nil},
		{"HTTP public", "0.0.0.0:8787", "", []string{"LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787"}},
		{"gRPC on all interfaces", "127.0.0.1:8787", ":8788", []string{"LOOMCYCLE_GRPC_ADDR=:8788"}},
		{"both public", ":8787", "[::]:8788", []string{"LOOMCYCLE_LISTEN_ADDR=:8787", "LOOMCYCLE_GRPC_ADDR=[::]:8788"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Env{ListenAddr: tc.listen, GrpcAddr: tc.grpc}
			if got := e.NonLoopbackListeners(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NonLoopbackListeners = %v, want %v", got, tc.want)
			}
		})
	}
}
