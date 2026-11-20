package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacetest"
)

var wsFull = tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true}

type wsFixture struct {
	st   store.Store
	h    *workspacetest.Harness
	tool *Workspace
	sess string
}

func newWSFixture(t *testing.T, l lifecycle.Limits) *wsFixture {
	t.Helper()
	return newWSFixtureWith(t, l, worker.Options{Capacity: 10})
}

// newWSFixtureWith is newWSFixture with the worker's options.
func newWSFixtureWith(t *testing.T, l lifecycle.Limits, opt worker.Options) *wsFixture {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := workspacetest.Start(t, st, "acme", opt)
	sess, err := st.CreateSession(context.Background(), "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if l.MaxPerRun == 0 {
		l.MaxPerRun = 4
	}
	return &wsFixture{st: st, h: h, sess: sess.ID,
		tool: &Workspace{Manager: lifecycle.New(h.D, st, l, nil), ExecDefault: 10 * time.Second, ExecMax: 20 * time.Second}}
}

// runCtx is a run of user with the given grant. root is the run tree's root
// run; "" makes the run its own root.
func (f *wsFixture) runCtx(t *testing.T, user, root string, pol tools.WorkspacePolicyValue) context.Context {
	t.Helper()
	r, err := f.st.CreateRun(context.Background(), f.sess, store.RunIdentity{AgentID: "a_" + user, UserID: user, TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if root == "" {
		root = r.ID
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: user, AgentID: "a_" + user, TenantID: "acme", RootRunID: root})
	ctx = tools.WithRunID(ctx, r.ID)
	return tools.WithRunPolicy(ctx, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: pol})
}

func (f *wsFixture) call(t *testing.T, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, err := f.tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute(%s): %v", input, err)
	}
	return res
}

func (f *wsFixture) mustOK(t *testing.T, ctx context.Context, input string) tools.Result {
	t.Helper()
	res := f.call(t, ctx, input)
	if res.IsError {
		t.Fatalf("%s: %s", input, res.Text)
	}
	return res
}

func refusalClass(res tools.Result) string {
	var r struct {
		Error string `json:"error"`
	}
	if !res.IsError || json.Unmarshal([]byte(res.Text), &r) != nil {
		return ""
	}
	return r.Error
}

