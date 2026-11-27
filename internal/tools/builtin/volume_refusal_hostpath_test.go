package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// hostPathVolume makes a volume root and, beside it, an outside directory
// with a file in it; inside the root, link.txt is a symlink to that file and
// linkdir one to the directory. Both returned paths are real (macOS's /var is
// a symlink).
func hostPathVolume(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

// assertRefusedWithoutHostPath fails unless res is a refusal whose text names
// neither the volume's host path nor the directory it sits in. A tool result
// reaches the model and the run's transcript; a host path there says where
// the volume lives on the server.
func assertRefusedWithoutHostPath(t *testing.T, what string, res tools.Result, root string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("%s: not refused: %q", what, res.Text)
	}
	if base := filepath.Dir(root); strings.Contains(res.Text, base) {
		t.Errorf("%s: refusal names a host path (%s): %q", what, base, res.Text)
	}
	// The refusal must be the path refusal, not one some earlier check made
	// (which would pass here whatever the path wording).
	for _, why := range []string{"escapes the volume", "symbolic link that leads outside", "does not exist", "the volume's directory", "dynamic and ephemeral volumes", "is a directory"} {
		if strings.Contains(res.Text, why) {
			return
		}
	}
	t.Errorf("%s: not a path refusal: %q", what, res.Text)
}

func execTool(t *testing.T, tool tools.Tool, ctx context.Context, args map[string]any) tools.Result {
	t.Helper()
	body, _ := json.Marshal(args)
	res, err := tool.Execute(ctx, body)
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	return res
}

func TestReadAndWrite_RefusalsNameNoHostPath(t *testing.T) {
	root, _ := hostPathVolume(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true})
	for _, p := range []string{"../outside/secret.txt", "link.txt", "linkdir/secret.txt", "missing.txt", "no/such/f.txt"} {
		assertRefusedWithoutHostPath(t, "read "+p, execTool(t, &Read{}, ctx, map[string]any{"path": p}), root)
	}
	for _, p := range []string{"../outside/new.txt", "linkdir/new.txt", "no/such/f.txt"} {
		assertRefusedWithoutHostPath(t, "write "+p, execTool(t, &Write{}, ctx, map[string]any{"path": p, "content": "x"}), root)
	}
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertRefusedWithoutHostPath(t, "read a directory", execTool(t, &Read{}, ctx, map[string]any{"path": "adir"}), root)
	assertRefusedWithoutHostPath(t, "notebook escape", execTool(t, &NotebookEdit{}, ctx, map[string]any{"file_path": "../outside/n.ipynb", "source": "x", "mode": "insert"}), root)
	assertRefusedWithoutHostPath(t, "notebook missing", execTool(t, &NotebookEdit{}, ctx, map[string]any{"file_path": "missing.ipynb", "source": "x", "mode": "insert"}), root)
}

func TestRead_FenceAndBrokenRootRefusalsNameNoHostPath(t *testing.T) {
	root, _ := hostPathVolume(t)
	fence := filepath.Join(root, "dynamic")
	if err := os.MkdirAll(filepath.Join(fence, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fence, "other", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "default", Root: root, Default: true, Fence: fence})
	assertRefusedWithoutHostPath(t, "read in the fence", execTool(t, &Read{}, ctx, map[string]any{"path": "dynamic/other/f.txt"}), root)

	gone := ctxWith(tools.VolumeBinding{Name: "default", Root: filepath.Join(root, "gone"), Default: true})
	assertRefusedWithoutHostPath(t, "read under a missing root", execTool(t, &Read{}, gone, map[string]any{"path": "f.txt"}), root)
	assertRefusedWithoutHostPath(t, "write under a missing root", execTool(t, &Write{}, gone, map[string]any{"path": "f.txt", "content": "x"}), root)
}

func TestCalcAndCryptoVolumeRefs_RefusalsNameNoHostPath(t *testing.T) {
	root, _ := hostPathVolume(t)
	ctx := calcRefCtx(calcBase(), root, "Read", "Write")
	for _, from := range []string{"volume:../outside/secret.txt", "volume:link.txt", "volume:linkdir/secret.txt", "volume:missing.csv"} {
		_, res := calcExec(t, &Calc{}, ctx, `{"expr":"1","from":{"t":"`+from+`"}}`)
		assertRefusedWithoutHostPath(t, "calc from "+from, res, root)
	}
	for _, to := range []string{"volume:../outside/out.json", "volume:linkdir/out.json", "volume:no/such/out.json"} {
		_, res := calcExec(t, &Calc{}, ctx, `{"expr":"1","to":"`+to+`"}`)
		assertRefusedWithoutHostPath(t, "calc to "+to, res, root)
	}

	c, _, cctx := cryptoFixture(t, "tenant")
	rctx := refCtx(cctx, root, "Read", "Write")
	for _, from := range []string{"volume:../outside/secret.txt", "volume:link.txt", "volume:missing.bin"} {
		_, res := cryptoExec(t, c, rctx, `{"op":"hash","from":"`+from+`"}`)
		assertRefusedWithoutHostPath(t, "crypto hash "+from, res, root)
	}
}
