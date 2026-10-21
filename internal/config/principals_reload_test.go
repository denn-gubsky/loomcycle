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
