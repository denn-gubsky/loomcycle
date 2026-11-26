package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go is pointed at the goproxy and gosum routes (RFC GW §6.3), on both
// engines: GOSUMDB keeps sum.golang.org's own key and only moves its address,
// so go still checks every module against the checksum database.
func TestEgressEnv_PointsGoAtItsRoutes(t *testing.T) {
	env := map[string]string{}
	for _, kv := range egressEnv("http://egress/", "egress") {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for k, v := range map[string]string{
		"GOPROXY": "http://egress/goproxy/",
		"GOSUMDB": "sum.golang.org http://egress/gosum/",
		"GOFLAGS": "-modcacherw",
	} {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

// A container session with egress reads cargo's source replacement from a
// file the worker wrote and mounted read-only at the root of the session,
// which cargo reads for every project under it: nothing is written into the
// workspace, and nothing in the session can change it.
func TestContainer_CargoConfigIsMountedReadOnlyWithEgress(t *testing.T) {
	// A short path: the engine refuses a directory too long for a socket.
	dir, err := os.MkdirTemp("/tmp", "lcg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := testContainer(t, func(o *ContainerOptions) { o.Egress = &ContainerEgress{Dir: dir, HostDir: "/host/e"} })
	a := c.runArgs("ws1", "/w/ws1")
	if !hasPair(a, "-v", "/host/e/k1/"+cargoConfigFile+":"+cargoConfigMount+":ro") {
		t.Fatalf("no read-only cargo config mount: %q", a)
	}
	if cargoConfigMount != "/.cargo/config.toml" {
		t.Fatalf("mounted at %q", cargoConfigMount)
	}
	if err := c.egress.writeStatic(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "k1", cargoConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`[source.crates-io]`, `replace-with = "loomcycle-crates"`, `registry = "sparse+http://egress/crates/"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("cargo config lacks %q:\n%s", want, b)
		}
	}
}

// Without egress there is nothing for cargo to reach, so nothing is mounted.
func TestContainer_NoCargoConfigWithoutEgress(t *testing.T) {
	c := testContainer(t, nil)
	if j := strings.Join(c.runArgs("ws1", "/w/ws1"), " "); strings.Contains(j, cargoConfigMount) {
		t.Fatalf("cargo config without egress: %q", j)
	}
}
