package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// symlinkRootVolume builds a volume whose ROOT is a symlink to a real
// directory, with, inside it: a file of its own, a subdirectory, a symlinked
// directory and a symlinked file that stay inside the volume, a symlinked
// file that escapes it, and the dynamic root (the binding's fence). It
// returns the symlinked root and the real one.
func symlinkRootVolume(t *testing.T) (root, real string) {
	t.Helper()
	real = realDir(t)
	outside := realDir(t)
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(real, "notes.txt"), "needle at the top\n")
	write(filepath.Join(real, "src", "main.txt"), "needle in src\n")
	write(filepath.Join(outside, "secret.txt"), "needle outside the volume\n")
	write(filepath.Join(real, "dynamic", "_ephemeral", "run-1", "work", "eph.txt"), "needle in another run\n")
	for link, target := range map[string]string{
		filepath.Join(real, "srclink"):    filepath.Join(real, "src"),
		filepath.Join(real, "alias.txt"):  filepath.Join(real, "notes.txt"),
		filepath.Join(real, "escape.txt"): filepath.Join(outside, "secret.txt"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	root = filepath.Join(realDir(t), "vol")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	return root, real
}

func runOnVolume(t *testing.T, tool tools.Tool, root, real string, args map[string]any) tools.Result {
	t.Helper()
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true, Fence: filepath.Join(real, "dynamic")})
	in, _ := json.Marshal(args)
	res, err := tool.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A volume whose root is a symlink is walked like any other. WalkDir does
// not descend a root that is itself a symlink, so Glob and Grep with no
// `path` found nothing in such a volume. Paths are reported relative to the
// volume as before, and symlinks inside it behave as before: a symlinked
// directory is not descended, a symlinked file is listed, one that escapes
// the volume is never opened, and the dynamic root stays fenced.
func TestGlobGrep_WalkAVolumeWhoseRootIsASymlink(t *testing.T) {
	link, real := symlinkRootVolume(t)
	// The real root is the control: whatever it yields is the behaviour the
	// symlinked root must reproduce.
	for _, root := range []string{real, link} {
		g := runOnVolume(t, &Glob{}, root, real, map[string]any{"pattern": "**/*.txt"})
		if g.IsError {
			t.Fatalf("root %s: Glob: %s", root, g.Text)
		}
		got := strings.Fields(g.Text)
		want := map[string]bool{"notes.txt": true, "src/main.txt": true, "alias.txt": true, "escape.txt": true}
		if len(got) != len(want) {
			t.Errorf("root %s: Glob = %q, want exactly %v", root, got, want)
		}
		for _, p := range got {
			if !want[p] {
				t.Errorf("root %s: Glob listed %q, want only %v", root, p, want)
			}
		}

		gr := runOnVolume(t, &Grep{}, root, real, map[string]any{"pattern": "needle", "output_mode": "content"})
		if gr.IsError {
			t.Fatalf("root %s: Grep: %s", root, gr.Text)
		}
		for _, line := range []string{"notes.txt:1:needle at the top", "src/main.txt:1:needle in src", "alias.txt:1:needle at the top"} {
			if !strings.Contains(gr.Text, line) {
				t.Errorf("root %s: Grep = %q, want a line %q", root, gr.Text, line)
			}
		}
		for _, leak := range []string{"outside the volume", "another run", "srclink/"} {
			if strings.Contains(gr.Text, leak) {
				t.Errorf("root %s: Grep = %q, must not contain %q", root, gr.Text, leak)
			}
		}

		// An absolute pattern under the root as the volume names it (the
		// path Context op=self reports) still matches.
		abs := runOnVolume(t, &Glob{}, root, real, map[string]any{"pattern": root + "/src/*.txt"})
		if abs.IsError || strings.TrimSpace(abs.Text) != "src/main.txt" {
			t.Errorf("root %s: Glob with an absolute pattern under the root = %q (error=%v), want src/main.txt", root, abs.Text, abs.IsError)
		}
	}
}
