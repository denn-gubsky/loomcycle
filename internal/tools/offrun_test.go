package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeteredOffRunCall_IsAbsentUnlessStamped(t *testing.T) {
	if _, ok := MeteredOffRunCall(context.Background()); ok {
		t.Error("a bare context reads as a metered off-run call")
	}
	// A run identity is what every admin and MCP dispatch stamps; it must not
	// read as the marker.
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{TenantID: "acme", UserID: "alice"})
	if _, ok := MeteredOffRunCall(ctx); ok {
		t.Error("a context carrying only a run identity reads as a metered off-run call")
	}
	v, ok := MeteredOffRunCall(WithMeteredOffRunCall(ctx, MeteredOffRunCallValue{TenantID: "acme", UserID: "alice"}))
	if !ok || v.TenantID != "acme" || v.UserID != "alice" {
		t.Errorf("the stamped value read back as %+v, %v", v, ok)
	}
}

// TestMeteredOffRunCall_HasOneStampingSite — the marker says "this call was
// admitted and will be billed by hand". It is true only where the caller, the
// key restriction and the budget were all handled, which is one function. A
// second production call site is a second place to get that wrong, so adding
// one must be a decision made here.
func TestMeteredOffRunCall_HasOneStampingSite(t *testing.T) {
	root := filepath.Join("..", "..")
	var sites []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if n := strings.Count(string(src), "WithMeteredOffRunCall("); n > 0 {
				rel, _ := filepath.Rel(root, path)
				for i := 0; i < n; i++ {
					sites = append(sites, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The definition, and the server's run-less decision path.
	want := []string{"internal/api/http/decide.go", "internal/tools/offrun.go"}
	if strings.Join(sites, ",") != strings.Join(want, ",") {
		t.Errorf("WithMeteredOffRunCall appears in %v, want exactly %v", sites, want)
	}
}
