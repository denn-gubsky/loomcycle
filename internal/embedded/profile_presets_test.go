package embedded

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// profilePresets are the presets that stand for a whole deployment shape.
// For each: the stack it expands to, and the top-level keys it may set itself.
//
// The key list is an allowlist on purpose. A profile is selected by one word,
// so whatever it sets arrives in a deployment unread. It may tune what the
// runtime already does (metrics). It may not name a host path (volumes), say
// who gets in (auth, principals, acls), move or delete data (storage,
// retention), or carry a credential. Adding a key here is a decision to make
// that key a default for everyone who selects the profile.
var profilePresets = map[string]struct {
	stack   string
	ownKeys []string
}{
	"personal": {stack: "base,local,chat,document-agent,personal"},
}

func TestProfilePresets_ExpandToTheirDocumentedStack(t *testing.T) {
	for name, want := range profilePresets {
		got, err := ResolveUnits([]string{name})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if names(got) != want.stack {
			t.Errorf("%s expands to %s, want %s", name, names(got), want.stack)
		}
	}
}

func TestProfilePresets_SetOnlyTheKeysAllowedToAProfile(t *testing.T) {
	for name, want := range profilePresets {
		data, err := Show(name)
		if err != nil {
			t.Fatal(err)
		}
		var tree map[string]yaml.Node
		if err := yaml.Unmarshal(data, &tree); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := tree["include"]; !ok {
			t.Errorf("%s has no include:, so it is not a profile", name)
		}
		delete(tree, "include")
		var got []string
		for k := range tree {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want.ownKeys, ",") {
			t.Errorf("%s sets top-level keys %v, want exactly %v (see profilePresets before widening)", name, got, want.ownKeys)
		}
	}
}

// Selecting a profile together with something it already includes layers that
// unit once.
func TestProfilePresets_SelectedWithAnIncludedUnitLayersItOnce(t *testing.T) {
	for name, want := range profilePresets {
		got, err := ResolveUnits([]string{name, "chat"})
		if err != nil {
			t.Fatalf("%s,chat: %v", name, err)
		}
		if names(got) != want.stack {
			t.Errorf("%s,chat expands to %s, want %s", name, names(got), want.stack)
		}
	}
}
