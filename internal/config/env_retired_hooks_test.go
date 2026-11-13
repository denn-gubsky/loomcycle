package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two hooks lists are yaml-only. A retired variable naming an entry the
// yaml list lacks is refused: the hook that entry served would stop being
// honoured with nothing but a log line to say so. One whose every entry the
// yaml already carries is only advised.
func TestRetiredEnvRefusals_HooksListRefusesAnEntryTheYAMLLacks(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		hooks       HooksConfig
		refused     bool
	}{
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: "acme:url-gate", refused: true},
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: "acme:url-gate,beta:gate",
			hooks: HooksConfig{PermitHostWiden: HostWidenPermitConfig{Owners: []string{"acme:url-gate"}}}, refused: true},
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: " acme : url-gate , shared ,",
			hooks: HooksConfig{PermitHostWiden: HostWidenPermitConfig{Owners: []string{"shared", "acme:url-gate", "other:x"}}}},
		// A grant is keyed on (tenant, owner): the same owner under another
		// tenant, or under the shared one, is another grant.
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: "acme:url-gate",
			hooks: HooksConfig{PermitHostWiden: HostWidenPermitConfig{Owners: []string{"url-gate", "beta:url-gate"}}}, refused: true},
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: "url-gate",
			hooks: HooksConfig{PermitHostWiden: HostWidenPermitConfig{Owners: []string{":url-gate"}}}},
		// An entry with no owner granted nothing.
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: " , acme: ,"},
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: ""},

		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: "hooks.internal", refused: true},
		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: "hooks.internal,10.0.0.0/8",
			hooks: HooksConfig{PrivateHostAllowlist: []string{"hooks.internal"}}, refused: true},
		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: " Hooks.Internal. , 10.0.0.0/8 ,",
			hooks: HooksConfig{PrivateHostAllowlist: []string{"10.0.0.0/8", "hooks.internal", "gate.svc"}}},
		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: " , "},
		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: ""},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			environ := []string{tc.name + "=" + tc.value}
			got := retiredEnvRefusals(environ, &Config{Hooks: tc.hooks})
			if refused := len(got) > 0; refused != tc.refused {
				t.Fatalf("refused = %v (%q), want %v", refused, got, tc.refused)
			}
			if tc.refused && !strings.HasPrefix(got[0], tc.name+": ") {
				t.Errorf("refusal does not lead with the variable: %q", got[0])
			}
			if adv := retiredEnvWarnings(environ); len(adv) == 0 {
				t.Errorf("no advisory for %s", tc.name)
			}
		})
	}
}

// The environment no longer adds to either hooks list: the yaml list is what
// the registry and the hook dispatcher get, entry for entry.
func TestLoad_HooksListsComeFromTheYAMLOnly(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default: { model: claude-sonnet-4-6 }
hooks:
  permit_host_widen:
    owners: ["yamltenant:yamlowner"]
  private_host_allowlist: ["hooks.internal"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Entries the yaml already carries: advised, not refused, and not added
	// a second time.
	t.Setenv("LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", "yamltenant:yamlowner")
	t.Setenv("LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", "hooks.internal")
	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Hooks.PermitHostWiden.Owners, []string{"yamltenant:yamlowner"}; !equalStrings(got, want) {
		t.Errorf("PermitHostWiden.Owners = %v, want %v", got, want)
	}
	if got, want := cfg.Hooks.PrivateHostAllowlist, []string{"hooks.internal"}; !equalStrings(got, want) {
		t.Errorf("PrivateHostAllowlist = %v, want %v", got, want)
	}
	advised := strings.Join(cfg.Warnings, "\n")
	for _, name := range []string{"LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST"} {
		if !strings.Contains(advised, name+" is set but no longer read") {
			t.Errorf("no advisory for %s; warnings = %q", name, cfg.Warnings)
		}
	}

	// An entry the yaml lacks stops the load, naming the variable and the
	// yaml key, never the entry.
	t.Setenv("LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", "othertenant:zz-owner")
	t.Setenv("LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", "zz-sidecar.local")
	_, err = Load(yamlPath)
	if err == nil {
		t.Fatal("Load accepted retired hooks variables naming entries the yaml lacks")
	}
	for _, want := range []string{
		"LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", "hooks.permit_host_widen.owners",
		"LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", "hooks.private_host_allowlist",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	for _, value := range []string{"zz-owner", "zz-sidecar"} {
		if strings.Contains(err.Error(), value) {
			t.Errorf("error carries a variable's value %q: %v", value, err)
		}
	}
}
