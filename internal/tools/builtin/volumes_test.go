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

// ctxWith attaches a VolumePolicy with the given bindings to a fresh ctx.
func ctxWith(bindings ...tools.VolumeBinding) context.Context {
	return tools.WithVolumePolicy(context.Background(), tools.VolumePolicyValue{Bindings: bindings})
}

// realDir EvalSymlinks's a fresh temp dir so a /var→/private/var (macOS) root
// matches the resolved paths the tools return.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A rw volume allows Write; the file lands under the volume root.
func TestWrite_RwVolumeAllowsWrite(t *testing.T) {
	root := realDir(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "work", Root: root, Default: true})
	w := &Write{Root: ""} // construction-time Root unset: only the volume can satisfy it.
	body, _ := json.Marshal(map[string]string{"path": "out.txt", "content": "hi", "volume": "work"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("rw volume should allow write, got %q", res.Text)
	}
	got, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Errorf("content = %q, want %q", got, "hi")
	}
}

// A ro volume refuses Write.
func TestWrite_RoVolumeRefusesWrite(t *testing.T) {
	root := realDir(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "ref", Root: root, ReadOnly: true, Default: true})
	w := &Write{}
	body, _ := json.Marshal(map[string]string{"path": "out.txt", "content": "hi", "volume": "ref"})
	res, err := w.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("ro volume must refuse write, got Text=%q IsError=%v", res.Text, res.IsError)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Error("refused write must not create the target file")
	}
}

// A ro volume refuses Edit.
func TestEdit_RoVolumeRefusesEdit(t *testing.T) {
	root := realDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "ref", Root: root, ReadOnly: true, Default: true})
	e := &Edit{}
	body, _ := json.Marshal(map[string]string{"path": "f.txt", "old_string": "aaa", "new_string": "bbb", "volume": "ref"})
	res, err := e.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("ro volume must refuse edit, got Text=%q IsError=%v", res.Text, res.IsError)
	}
	got, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(got) != "aaa" {
		t.Errorf("refused edit must not mutate the file; got %q", got)
	}
}

