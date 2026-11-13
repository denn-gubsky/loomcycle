package embedded

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RFC AQ — embedded config presets + agent/skill bundles. The binary ships a
// curated set of config layers (provider/tier PRESETS and agent+skill BUNDLES)
// so any install resolves a sane base and built-in agents without a source
// checkout. They are selected by name (LOOMCYCLE_PRESETS / --preset) and layered
// as the base of the RFC AN config stack, under the operator's thin overlay.
//
// A preset is config with no agents and no secrets: provider/tier/model routing,
// or an app sign-in client (`loomboard` declares auth.clients). A bundle
// additionally carries an agent def + its skills INLINE (the top-level `skills:`
// map, loomcycle PR #559) — a bundle is "a preset that also defines skills."
// Neither carries secrets: only `token_env` names (RFC AO).

//go:embed presets/*.yaml
var presetFS embed.FS

//go:embed bundles/*.yaml
var bundleFS embed.FS

// env.insecure.example — the non-secret env catalogue (the safe half of the
// two-file split, docs/CONFIGURATION.md §9c). Embedded so the binary is the
// single source of truth for "what can I configure," reachable without the
// source checkout (RFC AR's TrueNAS install dialog renders its options from it).
//
//go:embed env.insecure.example
var envInsecureExample []byte

// providers.default.yaml — RFC BF P2a. A `providers:`-ONLY config layer declaring
// every provider the pre-P2a hardcoded resolver built. cmd/loomcycle prepends it
// as the unconditional base of the config stack (unless LOOMCYCLE_NO_DEFAULT_PROVIDERS=1)
// so a config with no `providers:` block resolves providers byte-identically to
// pre-P2a. It is NOT a selectable preset/bundle (not under presets/ or bundles/)
// — it is always the base, never chosen by name.
//
//go:embed providers.default.yaml
var providersDefault []byte

// Unit is one embedded, selectable config layer — a preset or a bundle.
type Unit struct {
	Name        string // selector name (filename without .yaml)
	Kind        string // "preset" or "bundle"
	Description string // first descriptive comment line, for `loomcycle presets`
	Data        []byte // the layer's YAML bytes
	// Includes is the unit's top-level `include:` list: other units layered
	// under this one. Empty for a unit that includes nothing, and for one whose
	// list does not parse (ResolveUnits reports that error).
	Includes []string
}

// units is the embedded registry, built once at package load from the two
// embedded directories. A name must be unique across BOTH kinds (a preset and a
// bundle can't share a name) so the selector is unambiguous.
var units = loadUnits()

func loadUnits() map[string]Unit {
	out := map[string]Unit{}
	scan := func(efs embed.FS, dir, kind string) {
		entries, err := efs.ReadDir(dir)
		if err != nil {
			return // empty/absent dir — no units of this kind
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			data, err := efs.ReadFile(path.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".yaml")
			includes, _ := includesOf(data)
			out[name] = Unit{
				Name:        name,
				Kind:        kind,
				Description: descriptionFromYAML(data),
				Data:        data,
				Includes:    includes,
			}
		}
	}
	scan(presetFS, "presets", "preset")
	scan(bundleFS, "bundles", "bundle")
	return out
}

// descriptionFromYAML pulls a one-line human description from a unit's leading
// comment block: it skips the `# RFC AQ embedded …: <name>` title line and any
// blank comment lines, and returns the first content comment line (trimmed).
func descriptionFromYAML(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			if t == "" {
				continue // leading blank line before the comment block
			}
			break // hit real YAML before any description comment
		}
		body := strings.TrimSpace(strings.TrimPrefix(t, "#"))
		if body == "" || strings.Contains(body, "embedded preset:") || strings.Contains(body, "embedded bundle:") {
			continue
		}
		return body
	}
	return ""
}

// Units returns every embedded unit (presets + bundles), sorted by name. Used by
// `loomcycle presets` and the selector's "available names" error.
func Units() []Unit {
	out := make([]Unit, 0, len(units))
	for _, u := range units {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Show returns a single unit's YAML bytes (for `loomcycle presets show <name>`).
func Show(name string) ([]byte, error) {
	u, ok := units[name]
	if !ok {
		return nil, fmt.Errorf("unknown preset/bundle %q (available: %s)", name, strings.Join(unitNames(), ", "))
	}
	return u.Data, nil
}

// ResolveUnits maps an ordered selection of names to the units to layer, base
// first. A unit that lists others under `include:` is expanded depth-first:
// its includes come before it, in the order written, so its own keys are
// applied last and can override what it includes. Each unit appears once, at
// its first position: `server,chat` where server already includes chat gives
// one chat layer, under server.
//
// An unknown name and an include cycle are errors naming the chain that led
// there (typo protection; a cycle has no order to apply).
func ResolveUnits(names []string) ([]Unit, error) {
	return expandUnits(units, names)
}

// expandUnits is ResolveUnits over an explicit registry (the tests' seam).
func expandUnits(reg map[string]Unit, names []string) ([]Unit, error) {
	out := make([]Unit, 0, len(names))
	done := map[string]bool{}
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		u, ok := reg[name]
		if !ok {
			if len(chain) == 0 {
				return fmt.Errorf("unknown preset/bundle %q (available: %s)", name, strings.Join(registryNames(reg), ", "))
			}
			return fmt.Errorf("unknown preset/bundle %q in an include: (chain: %s) (available: %s)",
				name, strings.Join(append(chain, name), " -> "), strings.Join(registryNames(reg), ", "))
		}
		for _, c := range chain {
			if c == name {
				return fmt.Errorf("preset include cycle: %s", strings.Join(append(chain, name), " -> "))
			}
		}
		if done[name] {
			return nil
		}
		includes, err := includesOf(u.Data)
		if err != nil {
			return fmt.Errorf("preset/bundle %q: %w", name, err)
		}
		// A fresh slice per level: append on a shared backing array would let
		// one branch's chain overwrite a sibling's.
		next := append(append(make([]string, 0, len(chain)+1), chain...), name)
		for _, inc := range includes {
			if err := visit(inc, next); err != nil {
				return err
			}
		}
		done[name] = true
		out = append(out, u)
		return nil
	}
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// includesOf reads a unit's top-level `include:` list. The key is this
// package's alone: the config merge drops it from an embedded layer and
// refuses it in an operator's file.
func includesOf(data []byte) ([]string, error) {
	var head struct {
		Include []string `yaml:"include"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("include: must be a list of preset/bundle names: %w", err)
	}
	for _, n := range head.Include {
		if strings.TrimSpace(n) == "" {
			return nil, fmt.Errorf("include: has an empty name")
		}
	}
	return head.Include, nil
}

// EnvTemplate returns the embedded env.insecure.example (for `loomcycle
// env-template` and RFC AR's install dialog).
func EnvTemplate() []byte { return envInsecureExample }

// DefaultProviders returns the embedded providers.default.yaml bytes — the RFC BF
// P2a unconditional base layer (see providersDefault). cmd/loomcycle wraps it in a
// config.Layer and prepends it under any opt-in preset.
func DefaultProviders() []byte { return providersDefault }

func unitNames() []string { return registryNames(units) }

func registryNames(reg map[string]Unit) []string {
	out := make([]string, 0, len(reg))
	for n := range reg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
