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

// The refusal says which entry is missing, so the operator does not have to
// diff two lists by eye, that entries are compared as written, and that the
// variable can go if the yaml already grants what is needed. An entry that
// does not look like an owner name, a host or a range is not echoed: a URL
// pasted with its credentials must not reach a log.
func TestRetiredEnvRefusals_HooksListRefusalNamesTheFirstMissingEntry(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		hooks       HooksConfig
		says        []string
		not         []string
	}{
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: "shared, acme:url-gate ,beta:gate",
			hooks: HooksConfig{PermitHostWiden: HostWidenPermitConfig{Owners: []string{"shared"}}},
			says:  []string{`lacks "acme:url-gate"`, "compared as written", "hooks.permit_host_widen.owners", "remove the variable"},
			not:   []string{"beta:gate", `"shared"`}},
		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: "hooks.internal,10.20.0.0/16,gate.svc",
			hooks: HooksConfig{PrivateHostAllowlist: []string{"hooks.internal", "10.0.0.0/8"}},
			says:  []string{`lacks "10.20.0.0/16"`, "compared as written", "wider range", "hooks.private_host_allowlist", "remove the variable"},
			not:   []string{"gate.svc"}},
		{name: "LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST", value: "https://user:hunter2@hooks.internal/cb",
			says: []string{"not shown", "hooks.private_host_allowlist"},
			not:  []string{"hunter2", "user"}},
		{name: "LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS", value: "acme:" + strings.Repeat("x", 300),
			says: []string{"not shown"},
			not:  []string{"xxxxxxxx"}},
	} {
		got := retiredEnvRefusals([]string{tc.name + "=" + tc.value}, &Config{Hooks: tc.hooks})
		if len(got) != 1 {
			t.Fatalf("%s=%s: refusals = %q, want one", tc.name, tc.value, got)
		}
		for _, want := range tc.says {
			if !strings.Contains(got[0], want) {
				t.Errorf("%s: refusal does not say %q: %s", tc.name, want, got[0])
			}
		}
		for _, not := range tc.not {
			if strings.Contains(got[0], not) {
				t.Errorf("%s: refusal carries %q: %s", tc.name, not, got[0])
			}
		}
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

	// An entry the yaml lacks stops the load, naming the variable, the yaml
	// key and the entry: an owner name, a host or a range is not a secret,
	// and it is what the operator has to copy.
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
	for _, entry := range []string{`"othertenant:zz-owner"`, `"zz-sidecar.local"`} {
		if !strings.Contains(err.Error(), entry) {
			t.Errorf("error does not name the missing entry %s: %v", entry, err)
		}
	}
}

// The variable that named extra config layers is gone. Set to anything, the
// files it names are no longer layered, so it is refused; set and empty, it
// named none.
func TestRetiredEnvRefusals_ConfigFilesRefusedWhenItNamesAFile(t *testing.T) {
	for value, refused := range map[string]bool{"/etc/zz-a.yaml:/etc/zz-b.yaml": true, "zz.yaml": true, "": false} {
		environ := []string{"LOOMCYCLE_CONFIG_FILES=" + value}
		got := retiredEnvRefusals(environ, &Config{})
		if (len(got) > 0) != refused {
			t.Errorf("value %q: refused = %v (%q), want %v", value, len(got) > 0, got, refused)
		}
		if layer := RetiredLayerEnvRefusals(environ); (len(layer) > 0) != refused {
			t.Errorf("value %q: the assembly's refusal = %q, want refused = %v", value, layer, refused)
		}
		for _, msg := range got {
			if !strings.HasPrefix(msg, "LOOMCYCLE_CONFIG_FILES: ") || !strings.Contains(msg, "--config") ||
				!strings.Contains(msg, "LOOMCYCLE_CONFIG_DIR") || strings.Contains(msg, "zz") {
				t.Errorf("refusal %q does not name the variable and what to do, or carries the value", msg)
			}
		}
		if adv := retiredEnvWarnings(environ); len(adv) != 1 {
			t.Errorf("value %q: advisories = %q, want one", value, adv)
		}
	}
	// The assembly's check is for this variable alone: a rule that needs the
	// yaml is judged by the load, with the yaml.
	if got := RetiredLayerEnvRefusals([]string{"LOOMCYCLE_SCHEDULER_ENABLED=0"}); len(got) != 0 {
		t.Errorf("the assembly refused a variable that does not name config layers: %q", got)
	}
}
