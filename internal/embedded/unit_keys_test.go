package embedded

import (
	"fmt"
	"reflect"
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
// `acl_bindings`, `acl_modes` and `groups`, `hooks` (it holds the host-widening permits),
// `webhooks`, and anything not thought of yet. `teams` is on it under a
// narrower rule than the API's (unitTeamProblems).
var unitTopLevelKeys = map[string]bool{
	"include": true, // expanded by the layer assembly, never merged
	"models":  true, "provider_priority": true, "tiers": true, "user_tiers": true, "providers": true,
	"agents": true, "skills": true, "channels": true, "scheduled_runs": true,
	"teams":       true, // RFC GS: only what unitTeamProblems allows
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
	for _, key := range []string{"volumes", "principals", "retention", "storage", "acls", "acl_bindings", "acl_modes", "groups", "hooks", "webhooks", "presets"} {
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

// unitTeamProblems is what a unit's teams may not carry (RFC GS), though a
// team created over the API may. A unit arrives in a deployment unread, and a
// team must not be the way around the keys refused above:
//   - local.webhooks — `webhooks` is refused as a unit key;
//   - hooks / tool_hooks, on the team or on a state's handler — they carry
//     endpoints and headers, and `hooks` is refused as a unit key;
//   - definition_file — a unit has no directory, so the path would be read
//     from the operator's, a file the unit did not ship.
//
// local.schedules is allowed, as scheduled_runs is.
func unitTeamProblems(data []byte) ([]string, error) {
	var tree struct {
		Teams map[string]map[string]any `yaml:"teams"`
	}
	if err := yaml.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	var bad []string
	for name, team := range tree.Teams {
		at := "teams." + name
		for _, key := range []string{"definition_file", "hooks"} {
			if _, ok := team[key]; ok {
				bad = append(bad, at+"."+key)
			}
		}
		if local, ok := team["local"].(map[string]any); ok {
			if _, ok := local["webhooks"]; ok {
				bad = append(bad, at+".local.webhooks")
			}
		}
		states, _ := team["states"].([]any)
		for i, st := range states {
			handler, _ := st.(map[string]any)["handler"].(map[string]any)
			for _, key := range []string{"hooks", "tool_hooks"} {
				if _, ok := handler[key]; ok {
					bad = append(bad, fmt.Sprintf("%s.states[%d].handler.%s", at, i, key))
				}
			}
		}
	}
	sort.Strings(bad)
	return bad, nil
}

func TestUnits_AUnitsTeamsCarryNoWebhooksHooksOrFile(t *testing.T) {
	teams := 0
	for _, u := range Units() {
		bad, err := unitTeamProblems(u.Data)
		if err != nil {
			t.Errorf("%s: %v", u.Name, err)
			continue
		}
		if len(bad) > 0 {
			t.Errorf("%s: a unit's team may not carry %s (see unitTeamProblems)", u.Name, strings.Join(bad, ", "))
		}
		var tree struct {
			Teams map[string]any `yaml:"teams"`
		}
		_ = yaml.Unmarshal(u.Data, &tree)
		teams += len(tree.Teams)
	}
	if teams == 0 {
		t.Fatal("no embedded unit carries a team; this test would assert nothing")
	}
}

// The check above must be able to fail: each thing it refuses, written the way
// an author would, is caught.
func TestUnits_TheTeamCheckRefusesEachThingItExistsFor(t *testing.T) {
	bad, err := unitTeamProblems([]byte(`
teams:
  t:
    definition_file: t.json
    hooks: {run_end: [{webhook: {url: "https://example.invalid"}}]}
    local:
      schedules: {nightly: {schedule: "0 1 * * *"}}
      webhooks: {inbound: {}}
    states:
      - state: a
        handler: {kind: agent, agent: x, hooks: {}, tool_hooks: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"teams.t.definition_file", "teams.t.hooks", "teams.t.local.webhooks",
		"teams.t.states[0].handler.hooks", "teams.t.states[0].handler.tool_hooks",
	}
	if !reflect.DeepEqual(bad, want) {
		t.Errorf("refused %v, want %v (and local.schedules allowed)", bad, want)
	}
}
