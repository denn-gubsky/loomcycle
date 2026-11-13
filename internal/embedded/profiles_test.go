package embedded

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The config `init` writes and the example an operator copies are one text in
// two places. A difference means one of them was edited alone.
func TestProfileYAML_MatchesTheExampleAnOperatorCopies(t *testing.T) {
	profiles := Profiles()
	if len(profiles) < 2 {
		t.Fatalf("Profiles() = %v, want personal and server", profiles)
	}
	for _, name := range profiles {
		embeddedCopy, err := ProfileYAML(name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "examples", "profiles", name, "loomcycle.yaml")
		example, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("profile %q has no example: %v", name, err)
		}
		if !bytes.Equal(embeddedCopy, example) {
			t.Errorf("internal/embedded/profiles/%s.yaml and %s differ; edit the example, then copy it over the embedded file", name, path)
		}
	}
}

// Each profile is used with the preset of its own name, and the default one
// exists.
func TestProfiles_EachHasAPresetOfItsName(t *testing.T) {
	foundDefault := false
	for _, name := range Profiles() {
		if name == DefaultProfile {
			foundDefault = true
		}
		u, ok := units[name]
		if !ok || u.Kind != "preset" || len(u.Includes) == 0 {
			t.Errorf("profile %q has no preset of that name that includes a stack", name)
		}
	}
	if !foundDefault {
		t.Errorf("DefaultProfile %q is not among %v", DefaultProfile, Profiles())
	}
	if _, err := ProfileYAML("nope"); err == nil {
		t.Error("ProfileYAML of an unknown name returned no error")
	}
}