// A ro volume refuses NotebookEdit.
func TestNotebookEdit_RoVolumeRefusesEdit(t *testing.T) {
	root := realDir(t)
	nb := `{"cells":[{"id":"c1","cell_type":"code","source":["x=1"],"outputs":[],"execution_count":null}],"metadata":{},"nbformat":4,"nbformat_minor":5}`
	if err := os.WriteFile(filepath.Join(root, "n.ipynb"), []byte(nb), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "ref", Root: root, ReadOnly: true, Default: true})
	n := &NotebookEdit{}
	body, _ := json.Marshal(map[string]string{"file_path": "n.ipynb", "cell_id": "c1", "source": "x=2", "mode": "replace", "volume": "ref"})
	res, err := n.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("ro volume must refuse notebook edit, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

// Read operates on a ro volume (any volume is readable).
func TestRead_RoVolumeAllowsRead(t *testing.T) {
	root := realDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "ref", Root: root, ReadOnly: true, Default: true})
	r := &Read{}
	body, _ := json.Marshal(map[string]string{"path": "f.txt", "volume": "ref"})
	res, err := r.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Text != "data" {
		t.Fatalf("ro volume should be readable, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

// Bash refuses a ro volume (it cannot enforce read-only honestly — RFC §6).
func TestBash_RoVolumeRefused(t *testing.T) {
	root := realDir(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "ref", Root: root, ReadOnly: true, Default: true})
	b := &Bash{Enabled: true}
	body, _ := json.Marshal(map[string]string{"command": "echo hi", "volume": "ref"})
	res, err := b.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("Bash must refuse a ro volume, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

// Bash binds its cwd to a rw volume root.
func TestBash_RwVolumeBindsCwd(t *testing.T) {
	root := realDir(t)
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(tools.VolumeBinding{Name: "work", Root: root, Default: true})
	b := &Bash{Enabled: true}
	body, _ := json.Marshal(map[string]string{"command": "ls marker.txt", "volume": "work"})
	res, err := b.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, "marker.txt") {
		t.Fatalf("Bash cwd should be the volume root, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

// A named volume resolves the path under THAT volume's root, and a `..` escape
// is still rejected (the unchanged resolveInsideRoot containment).
func TestRead_NamedVolumeResolvesUnderItsRootAndRejectsEscape(t *testing.T) {
	base := realDir(t)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "x.txt"), []byte("from-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "secret.txt"), []byte("from-b"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(
		tools.VolumeBinding{Name: "a", Root: a, Default: true},
		tools.VolumeBinding{Name: "b", Root: b},
	)
	r := &Read{}

	// volume:"a" reads under a's root.
	body, _ := json.Marshal(map[string]string{"path": "x.txt", "volume": "a"})
	res, err := r.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Text != "from-a" {
		t.Fatalf("volume a should read x.txt, got Text=%q IsError=%v", res.Text, res.IsError)
	}

	// A `..` escape from volume a into volume b is still rejected.
	body, _ = json.Marshal(map[string]string{"path": "../b/secret.txt", "volume": "a"})
	res, err = r.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "escapes sandbox") {
		t.Fatalf("`..` escape from volume a must be rejected, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

// Omitted `volume` uses the designated default binding.
func TestRead_OmittedVolumeUsesDefault(t *testing.T) {
	base := realDir(t)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b, "x.txt"), []byte("default-is-b"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWith(
		tools.VolumeBinding{Name: "a", Root: a},
		tools.VolumeBinding{Name: "b", Root: b, Default: true},
	)
	r := &Read{}
	body, _ := json.Marshal(map[string]string{"path": "x.txt"}) // no volume → default (b).
	res, err := r.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Text != "default-is-b" {
		t.Fatalf("omitted volume should use the default (b), got Text=%q IsError=%v", res.Text, res.IsError)
	}
}

// Omitted `volume` with multiple bindings and NO designated default errors,
// listing the available names.
func TestRead_OmittedVolumeNoDefaultErrorsListsNames(t *testing.T) {
	base := realDir(t)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := ctxWith(
		tools.VolumeBinding{Name: "a", Root: a},
		tools.VolumeBinding{Name: "b", Root: b},
	)
	r := &Read{}
	body, _ := json.Marshal(map[string]string{"path": "x.txt"})
	res, err := r.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("multiple bindings + no default must error on omitted volume; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "a") || !strings.Contains(res.Text, "b") {
		t.Errorf("error should list available volume names; got %q", res.Text)
	}
}

// A named volume the agent is NOT bound to errors, listing the bound volumes.
func TestRead_UnboundVolumeErrorsListsBound(t *testing.T) {
	root := realDir(t)
	ctx := ctxWith(tools.VolumeBinding{Name: "work", Root: root, Default: true})
	r := &Read{}
	body, _ := json.Marshal(map[string]string{"path": "x.txt", "volume": "secrets"})
	res, err := r.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not bound to volume") {
		t.Fatalf("naming an unbound volume must error, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "work") {
		t.Errorf("error should list the bound volume(s); got %q", res.Text)
	}
}

// Context op=self reports the bound-volume list (name/path/mode/default) when
// a VolumePolicy is on ctx — so a bound agent can introspect exactly which
// volumes it may touch and which verb each allows.
func TestContextSelf_ReportsBoundVolumes(t *testing.T) {
	tool, ctx := contextFixture(t)
	ctx = tools.WithVolumePolicy(ctx, tools.VolumePolicyValue{Bindings: []tools.VolumeBinding{
		{Name: "work", Root: "/work/a", Default: true},
		{Name: "ref", Root: "/work/ref", ReadOnly: true},
	}})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if res.IsError {
		t.Fatalf("self: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if _, ok := out["sandbox"]; ok {
		t.Error("a bound agent should report `volumes`, not the legacy `sandbox` block")
	}
	vols, ok := out["volumes"].(map[string]any)
	if !ok {
		t.Fatalf("volumes = %v (%T), want an object", out["volumes"], out["volumes"])
	}
	bindings, ok := vols["bindings"].([]any)
	if !ok || len(bindings) != 2 {
		t.Fatalf("volumes.bindings = %v, want 2 entries", vols["bindings"])
	}
	first := bindings[0].(map[string]any)
	if first["name"] != "work" || first["mode"] != "rw" || first["default"] != true {
		t.Errorf("first binding = %v, want work/rw/default", first)
	}
	second := bindings[1].(map[string]any)
	if second["name"] != "ref" || second["mode"] != "ro" {
		t.Errorf("second binding = %v, want ref/ro", second)
	}
}

// Backward-compat: an UNBOUND agent (no VolumePolicy on ctx) uses the tool's
// construction-time Root exactly as before the feature.
func TestRead_UnboundAgentUsesLegacyRoot(t *testing.T) {
	root := realDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No VolumePolicy attached → effectiveRoot returns the construction-time Root.
	r := &Read{Root: root}
	body, _ := json.Marshal(map[string]string{"path": "f.txt"})
	res, err := r.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Text != "legacy" {
		t.Fatalf("unbound agent should use legacy Root, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}
