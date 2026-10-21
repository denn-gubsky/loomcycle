package config

import (
	"sync"
	"testing"
)

// A declared principal's token_env is denied for ${...} interpolation so no
// definition can send its bearer elsewhere. That deny used to be written into
// the shared infra-secret set, which the same load then checks token_env
// against — so the second load in a process (a config reload, `-count=2`)
// refused every principal as "one of loomcycle's own infrastructure secrets".

const reloadPrincipalsYAML = `defaults: { provider: anthropic, model: claude-sonnet-4-6 }
principals:
  alice:
    tenant: acme
    subject: alice
    scopes: [runs:read]
    token_env: LOOMCYCLE_RELOADTEST_ALICE_TOKEN
`

func TestLoad_SecondLoadKeepsDeclaredPrincipals(t *testing.T) {
	t.Setenv("LOOMCYCLE_RELOADTEST_ALICE_TOKEN", "lct_reload_alice")
	path := writeCfg(t, reloadPrincipalsYAML)
	for i := 1; i <= 2; i++ {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load #%d: %v", i, err)
		}
		if len(cfg.ResolvedPrincipals) != 1 || cfg.ResolvedPrincipals[0].Principal.Subject != "alice" {
			t.Fatalf("load #%d resolved %+v, want alice", i, cfg.ResolvedPrincipals)
		}
	}
	// The deny still holds after both loads.
	if got := expandEnv("${LOOMCYCLE_RELOADTEST_ALICE_TOKEN}"); got != "${LOOMCYCLE_RELOADTEST_ALICE_TOKEN}" {
		t.Errorf("a principal bearer was interpolated after a second load: %q", got)
	}
}

// A reload resolves principals while the runtime expands ${...} in dynamic MCP
// definitions on other goroutines. Run under -race.
func TestResolvePrincipals_ConcurrentWithExpandIsRaceFree(t *testing.T) {
	t.Setenv("LOOMCYCLE_RACETEST_BOB_TOKEN", "lct_race_bob")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c := &Config{Principals: map[string]PrincipalDef{
				"bob": {Tenant: "acme", Subject: "bob", Scopes: []string{"runs:read"}, TokenEnv: "LOOMCYCLE_RACETEST_BOB_TOKEN"},
			}}
			_ = resolvePrincipals(c)
		}()
		go func() {
			defer wg.Done()
			_ = expandEnv("${LOOMCYCLE_RACETEST_BOB_TOKEN}")
			_ = EnvNameCredentialSafe("LOOMCYCLE_RACETEST_BOB_TOKEN")
		}()
	}
	wg.Wait()
}

// remove is test-only: production never takes a name off the set.
func (s *envNameSet) remove(name string) {
	s.mu.Lock()
	delete(s.m, name)
	s.mu.Unlock()
}

// The YAML text is ${...}-expanded before the principals block is resolved, so
// the FIRST load in a process must already know the principals' token_env
// names, or a static header carries a principal's bearer to the server.
func TestLoad_FirstLoadLeavesAPrincipalBearerLiteral(t *testing.T) {
	const env = "LOOMCYCLE_FIRSTLOAD_CAROL_TOKEN"
	t.Setenv(env, "carol_secret")
	t.Cleanup(func() { principalBearerEnvs.remove(env) })
	cfg, err := Load(writeCfg(t, `defaults: { provider: anthropic, model: claude-sonnet-4-6 }
principals:
  carol:
    tenant: acme
    subject: carol
    scopes: [runs:read]
    token_env: `+env+`
mcp_servers:
  peer:
    transport: http
    url: https://peer.example/mcp
    headers:
      Authorization: "Bearer ${`+env+`}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["peer"].Headers["Authorization"]; got != "Bearer ${"+env+"}" {
		t.Errorf("first load interpolated a principal bearer into an MCP header: %q", got)
	}
}

// Across layers too: the principal declared in a later layer still protects
// its bearer from an earlier layer's text.
func TestLoadLayers_PrincipalInALaterLayerProtectsAnEarlierOne(t *testing.T) {
	const env = "LOOMCYCLE_LAYERS_DAN_TOKEN"
	t.Setenv(env, "dan_secret")
	t.Cleanup(func() { principalBearerEnvs.remove(env) })
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", `defaults: { provider: anthropic, model: claude-sonnet-4-6 }
mcp_servers:
  peer:
    transport: http
    url: https://peer.example/mcp
    headers:
      Authorization: "Bearer ${`+env+`}"
`)
	top := writeYAML(t, dir, "top.yaml", `principals:
  dan:
    tenant: acme
    subject: dan
    scopes: [runs:read]
    token_env: `+env+`
`)
	cfg, err := Load(base, top)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["peer"].Headers["Authorization"]; got != "Bearer ${"+env+"}" {
		t.Errorf("an earlier layer interpolated a later layer's principal bearer: %q", got)
	}
}

// Boot and reload must agree: a check that names a principal's bearer as a
// credential source refuses it on the first load, as it does on a reload.
func TestLoad_FirstLoadRefusesAPrincipalBearerAsACredentialSource(t *testing.T) {
	const env = "LOOMCYCLE_FIRSTLOAD_ERIN_TOKEN"
	t.Setenv(env, "erin_secret")
	t.Cleanup(func() { principalBearerEnvs.remove(env) })
	_, err := Load(writeCfg(t, `defaults: { provider: anthropic, model: claude-sonnet-4-6 }
principals:
  erin:
    tenant: acme
    subject: erin
    scopes: [runs:read]
    token_env: `+env+`
change_subscriptions:
  hook:
    callback_url: https://hooks.example/cb
    secret_env: `+env+`
`))
	if err == nil {
		t.Fatal("first load accepted a principal's bearer as a change_subscriptions secret_env")
	}
}
