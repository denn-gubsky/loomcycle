package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEngine_RunArgs_ExposeAttachesAliasNetwork(t *testing.T) {
	e := NewEngine(testCfg(), &fakeRunner{})
	args := e.runArgs("n", openOpts{
		Network: "none", ExposeNetwork: "loom-dev", ExposeAlias: "devsrv",
		TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100, Image: "img:latest",
	})
	if !hasPair(args, "--network", "loom-dev") {
		t.Errorf("exposed session must join the expose network: %v", args)
	}
	if !hasPair(args, "--network-alias", "devsrv") {
		t.Errorf("exposed session must get the --network-alias: %v", args)
	}
	// A container has ONE --network: it must not also be none/bridge.
	if hasPair(args, "--network", "none") || hasPair(args, "--network", "bridge") {
		t.Errorf("exposed session must not also be none/bridge: %v", args)
	}
}

func TestEngine_RunArgs_ExposeWinsOverEgress(t *testing.T) {
	cfg := testCfg()
	cfg.AllowEgress = true
	e := NewEngine(cfg, &fakeRunner{})
	// Both egress and expose requested → expose wins (the expose network already
	// carries egress), so no separate bridge is attached.
	args := e.runArgs("n", openOpts{
		Network: "egress", ExposeNetwork: "loom-dev", ExposeAlias: "app",
		TmpfsMB: 512, CPUs: 1, MemMB: 512, Pids: 100,
	})
	if !hasPair(args, "--network", "loom-dev") || hasPair(args, "--network", "bridge") {
		t.Errorf("expose must win over the egress bridge: %v", args)
	}
}

func TestValidateExposeAlias(t *testing.T) {
	for _, ok := range []string{"devsrv", "app-1", "a", "my-dev-server", "web3"} {
		if err := validateExposeAlias(ok); err != nil {
			t.Errorf("%q should be a valid alias: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "x-", "Dev", "a_b", "a.b", "a/b", strings.Repeat("a", 64)} {
		if err := validateExposeAlias(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestOpen_ExposeGate(t *testing.T) {
	// Gate OFF (no SANDBOX_EXPOSE_NETWORK) → an `expose` request is refused and NO
	// session is created (exposure is never implicit).
	fr := &fakeRunner{}
	cfg := testCfg() // ExposeNetwork == ""
	d := NewDispatcher(cfg, NewEngine(cfg, fr), NewStore(cfg.SessionIdleTTL, cfg.SessionMaxTTL))
	text, isErr, err := d.Call(context.Background(), caller{Principal: "p"}, "sandbox_open", json.RawMessage(`{"expose":"devsrv"}`))
	if err != nil || !isErr {
		t.Fatalf("expose without the operator gate must be refused; text=%q isErr=%v", text, isErr)
	}
	if len(fr.calls) != 0 {
		t.Errorf("no container should be created when expose is refused: %v", fr.calls)
	}

	// Gate ON → the session attaches to the alias network and the response returns
	// the reachable host.
	fr2 := &fakeRunner{}
	cfg2 := testCfg()
	cfg2.ExposeNetwork = "loom-dev"
	d2 := NewDispatcher(cfg2, NewEngine(cfg2, fr2), NewStore(cfg2.SessionIdleTTL, cfg2.SessionMaxTTL))
	text2, isErr2, err := d2.Call(context.Background(), caller{Principal: "p"}, "sandbox_open", json.RawMessage(`{"expose":"devsrv"}`))
	if err != nil || isErr2 {
		t.Fatalf("expose with the gate on should succeed; text=%q isErr=%v", text2, isErr2)
	}
	if len(fr2.calls) == 0 || !hasPair(fr2.calls[0], "--network", "loom-dev") || !hasPair(fr2.calls[0], "--network-alias", "devsrv") {
		t.Errorf("session not attached to the alias network: %v", fr2.calls)
	}
	if !strings.Contains(text2, `"exposed_host":"devsrv"`) {
		t.Errorf("open response should return exposed_host; got %q", text2)
	}

	// A bad alias is rejected even with the gate on.
	fr3 := &fakeRunner{}
	d3 := NewDispatcher(cfg2, NewEngine(cfg2, fr3), NewStore(cfg2.SessionIdleTTL, cfg2.SessionMaxTTL))
	_, isErr3, _ := d3.Call(context.Background(), caller{Principal: "p"}, "sandbox_open", json.RawMessage(`{"expose":"Bad_Alias"}`))
	if !isErr3 || len(fr3.calls) != 0 {
		t.Errorf("an invalid alias must be refused with no container created")
	}
}
