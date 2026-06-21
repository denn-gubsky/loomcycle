package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// pathFixture returns a Path tool over an in-memory sqlite store, plus a ctx
// whose identity resolves the agent scope to scope_id "agent-1" under tenant
// "tnt". Seed dirents with seedDirent (the tool itself doesn't create them —
// that's the Memory/Volume/Document wiring's job).
func pathFixture(t *testing.T) (*Path, context.Context, store.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := tools.WithAgentName(context.Background(), "agent-1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", UserID: "u1", TenantID: "tnt"})
	return &Path{Store: s}, ctx, s
}

func seedDirent(t *testing.T, s store.Store, tenant, scope, scopeID, parent, name, kind string) {
	t.Helper()
	if _, err := s.DirentCreate(context.Background(), store.DirentRow{
		TenantID: tenant, Scope: scope, ScopeID: scopeID,
		ParentPath: parent, Name: name, Kind: kind, ResourceRef: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed %s%s: %v", parent, name, err)
	}
}

// seedAgent seeds into the fixture's agent scope (tnt / agent / agent-1).
func seedAgent(t *testing.T, s store.Store, parent, name, kind string) {
	seedDirent(t, s, "tnt", "agent", "agent-1", parent, name, kind)
}

func pathExec(t *testing.T, p *Path, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := p.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		_ = json.Unmarshal([]byte(res.Text), &out)
	}
	return out, res
}

func TestPath_LsOneLevel(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "docs", "directory")
	seedAgent(t, s, "/docs/", "a", "document")
	seedAgent(t, s, "/docs/", "b", "document")
	seedAgent(t, s, "/docs/a/", "deep", "document") // must not appear one-level

	out, res := pathExec(t, p, ctx, `{"op":"ls","path":"/docs"}`)
	if res.IsError {
		t.Fatalf("ls: %q", res.Text)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 2 {
		t.Errorf("ls /docs one-level = %d entries, want 2: %s", len(entries), res.Text)
	}

	// Recursive picks up the deep child.
	out, res = pathExec(t, p, ctx, `{"op":"ls","path":"/docs","recursive":true}`)
	if res.IsError {
		t.Fatalf("ls recursive: %q", res.Text)
	}
	entries, _ = out["entries"].([]any)
	if len(entries) != 3 {
		t.Errorf("ls /docs recursive = %d entries, want 3: %s", len(entries), res.Text)
	}
}

func TestPath_ResolveAndStat(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/notes/", "x", "memory_entry")
	seedAgent(t, s, "/", "notes", "directory")

	out, res := pathExec(t, p, ctx, `{"op":"resolve","path":"/notes/x"}`)
	if res.IsError || out["kind"] != "memory_entry" || out["full_path"] != "/notes/x" {
		t.Errorf("resolve /notes/x = %s (err=%v)", res.Text, res.IsError)
	}
	// A miss is a model-facing error.
	_, res = pathExec(t, p, ctx, `{"op":"resolve","path":"/notes/missing"}`)
	if !res.IsError {
		t.Errorf("resolve of a missing path should error; got %q", res.Text)
	}
}

func TestPath_MvCascade(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "docs", "directory")
	seedAgent(t, s, "/docs/", "a", "document")
	seedAgent(t, s, "/docs/a/", "b", "document")

	_, res := pathExec(t, p, ctx, `{"op":"mv","path":"/docs","to":"/archive"}`)
	if res.IsError {
		t.Fatalf("mv: %q", res.Text)
	}
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/archive/a/b"}`); r.IsError {
		t.Errorf("cascade: /archive/a/b should resolve after mv: %q", r.Text)
	}
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/docs/a"}`); !r.IsError {
		t.Errorf("old /docs/a should be gone after mv")
	}
}

func TestPath_MvRejectsClobberAndRoot(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "a", "document")
	seedAgent(t, s, "/", "b", "document")
	// no-clobber: destination exists.
	if _, r := pathExec(t, p, ctx, `{"op":"mv","path":"/a","to":"/b"}`); !r.IsError {
		t.Errorf("mv onto an existing path should refuse (no-clobber)")
	}
	// can't move the root.
	if _, r := pathExec(t, p, ctx, `{"op":"mv","path":"/","to":"/x"}`); !r.IsError {
		t.Errorf("mv of root should refuse")
	}
}

