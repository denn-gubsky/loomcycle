package channels

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The writer census: every channel message is written by the channel writer
// (StorePublisher.Write), so a channel's definition — its hold, and the
// channel hooks that build on it — is honoured by every writer. A hold that
// five writers honour and a sixth does not is a hold with a hole in it; that
// is how the scheduler's ticks and a webhook's on_complete walked past one.
//
// This walks the module's non-test Go sources and fails on:
//   - a ChannelPublish call outside the store and the writer;
//   - a SnapshotRestoreChannelMessage call outside the store and snapshot
//     restore (a restore writes rows verbatim, held ones included);
//   - a reference to a reserved instant (held, or awaiting hooks) outside
//     the store and this package (only the writer decides that a message is
//     held — and the hook worker, which releases into a hold);
//   - a release or drop of a message awaiting hooks outside the store and
//     the channel-hook worker (only its hooks decide it);
//   - a WriteRequest without a TenantID (the definition is resolved in it).

// writerAllowed maps a guarded name to the module paths allowed to use it. A
// path ending in "/" allows the whole directory.
var writerAllowed = map[string][]string{
	"ChannelPublish":                {"internal/store/", "internal/channels/system.go"},
	"SnapshotRestoreChannelMessage": {"internal/store/", "internal/snapshot/restore.go"},
	// The hook worker releases what a channel's hooks let through into the
	// channel's hold, when it has one.
	"ChannelHeldVisibleAt":     {"internal/store/", "internal/channels/", "internal/channelhooks/"},
	"ChannelHookHeldVisibleAt": {"internal/store/", "internal/channels/"},
	// A hook-held message is settled only by the channel-hook worker.
	"ChannelReleaseHookHeld": {"internal/store/", "internal/channelhooks/"},
	"ChannelDropHookHeld":    {"internal/store/", "internal/channelhooks/"},
}

// skipDirs are trees with no Go the runtime builds.
var skipDirs = map[string]bool{".git": true, "web": true, "node_modules": true, "adapters": true, "packages": true, "vendor": true, "testdata": true}

func allowedAt(name, rel string) bool {
	for _, a := range writerAllowed[name] {
		if strings.HasSuffix(a, "/") && strings.HasPrefix(rel, a) || rel == a {
			return true
		}
	}
	return false
}

// censusFile reports every census violation in one source file. rel is its
// module-relative path.
func censusFile(fset *token.FileSet, rel string, src any) ([]string, error) {
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	report := func(n ast.Node, msg string) {
		out = append(out, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), msg))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			name := x.Sel.Name
			if _, guarded := writerAllowed[name]; guarded && !allowedAt(name, rel) {
				report(x, name+" outside the channel writer — write through channels.Writer")
			}
		case *ast.CompositeLit:
			if isWriteRequest(x.Type) && !hasKey(x, "TenantID") {
				report(x, "channels.WriteRequest without a TenantID — the channel's definition is resolved in it")
			}
		}
		return true
	})
	return out, nil
}

func isWriteRequest(t ast.Expr) bool {
	switch x := t.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "channels" && x.Sel.Name == "WriteRequest"
	case *ast.Ident:
		return x.Name == "WriteRequest"
	}
	return false
}

func hasKey(lit *ast.CompositeLit, key string) bool {
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return true
			}
		}
	}
	return false
}

func TestChannelWriterCensus_OnlyTheWriterWrites(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var violations []string
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		v, perr := censusFile(fset, filepath.ToSlash(rel), src)
		if perr != nil {
			return perr
		}
		files++
		violations = append(violations, v...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A census that read nothing proves nothing.
	if files < 500 {
		t.Fatalf("the census read %d files; the walk is not seeing the module", files)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// The census must be able to fail: each rule catches a planted violation.
func TestChannelWriterCensus_CatchesAPlantedWrite(t *testing.T) {
	for name, src := range map[string]string{
		"direct store write": `package x
func f() { s.ChannelPublish(ctx, msg, 0) }`,
		"stamping a hold": `package x
func f() { m.VisibleAt = store.ChannelHeldVisibleAt() }`,
		"stamping a hook hold": `package x
func f() { m.VisibleAt = store.ChannelHookHeldVisibleAt() }`,
		"deciding a hooked message": `package x
func f() { s.ChannelReleaseHookHeld(ctx, key, nil, t) }`,
		"restoring outside snapshot": `package x
func f() { s.SnapshotRestoreChannelMessage(ctx, m) }`,
		"a write with no tenant": `package x
func f() { w.Write(ctx, channels.WriteRequest{Channel: "c"}) }`,
	} {
		v, err := censusFile(token.NewFileSet(), "internal/somewhere/x.go", src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(v) == 0 {
			t.Errorf("%s: the census did not catch it", name)
		}
	}
	// And a write that names its tenant, in the writer's own file, is fine.
	ok := `package channels
func f() { p.Store.ChannelPublish(ctx, msg, 0); _ = WriteRequest{Channel: "c", TenantID: "t"} }`
	if v, _ := censusFile(token.NewFileSet(), "internal/channels/system.go", ok); len(v) != 0 {
		t.Errorf("the writer itself was flagged: %v", v)
	}
}
