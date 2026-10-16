package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A static volume is fenced only when its root is the dynamic root or above
// it. A sibling layout (the dynamic root beside the volume) and a volume
// inside the dynamic root (a dynamic volume's own root) are not fenced, so
// those configs keep working unchanged.
func TestStaticVolumeFence_OnlyForARootAtOrAboveTheDynamicRoot(t *testing.T) {
	top := realDir(t)
	dyn := filepath.Join(top, "work", "dynamic")
	sibling := filepath.Join(top, "other")
	inside := filepath.Join(dyn, "acme", "repo")
	for _, d := range []string{dyn, sibling, inside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Volumes: map[string]config.Volume{"pool": {Path: dyn, DynamicRoot: true}}}
	for root, want := range map[string]string{
		top:                              dyn,
		filepath.Join(top, "work"):       dyn,
		dyn:                              dyn,
		dyn + string(filepath.Separator): dyn,
		sibling:                          "",
		inside:                           "",
	} {
		if got := StaticVolumeFence(cfg, root); got != want {
			t.Errorf("StaticVolumeFence(%q) = %q, want %q", root, got, want)
		}
	}
	if got := StaticVolumeFence(&config.Config{}, top); got != "" {
		t.Errorf("no dynamic root configured: fence = %q, want none", got)
	}
}

// The fence holds when the volume's root is reached through a symlink (the
// walk's paths are then not real) and when a symlink inside the volume points
// into the fence.
func TestVolumeFence_HoldsThroughSymlinks(t *testing.T) {
	top := realDir(t)
	dyn := filepath.Join(top, "dynamic")
	eph := filepath.Join(dyn, "_ephemeral", "run-1", "work")
	if err := os.MkdirAll(eph, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eph, "secret.txt"), []byte("needle in another run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "notes.txt"), []byte("needle of my own\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(eph, filepath.Join(top, "peek")); err != nil {
		t.Fatal(err)
	}
	// The root's PARENT is a symlink: WalkDir descends the root, and the
	// paths it yields are not real. (A root that is itself a symlink is not
	// walked at all, fence or not.)
	link := filepath.Join(realDir(t), "parent")
	if err := os.Symlink(filepath.Dir(top), link); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(link, filepath.Base(top))
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true, Fence: dyn})
	call := func(tool tools.Tool, args map[string]any) tools.Result {
		in, _ := json.Marshal(args)
		res, err := tool.Execute(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := call(&Glob{}, map[string]any{"pattern": "**/*.txt"}); res.IsError || strings.Contains(res.Text, "secret") || !strings.Contains(res.Text, "notes.txt") {
		t.Errorf("Glob through a symlinked root = %q (error=%v)", res.Text, res.IsError)
	}
	if res := call(&Grep{}, map[string]any{"pattern": "needle", "output_mode": "content"}); res.IsError || strings.Contains(res.Text, "another") || !strings.Contains(res.Text, "of my own") {
		t.Errorf("Grep through a symlinked root and a symlink into the fence = %q (error=%v)", res.Text, res.IsError)
	}
	if res := call(&Read{}, map[string]any{"path": "peek/secret.txt"}); !res.IsError || strings.Contains(res.Text, "needle") {
		t.Errorf("Read through a symlink into the fence = %q (error=%v)", res.Text, res.IsError)
	}
	if res := call(&Write{}, map[string]any{"path": "peek/x.txt", "content": "x"}); !res.IsError {
		t.Errorf("Write through a symlink into the fence = %q", res.Text)
	}
	if _, err := os.Stat(filepath.Join(eph, "x.txt")); err == nil {
		t.Error("a write through a symlink landed in the fence")
	}
	if res := call(&Read{}, map[string]any{"path": "notes.txt"}); res.IsError || res.Text != "needle of my own\n" {
		t.Errorf("Read of the volume's own file = %q (error=%v)", res.Text, res.IsError)
	}
}
