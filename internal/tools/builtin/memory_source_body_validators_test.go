package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// The exported body validators are what a snapshot restore runs before it
// writes a memory-backend or document-source def. Each must refuse what the
// tool's create/fork refuses — above all the exfiltration pair: a base_url
// the host would dial plus an api_key_env naming one of loomcycle's own
// infrastructure secrets — and accept what an author could store.
func TestMemorySourceBodyValidators_RefuseWhatAuthoringRefuses(t *testing.T) {
	cases := []struct {
		name     string
		validate func(json.RawMessage) error
		body     string
		wantErr  string // "" = must pass
	}{
		{"backend inprocess", ValidateMemoryBackendDefBody, `{"name":"mb","kind":"inprocess"}`, ""},
		{"backend remote", ValidateMemoryBackendDefBody,
			`{"name":"mb","kind":"remote","config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_PEER_KEY"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_PEER_KEY_{tenant_id}"}}`, ""},
		{"backend infra secret", ValidateMemoryBackendDefBody,
			`{"kind":"remote","config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_AUTH_TOKEN"}}`, "not an allowed credential env var"},
		{"backend infra secret on inprocess", ValidateMemoryBackendDefBody,
			`{"kind":"inprocess","config":{"api_key_env":"LOOMCYCLE_AUTH_TOKEN"}}`, "not an allowed credential env var"},
		{"backend file scheme", ValidateMemoryBackendDefBody,
			`{"kind":"remote","config":{"base_url":"file:///etc/passwd"}}`, "http or https"},
		{"backend no host", ValidateMemoryBackendDefBody,
			`{"kind":"remote","config":{"base_url":"https:///no-host"}}`, "has no host"},
		{"backend retired kind", ValidateMemoryBackendDefBody, `{"kind":"mem9"}`, "unknown kind"},
		{"backend prefix without tenant", ValidateMemoryBackendDefBody,
			`{"kind":"inprocess","tenancy_strategy":{"kind":"shared_key_with_prefix","prefix_pattern":"all"}}`, "{tenant_id}"},
		{"backend not a def", ValidateMemoryBackendDefBody, `{"config":"x"}`, "does not decode"},
		{"backend metadata IP", ValidateMemoryBackendDefBody,
			`{"kind":"remote","config":{"base_url":"http://169.254.169.254/latest/meta-data/"}}`, "metadata address"},
		{"backend env_pattern names the bearer", ValidateMemoryBackendDefBody,
			`{"kind":"remote","config":{"base_url":"https://peer.example"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_AUTH_TOKEN{tenant_id}"}}`, "env var pattern"},

		{"source ok", ValidateDocumentSourceDefBody,
			`{"name":"ds","config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_PEER_KEY"}}`, ""},
		{"source infra secret", ValidateDocumentSourceDefBody,
			`{"config":{"base_url":"https://peer.example","api_key_env":"LOOMCYCLE_AUTH_TOKEN"}}`, "not an allowed credential env var"},
		{"source no base_url", ValidateDocumentSourceDefBody, `{"config":{}}`, "base_url is required"},
		{"source gopher", ValidateDocumentSourceDefBody, `{"config":{"base_url":"gopher://peer.example"}}`, "http or https"},
		{"source prefix tenancy", ValidateDocumentSourceDefBody,
			`{"config":{"base_url":"https://peer.example"},"tenancy_strategy":{"kind":"shared_key_with_prefix"}}`, "key_per_tenant"},
		{"source loopback IPv6", ValidateDocumentSourceDefBody, `{"config":{"base_url":"http://[::1]:8787"}}`, "metadata address"},
		{"source env_pattern outside LOOMCYCLE_", ValidateDocumentSourceDefBody,
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
