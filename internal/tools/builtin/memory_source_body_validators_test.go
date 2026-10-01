package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A restore judges a remote peer's host against THIS host's allowlists, as
// authoring does: an unlisted host is refused, while a def at exactly the url
// the yaml declares for its name — what a row bootstrapped from yaml carries,
// and which dials as operator-authored — is kept. A def that dials nothing
// (inprocess) is not held to the floor, and no config lists no host.
func TestMemorySourceBodyValidators_RefuseAnUnlistedPeerHost(t *testing.T) {
	cfg := &config.Config{
		Env: config.Env{HTTPHostAllowlist: []string{"listed.example"}},
		MemoryBackends: map[string]config.MemoryBackend{
			"mb": {Kind: "remote", Config: config.MemoryBackendConfig{BaseURL: "https://yaml-peer.internal"}},
		},
		DocumentSources: map[string]config.DocumentSource{
			"ds": {Config: config.DocumentSourceConfig{BaseURL: "https://yaml-docs.internal"}},
		},
	}
	const floor = "not in LOOMCYCLE_HTTP_HOST_ALLOWLIST"
	cases := []struct {
		name     string
		validate func(json.RawMessage) error
		body     string
		wantErr  string // "" = must pass
	}{
		{"backend listed", MemoryBackendDefBodyValidator(cfg),
			`{"name":"x","kind":"remote","config":{"base_url":"https://api.listed.example"}}`, ""},
		{"backend unlisted", MemoryBackendDefBodyValidator(cfg),
			`{"name":"x","kind":"remote","config":{"base_url":"https://attacker.example","api_key_env":"LOOMCYCLE_PEER_KEY_GLOBEX"}}`, floor},
		{"backend at its yaml url", MemoryBackendDefBodyValidator(cfg),
			`{"name":"mb","kind":"remote","config":{"base_url":"https://yaml-peer.internal"}}`, ""},
		{"backend at another name's yaml url", MemoryBackendDefBodyValidator(cfg),
			`{"name":"x","kind":"remote","config":{"base_url":"https://yaml-peer.internal"}}`, floor},
		{"backend inprocess dials nothing", MemoryBackendDefBodyValidator(cfg),
			`{"name":"x","kind":"inprocess","config":{"base_url":"https://attacker.example"}}`, ""},
		{"backend with no config", MemoryBackendDefBodyValidator(nil),
			`{"name":"x","kind":"remote","config":{"base_url":"https://api.listed.example"}}`, floor},
		{"source listed", DocumentSourceDefBodyValidator(cfg),
			`{"name":"x","config":{"base_url":"https://listed.example"}}`, ""},
		{"source unlisted", DocumentSourceDefBodyValidator(cfg),
			`{"name":"x","config":{"base_url":"https://attacker.example"}}`, floor},
		{"source at its yaml url", DocumentSourceDefBodyValidator(cfg),
			`{"name":"ds","config":{"base_url":"https://yaml-docs.internal"}}`, ""},
		{"source at another name's yaml url", DocumentSourceDefBodyValidator(cfg),
			`{"name":"x","config":{"base_url":"https://yaml-docs.internal"}}`, floor},
		{"source with no config", DocumentSourceDefBodyValidator(nil),
			`{"name":"x","config":{"base_url":"https://listed.example"}}`, floor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.validate(json.RawMessage(c.body))
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("accepted; want %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("error = %v, want it to say %q", err, c.wantErr)
			}
		})
	}
}

// The exported body validators are what a snapshot restore runs before it
// writes a memory-backend or document-source def. Each must refuse what the
// tool's create/fork refuses — above all the exfiltration pair: a base_url
// the host would dial plus an api_key_env naming one of loomcycle's own
// infrastructure secrets — and accept what an author could store.
func TestMemorySourceBodyValidators_RefuseWhatAuthoringRefuses(t *testing.T) {
	cfg := &config.Config{Env: config.Env{HTTPHostAllowlist: []string{"peer.example"}}}
	mbv := MemoryBackendDefBodyValidator(cfg)
	dsv := DocumentSourceDefBodyValidator(cfg)
	cases := []struct {
		name     string
		validate func(json.RawMessage) error
		body     string
		wantErr  string // "" = must pass
	}{
		{"backend inprocess", mbv, `{"name":"mb","kind":"inprocess"}`, ""},
		{"backend remote", mbv,
			`{"name":"mb","kind":"remote","config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_PEER_KEY"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_PEER_KEY_{tenant_id}"}}`, ""},
		{"backend infra secret", mbv,
			`{"kind":"remote","config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_AUTH_TOKEN"}}`, "not an allowed credential env var"},
		{"backend infra secret on inprocess", mbv,
			`{"kind":"inprocess","config":{"api_key_env":"LOOMCYCLE_AUTH_TOKEN"}}`, "not an allowed credential env var"},
		{"backend file scheme", mbv,
			`{"kind":"remote","config":{"base_url":"file:///etc/passwd"}}`, "http or https"},
		{"backend no host", mbv,
			`{"kind":"remote","config":{"base_url":"https:///no-host"}}`, "has no host"},
		{"backend retired kind", mbv, `{"kind":"mem9"}`, "unknown kind"},
		{"backend prefix without tenant", mbv,
			`{"kind":"inprocess","tenancy_strategy":{"kind":"shared_key_with_prefix","prefix_pattern":"all"}}`, "{tenant_id}"},
		{"backend not a def", mbv, `{"config":"x"}`, "does not decode"},
		{"backend metadata IP", mbv,
			`{"kind":"remote","config":{"base_url":"http://169.254.169.254/latest/meta-data/"}}`, "metadata address"},
		{"backend stored-credential reference", mbv,
			`{"name":"mb","kind":"remote","config":{"base_url":"https://peer.example","api_key_env":"$cred:peer_key"}}`, ""},
		{"backend reference inside text", mbv,
			`{"kind":"remote","config":{"base_url":"https://peer.example","api_key_env":"Bearer $cred:peer_key"}}`, "not an allowed credential env var"},
		{"backend env_pattern names the bearer", mbv,
			`{"kind":"remote","config":{"base_url":"https://peer.example"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_AUTH_TOKEN{tenant_id}"}}`, "env var pattern"},

		{"source ok", dsv,
			`{"name":"ds","config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_PEER_KEY"}}`, ""},
		{"source infra secret", dsv,
			`{"config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_AUTH_TOKEN"}}`, "not an allowed credential env var"},
		{"source stored-credential reference", dsv,
			`{"name":"ds","config":{"base_url":"https://peer.example","api_key_env":"$cred:peer_key"}}`, ""},
		{"source no base_url", dsv, `{"config":{}}`, "base_url is required"},
		{"source gopher", dsv, `{"config":{"base_url":"gopher://peer.example"}}`, "http or https"},
		{"source prefix tenancy", dsv,
			`{"config":{"base_url":"https://peer.example"},"tenancy_strategy":{"kind":"shared_key_with_prefix"}}`, "key_per_tenant"},
		{"source loopback IPv6", dsv, `{"config":{"base_url":"http://[::1]:8787"}}`, "metadata address"},
		{"source env_pattern outside LOOMCYCLE_", dsv,
			`{"config":{"base_url":"https://peer.example"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"PEER_{tenant_id}_KEY"}}`, "env var pattern"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.validate(json.RawMessage(c.body))
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("refused a body an author could store: %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("accepted a body authoring refuses (want %q)", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("error = %v, want it to say %q", err, c.wantErr)
			}
		})
	}
}