// The grant is checked before anything else: with none, every op is
// refused and nothing reaches the store or the worker; with ops [read], a
// command and a write are refused and a read runs.
func TestWorkspace_EveryOpChecksTheGrantFirst(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"a.txt","content":"hello"}`)

	ops := []string{
		`{"op":"open","name":"other"}`, `{"op":"release","name":"work"}`, `{"op":"list"}`, `{"op":"status","name":"work"}`,
		`{"op":"exec","name":"work","command":"touch made"}`, `{"op":"read","name":"work","path":"a.txt"}`,
		`{"op":"write","name":"work","path":"b.txt","content":"x"}`, `{"op":"edit","name":"work","path":"a.txt","old_string":"hello","new_string":"bye"}`,
		`{"op":"glob","name":"work","pattern":"*"}`, `{"op":"grep","name":"work","pattern":"hello"}`,
	}
	// The same run, its grant taken away: the pin would let it through, so
	// only the grant stops these.
	none := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}})
	for _, in := range ops {
		if c := refusalClass(f.call(t, none, in)); c != "not_granted" {
			t.Errorf("no grant: %s = %q, want not_granted", in, c)
		}
	}
	readOnly := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Read: true}})
	for _, in := range []string{ops[0], ops[4], ops[6], ops[7]} {
		if c := refusalClass(f.call(t, readOnly, in)); c != "not_granted" {
			t.Errorf("ops [read]: %s = %q, want not_granted", in, c)
		}
	}
	if res := f.mustOK(t, readOnly, ops[5]); res.Text != "hello" {
		t.Errorf("ops [read]: read = %q", res.Text)
	}
	rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(owner))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one workspace", len(rows))
	}
	dir := f.h.Dir(rows[0].ID)
	for _, name := range []string{"made", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a refused op made %s", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "hello" {
		t.Errorf("a refused edit changed the file: %q", b)
	}
}

// The pin: a sibling run of the same tree and a run of another user in the
// tenant cannot list, name or use the workspace, and are told what they
// would be told of a name nobody uses.
func TestWorkspace_AnotherRunCannotListNameOrUseAWorkspace(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"a.txt","content":"mine"}`)
	sibling := f.runCtx(t, "alice", tools.RunIdentity(owner).RootRunID, wsFull)
	other := f.runCtx(t, "bob", "", wsFull)
	unused := f.call(t, sibling, `{"op":"read","name":"nothing-here","path":"a.txt"}`)
	for who, ctx := range map[string]context.Context{"sibling": sibling, "another user": other} {
		if res := f.mustOK(t, ctx, `{"op":"list"}`); res.Text != `{"workspaces":[]}` {
			t.Errorf("%s: list = %s", who, res.Text)
		}
		for _, in := range []string{
			`{"op":"status","name":"work"}`, `{"op":"read","name":"work","path":"a.txt"}`,
			`{"op":"exec","name":"work","command":"cat a.txt"}`, `{"op":"write","name":"work","path":"a.txt","content":"theirs"}`,
			`{"op":"release","name":"work"}`,
		} {
			res := f.call(t, ctx, in)
			if refusalClass(res) != "not_found" || strings.Contains(res.Text, "mine") {
				t.Errorf("%s: %s = %s, want not_found", who, in, res.Text)
			}
		}
		res := f.call(t, ctx, `{"op":"read","name":"nothing-here","path":"a.txt"}`)
		if res.Text != unused.Text {
			t.Errorf("%s: a real workspace's refusal differs from an unused name's: %s vs %s", who, res.Text, unused.Text)
		}
	}
	if res := f.mustOK(t, owner, `{"op":"read","name":"work","path":"a.txt"}`); res.Text != "mine" {
		t.Errorf("the owner's file = %q, want it untouched", res.Text)
	}
}

// File ops through the worker give what the file tools give on the same
// tree, paths aside: the tools print the volume's absolute path, the
// workspace prints paths relative to it.
func TestWorkspace_FileOpsMatchTheFileTools(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(owner))
	wsDir := f.h.Dir(rows[0].ID)
	vol := realDir(t)
	for _, root := range []string{vol, wsDir} {
		for name, body := range map[string]string{
			"src/main.go": "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
			"src/util.go": "package main\n\n// helper does nothing\nfunc helper() {}\n",
			"README.md":   "line one\nline two\nline three\n",
		} {
			p := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			// One mtime for both trees, so glob's newest-first order matches.
			mt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(time.Duration(len(name)) * time.Second)
			_ = os.Chtimes(p, mt, mt)
		}
	}
	volCtx := ctxWith(tools.VolumeBinding{Name: "default", Root: vol, Default: true})
	cases := []struct {
		tool  tools.Tool
		op    string
		input string
	}{
		{&Read{}, "read", `{"path":"README.md"}`},
		{&Read{}, "read", `{"path":"README.md","offset":2,"limit":1,"line_numbers":true}`},
		{&Read{}, "read", `{"path":"missing.txt"}`},
		{&Glob{}, "glob", `{"pattern":"**/*.go"}`},
		{&Glob{}, "glob", `{"pattern":"*.md","path":"src"}`},
		{&Grep{}, "grep", `{"pattern":"func \\w+","output_mode":"content"}`},
		{&Grep{}, "grep", `{"pattern":"HELPER","case_insensitive":true}`},
		{&Grep{}, "grep", `{"pattern":"line","path":"README.md","output_mode":"count"}`},
		{&Write{}, "write", `{"path":"src/new.txt","content":"fresh"}`},
		{&Edit{}, "edit", `{"path":"README.md","old_string":"two","new_string":"2"}`},
		{&Edit{}, "edit", `{"path":"README.md","old_string":"absent","new_string":"x"}`},
		{&Read{}, "read", `{"path":"README.md"}`},
	}
	norm := func(s string) string {
		s = strings.ReplaceAll(s, vol+string(filepath.Separator), "")
		return strings.ReplaceAll(s, vol, ".")
	}
	for _, c := range cases {
		want, err := c.tool.Execute(volCtx, json.RawMessage(c.input))
		if err != nil {
			t.Fatal(err)
		}
		in := `{"op":"` + c.op + `","name":"work",` + c.input[1:]
		got := f.call(t, owner, in)
		if want.IsError {
			// The worker answers a failure as a refusal; its detail is the
			// file tool's text.
			var r struct{ Detail string }
			_ = json.Unmarshal([]byte(got.Text), &r)
			if !got.IsError || r.Detail != norm(want.Text) {
				t.Errorf("%s %s: workspace %q, file tool %q", c.op, c.input, got.Text, want.Text)
			}
			continue
		}
		if got.IsError || got.Text != norm(want.Text) {
			t.Errorf("%s %s:\nworkspace %q\nfile tool %q", c.op, c.input, got.Text, norm(want.Text))
		}
		if (want.Count == nil) != (got.Count == nil) || (want.Count != nil && *want.Count != *got.Count) {
			t.Errorf("%s %s: count %v, file tool %v", c.op, c.input, got.Count, want.Count)
		}
	}
}

