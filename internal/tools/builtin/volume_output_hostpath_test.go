package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// symlinkedVolume makes a volume whose bound root is a symbolic link to the
// real directory, the way an operator mounts one through a link. It returns
// the bound root and every host form of it (the link, its real target, and
// both of their parents), none of which a tool result may name.
func symlinkedVolume(t *testing.T) (root string, hostForms []string) {
	t.Helper()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "link")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, []string{root, real, resolved, base, filepath.Dir(resolved)}
}

// assertNamesCallerPath fails unless res succeeded, names want as the caller
// wrote it, and names no host form of the volume. A tool result reaches the
// model and the run's transcript; a host path there says where the volume
// lives on the server.
func assertNamesCallerPath(t *testing.T, what string, res tools.Result, want string, hostForms []string) {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s: refused: %q", what, res.Text)
	}
	for _, h := range hostForms {
		if strings.Contains(res.Text, h) {
			t.Errorf("%s: result names a host path (%s): %q", what, h, res.Text)
		}
	}
	if !strings.Contains(res.Text, want) {
		t.Errorf("%s: result does not name the caller's path %q: %q", what, want, res.Text)
	}
}

func TestWriteEditNotebookEdit_ResultNamesCallerPathNotHostPath(t *testing.T) {
	root, hostForms := symlinkedVolume(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})

	res := execTool(t, &Write{}, ctx, map[string]any{"path": "sub/a.txt", "content": "alpha beta"})
	assertNamesCallerPath(t, "write", res, "sub/a.txt", hostForms)
	if res.Text != "wrote 10 bytes to sub/a.txt" {
		t.Errorf("write result = %q", res.Text)
	}

	res = execTool(t, &Edit{}, ctx, map[string]any{"path": "sub/a.txt", "old_string": "beta", "new_string": "gamma"})
	assertNamesCallerPath(t, "edit", res, "sub/a.txt", hostForms)
	if res.Text != "edited sub/a.txt (1 replacement)" {
		t.Errorf("edit result = %q", res.Text)
	}

	if err := os.WriteFile(filepath.Join(root, "sub", "nb.ipynb"), []byte(fixtureNotebook), 0o644); err != nil {
		t.Fatal(err)
	}
	res = execTool(t, &NotebookEdit{}, ctx, map[string]any{"file_path": "sub/nb.ipynb", "cell_id": "aaaaaaaa", "source": "print(2)", "mode": "replace"})
	assertNamesCallerPath(t, "notebook", res, "sub/nb.ipynb", hostForms)
	if res.Text != "notebook replace: sub/nb.ipynb (2 cells)" {
		t.Errorf("notebook result = %q", res.Text)
	}
}

// A path written with "./" or a symlink inside the volume is echoed as
// written, not cleaned or resolved: the resolved form would be the link's
// host target.
func TestWriteEdit_ResultEchoesPathAsWritten(t *testing.T) {
	root, hostForms := symlinkedVolume(t)
	if err := os.Symlink("sub", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})

	res := execTool(t, &Write{}, ctx, map[string]any{"path": "./alias/b.txt", "content": "x"})
	assertNamesCallerPath(t, "write via alias", res, "./alias/b.txt", hostForms)
	res = execTool(t, &Edit{}, ctx, map[string]any{"path": "alias/b.txt", "old_string": "x", "new_string": "y"})
	assertNamesCallerPath(t, "edit via alias", res, "edited alias/b.txt ", hostForms)
}
