package engine

import (
	"slices"
	"strings"
	"testing"
)

// Without egress a session keeps P3's confinement: no network, and no
// package manager pointed anywhere.
func TestContainer_ASessionWithoutEgressHasNoNetwork(t *testing.T) {
	c := testContainer(t, nil)
	args := c.runArgs("ws1", "/w/ws1")
	if !hasPair(args, "--network", "none") {
		t.Fatalf("network: %q", args)
	}
	if j := strings.Join(args, " "); strings.Contains(j, "PIP_INDEX_URL") || strings.Contains(j, "--dns") {
		t.Fatalf("egress settings without egress: %q", args)
	}
}

// With egress a session joins its own network, resolves through nothing,
// and has its package managers pointed at the routes.
func TestContainer_ASessionWithEgressJoinsANetworkOfItsOwn(t *testing.T) {
	c := testContainer(t, func(o *ContainerOptions) { o.Egress = &ContainerEgress{Dir: "/e"} })
	a1, a2 := c.runArgs("ws1", "/w/ws1"), c.runArgs("ws2", "/w/ws2")
	if !hasPair(a1, "--network", "loomws-k1-ws1-net") || !hasPair(a2, "--network", "loomws-k1-ws2-net") {
		t.Fatalf("networks: %q / %q", a1, a2)
	}
	if hasPair(a1, "--network", "none") || slices.Contains(a1, "host") || slices.Contains(a1, "bridge") {
		t.Fatalf("a session also on another network: %q", a1)
	}
	if !hasPair(a1, "--dns", "127.0.0.1") {
		t.Errorf("resolver: %q", a1)
	}
	for _, kv := range []string{"PIP_INDEX_URL=http://egress/pypi/simple/", "PIP_TRUSTED_HOST=egress", "npm_config_registry=http://egress/npm/", "npm_config_audit=false", "LOOMCYCLE_EGRESS=http://egress/"} {
		if !hasPair(a1, "--env", kv) {
			t.Errorf("missing --env %s", kv)
		}
	}
	// The probe never has a network.
	if !hasPair(c.hardening("none"), "--network", "none") {
		t.Error("the probe's network")
	}
}

// The gateway is confined as a session is, is on the session's network only,
// and mounts nothing but its own workspace's socket directory and the
// forwarder, read-only.
func TestContainer_TheGatewaySeesOnlyItsOwnSocket(t *testing.T) {
	c := testContainer(t, func(o *ContainerOptions) {
		o.Runtime = "runsc"
		o.Egress = &ContainerEgress{Dir: "/e", HostDir: "/host/e"}
	})
	a := c.gatewayArgs("ws1")
	for _, p := range [][2]string{
		{"--network", "loomws-k1-ws1-net"}, {"--network-alias", "egress"}, {"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"}, {"--user", "1000:1000"}, {"--runtime", "runsc"},
		{"--name", "loomws-k1-ws1-gw"},
	} {
		if !hasPair(a, p[0], p[1]) {
			t.Errorf("missing %s %s in %q", p[0], p[1], a)
		}
	}
	if !slices.Contains(a, "--read-only") {
		t.Error("not read-only")
	}
	var mounts []string
	for i := 0; i+1 < len(a); i++ {
		if a[i] == "-v" {
			mounts = append(mounts, a[i+1])
		}
	}
	want := []string{"/host/e/k1/" + wsDir("ws1") + ":/run/loomgw:ro", "/host/e/k1/gateway.py:/opt/loomgw/gateway.py:ro"}
	if !slices.Equal(mounts, want) {
		t.Fatalf("mounts %q, want %q", mounts, want)
	}
	if wsDir("ws1") == wsDir("ws2") {
		t.Fatal("two workspaces share a socket directory")
	}
	if j := strings.Join(a, " "); strings.Contains(j, "--env") || strings.Contains(j, "-e ") {
		t.Errorf("the gateway gets an environment: %q", a)
	}
}

func TestNewContainer_RefusesAnEgressDirectoryItCannotUse(t *testing.T) {
	for _, eg := range []ContainerEgress{
		{Dir: "relative"},
		{Dir: "/e", HostDir: "relative"},
		{Dir: "/e:x"},
		{Dir: "/" + strings.Repeat("d", 90)},
		{Dir: "/e", Image: "-bad"},
	} {
		eg := eg
		if _, err := NewContainer(ContainerOptions{Image: "img:1", User: "1000:1000", WorkerKey: "k1", Egress: &eg}); err == nil {
			t.Errorf("%+v: accepted", eg)
		}
	}
}