func TestWorkspace_RefusesUnknownAndForeignArguments(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	for _, in := range []string{
		`{"op":"read","name":"work","path":"a","volume":"v"}`,
		`{"op":"read","name":"work","path":"a","content":"x"}`,
		`{"op":"exec","name":"work","command":"ls","timeout":5}`,
		`{"op":"launch","name":"work"}`,
		`{"op":"read","path":"a"}`,
	} {
		if c := refusalClass(f.call(t, owner, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
}

func TestWorkspace_ExecReportsTheExitCodeAndBoundsTheTimeout(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	res := f.call(t, owner, `{"op":"exec","name":"work","command":"echo out; exit 4"}`)
	if !res.IsError || res.Text != `{"exit_code":4,"output":"out\n"}` {
		t.Errorf("exec = %v %s", res.IsError, res.Text)
	}
	if c := refusalClass(f.call(t, owner, `{"op":"exec","name":"work","command":"true","timeout_s":21}`)); c != "invalid_argument" {
		t.Errorf("a timeout over the ceiling = %q", c)
	}
	res = f.call(t, owner, `{"op":"exec","name":"work","command":"sleep 30","timeout_s":1}`)
	if !strings.Contains(res.Text, `"timed_out":true`) || !res.IsError {
		t.Errorf("a command past its timeout = %s", res.Text)
	}
}

func TestWorkspace_OpenRefusesPastMaxPerRun(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{MaxPerRun: 1})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"one"}`)
	if c := refusalClass(f.call(t, owner, `{"op":"open","name":"two"}`)); c != "limit" {
		t.Errorf("a second workspace = %q, want limit", c)
	}
	f.mustOK(t, owner, `{"op":"release","name":"one"}`)
	f.mustOK(t, owner, `{"op":"open","name":"two"}`)
}

// A worker that went away fails the op with worker_lost, and says what that
// means; the run goes on.
func TestWorkspace_AWorkerGoneIsWorkerLost(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	f.h.StopWorker()
	res := f.call(t, owner, `{"op":"exec","name":"work","command":"true"}`)
	if refusalClass(res) != "worker_lost" || !strings.Contains(res.Text, "unknown") {
		t.Errorf("exec on a gone worker = %s", res.Text)
	}
}

func TestWorkspace_ModeOffRefusesEveryOp(t *testing.T) {
	ctx := tools.WithRunID(tools.WithRunPolicy(context.Background(), tools.RunPolicy{Workspace: wsFull}), "r1")
	res, err := (&Workspace{}).Execute(ctx, json.RawMessage(`{"op":"open","name":"work"}`))
	if err != nil || refusalClass(res) != "workspaces_off" {
		t.Errorf("= %s, %v; want workspaces_off", res.Text, err)
	}
}