// Regression (review finding #1): moving a directory into its own subtree
// must be refused — otherwise the descendant-rewrite orphans the whole tree.
func TestPath_MvIntoOwnSubtreeRefused(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "proj", "directory")
	seedAgent(t, s, "/proj/", "file", "document")

	if _, r := pathExec(t, p, ctx, `{"op":"mv","path":"/proj","to":"/proj/inner"}`); !r.IsError {
		t.Fatalf("mv into own subtree must be refused; got %q", r.Text)
	}
	// The tree must be intact — /proj and /proj/file still resolve.
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/proj"}`); r.IsError {
		t.Errorf("/proj was corrupted by a refused mv: %q", r.Text)
	}
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/proj/file"}`); r.IsError {
		t.Errorf("/proj/file was corrupted by a refused mv: %q", r.Text)
	}
}

func TestPath_RmRecursiveRefusal(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "docs", "directory")
	seedAgent(t, s, "/docs/", "a", "document")

	// rm without recursive on a path with descendants refuses.
	if _, r := pathExec(t, p, ctx, `{"op":"rm","path":"/docs"}`); !r.IsError {
		t.Errorf("rm of a non-empty path should refuse without recursive")
	}
	// recursive removes it.
	if _, r := pathExec(t, p, ctx, `{"op":"rm","path":"/docs","recursive":true}`); r.IsError {
		t.Fatalf("rm recursive: %q", r.Text)
	}
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/docs"}`); !r.IsError {
		t.Errorf("/docs should be gone after recursive rm")
	}
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/docs/a"}`); !r.IsError {
		t.Errorf("/docs/a should be gone after recursive rm")
	}
}

func TestPath_RmResourceTooRejected(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "x", "memory_entry")
	if _, r := pathExec(t, p, ctx, `{"op":"rm","path":"/x","resource_too":true}`); !r.IsError {
		t.Errorf("rm resource_too should be rejected in v1")
	}
}

func TestPath_NoDotDot(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	if _, r := pathExec(t, p, ctx, `{"op":"resolve","path":"/docs/../etc/passwd"}`); !r.IsError {
		t.Errorf("a path containing .. must be refused")
	}
}

// THE isolation test: a dirent in the agent scope is invisible to a user-scope
// listing, and a different tenant can't see it either.
func TestPath_ScopeAndTenantIsolation(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/", "secret", "memory_entry") // tnt / agent / agent-1

	// Same path under user scope (scope_id u1) is a different tree → empty.
	out, res := pathExec(t, p, ctx, `{"op":"ls","path":"/","scope":"user"}`)
	if res.IsError {
		t.Fatalf("ls user: %q", res.Text)
	}
	if entries, _ := out["entries"].([]any); len(entries) != 0 {
		t.Errorf("user-scope listing leaked the agent-scope dirent: %s", res.Text)
	}

	// A run in a DIFFERENT tenant can't resolve it.
	ctxB := tools.WithAgentName(context.Background(), "agent-1")
	ctxB = tools.WithRunIdentity(ctxB, tools.RunIdentityValue{AgentID: "a_test", TenantID: "other"})
	if _, r := pathExec(t, p, ctxB, `{"op":"resolve","path":"/secret"}`); !r.IsError {
		t.Errorf("cross-tenant resolve must 404 (opaque); got %q", r.Text)
	}

	// The owning agent sees it.
	out, res = pathExec(t, p, ctx, `{"op":"ls","path":"/"}`)
	if res.IsError {
		t.Fatalf("ls agent: %q", res.Text)
	}
	if entries, _ := out["entries"].([]any); len(entries) != 1 {
		t.Errorf("agent-scope listing should see its own dirent: %s", res.Text)
	}
}

func TestPath_ScopeUserRequiresUserID(t *testing.T) {
	p, _, s := pathFixture(t)
	// ctx with NO user_id.
	ctx := tools.WithAgentName(context.Background(), "agent-1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "tnt"})
	_ = s
	if _, r := pathExec(t, p, ctx, `{"op":"ls","path":"/","scope":"user"}`); !r.IsError {
		t.Errorf("scope=user without a user_id should error")
	}
}
