package embedded

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// unitTopLevelKeys is every top-level key a unit built into the binary may
// set. A preset is selected by one word and arrives in a deployment unread,
// so what the shipped ones can carry is a closed list: routing, agents and
// what agents use. Not on it, and so refused by this test: `volumes` (a host
// path), `principals` and the `auth` keys that decide who gets in, `storage`
// and `retention` (where data lives and when it is deleted), `acls`,
// `acl_bindings` and `groups`, `hooks` (it holds the host-widening permits),
// `webhooks`, and anything not thought of yet.
var unitTopLevelKeys = map[string]bool{
	"include": true, // expanded by the layer assembly, never merged
	"models":  true, "provider_priority": true, "tiers": true, "user_tiers": true, "providers": true,
	"agents": true, "skills": true, "channels": true, "scheduled_runs": true,
	"mcp_servers": true, // sandbox: the sidecar's MCP endpoint its agents call
	"metrics":     true, // server: the process sampler
	"auth":        true, // only the sub-keys in unitAuthKeys
}

// unitAuthKeys are the `auth:` sub-keys a unit may set, each with the one
// unit that does and why. Everything else under auth (gates, mode,
// trusted_proxies, client_ip_header, session, lifecycle, login_origins) is
// the operator's alone.
var unitAuthKeys = map[string]string{
	"clients": "loomboard: registers the desktop app as a public sign-in client; it admits no one by itself, a gate still has to",
}

func TestUnits_EveryUnitSetsOnlyKeysAPresetMayCarry(t *testing.T) {
	layers := map[string][]byte{"providers.default": DefaultProviders()}
	for _, u := range Units() {
		layers[u.Name] = u.Data
	}
	if len(layers) < 15 {
		t.Fatalf("only %d embedded layers found; this test would assert nothing", len(layers))
	}
	for name, data := range layers {
		var tree map[string]yaml.Node
		if err := yaml.Unmarshal(data, &tree); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var bad []string
		for key, node := range tree {
			if !unitTopLevelKeys[key] {
				bad = append(bad, key)
				continue
			}
			if key != "auth" {
				continue
			}
			var auth map[string]yaml.Node
			if err := node.Decode(&auth); err != nil {
				t.Errorf("%s: auth: %v", name, err)
				continue
			}
			for sub := range auth {
				if _, ok := unitAuthKeys[sub]; !ok {
					bad = append(bad, "auth."+sub)
				}
			}
		}
		sort.Strings(bad)
		if len(bad) > 0 {
			t.Errorf("%s sets %s: not a key a unit built into the binary may carry (see unitTopLevelKeys before widening)", name, strings.Join(bad, ", "))
		}
	}
}

// The check above must be able to fail: the keys it exists to keep out are
// not on its list.
func TestUnits_TheKeysThatDecideAccessAndDataAreNotOnTheList(t *testing.T) {
	for _, key := range []string{"volumes", "principals", "retention", "storage", "acls", "acl_bindings", "groups", "hooks", "webhooks", "presets"} {
		if unitTopLevelKeys[key] {
			t.Errorf("%q is allowed to units", key)
		}
	}
	for _, sub := range []string{"gates", "mode", "trusted_proxies", "client_ip_header", "login_origins"} {
		if _, ok := unitAuthKeys[sub]; ok {
			t.Errorf("auth.%s is allowed to units", sub)
		}
	}
}
