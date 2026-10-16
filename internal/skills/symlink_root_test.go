package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// A skills root that is itself a symlink loads its skills. WalkDir does not
// descend a root that is a symlink, so such a root loaded none, with no
// error. Names and each skill's Path read exactly as they do for the real
// root: the Path is under the root as configured.
func TestLoadSet_ARootThatIsASymlinkLoadsItsSkills(t *testing.T) {
	real := t.TempDir()
	for _, name := range []string{"voice", "doc/redactor"} {
		dir := filepath.Join(real, filepath.FromSlash(name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: d\n---\n\nbody\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	set, err := LoadSet(link)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if names := set.Names(); len(names) != 2 || names[0] != "doc/redactor" || names[1] != "voice" {
		t.Fatalf("names = %v, want [doc/redactor voice]", names)
	}
	sk, _ := set.Get("doc/redactor")
	if want := filepath.Join(link, "doc", "redactor", "SKILL.md"); sk.Path != want {
		t.Errorf("Path = %q, want %q (under the root as configured)", sk.Path, want)
	}
}
