package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// replica is one server of a cluster: its own process state (the tree's
// in-memory volume set) over the store every replica shares.
type replica struct {
	srv  *Server
	tool *builtin.VolumeDef
	set  *tools.EphemeralVolumeSet
}

// runCtx is a context for a run of the tree rooted at root on this replica,
// built as the server builds one: the tree's set and the tree's record.
func (r *replica) runCtx(root string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", RootRunID: root})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.VolumeDef = tools.VolumeDefPolicyValue{Scopes: []string{"any"}} })
	ctx = tools.WithEphemeralVolumes(ctx, r.set)
	return r.srv.withEphemeralRecord(ctx, root)
}

// twoReplicas builds two servers over one store. With shared, both use the
// same dynamic root, as replicas whose dynamic root is shared storage do;
// otherwise each has its own disk.
func twoServersOneStore(t *testing.T, shared bool) (a, b *replica) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "cluster.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	realDir := func() string {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	rootA := realDir()
	rootB := rootA
	if !shared {
		rootB = realDir()
	}
	mk := func(dynRoot string) *replica {
		cfg := makeBaseConfig()
		cfg.Volumes = map[string]config.Volume{"pool": {Path: dynRoot, Mode: "rw", DynamicRoot: true}}
		tool := &builtin.VolumeDef{Cfg: cfg, Store: st, MaxNameLen: 64}
		srv := New(cfg, &stubResolver{}, []tools.Tool{tool}, concurrency.New(4, 4, time.Second), st)
		return &replica{srv: srv, tool: tool, set: tools.NewEphemeralVolumeSet()}
	}
	return mk(rootA), mk(rootB)
}

func execTool(t *testing.T, tool tools.Tool, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return res
}

// A run of a tree on one replica reaches an ephemeral volume a run of the
// same tree created on another replica after it started, when the dynamic
// root is storage both reach. Before, the name was looked up only in this
// process's set, which never heard of it.
func TestEphemeralVolume_CreatedOnAnotherReplicaIsVisible(t *testing.T) {
	a, b := twoServersOneStore(t, true)
	const root = "r_tree"
	ctxA, ctxB := a.runCtx(root), b.runCtx(root) // B's run is already going

	res := execTool(t, a.tool, ctxA, `{"op":"create","name":"scratch","ephemeral":true,"mode":"rw"}`)
	if res.IsError {
		t.Fatalf("create on A: %s", res.Text)
	}
	var made struct{ Path string }
	_ = json.Unmarshal([]byte(res.Text), &made)
	if err := os.WriteFile(filepath.Join(made.Path, "note.txt"), []byte("from A\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := execTool(t, &builtin.Read{}, ctxB, `{"volume":"scratch","path":"note.txt"}`)
	if got.IsError || !strings.Contains(got.Text, "from A") {
		t.Fatalf("B read the volume A created: %s", got.Text)
	}
	if !b.set.Has("scratch") {
		t.Error("B did not take the volume into its set; every call would read the store again")
	}

	// B closes it; A's set still says writable, and A's next write is
	// refused because a write asks the record.
	if res := execTool(t, b.tool, ctxB, `{"op":"set_mode","name":"scratch","mode":"ro"}`); res.IsError {
		t.Fatalf("set_mode on B: %s", res.Text)
	}
	if res := execTool(t, &builtin.Write{}, ctxA, `{"volume":"scratch","path":"x.txt","content":"no"}`); !res.IsError {
		t.Error("A wrote to a volume B made read-only")
	}
	if res := execTool(t, &builtin.Write{}, ctxB, `{"volume":"scratch","path":"x.txt","content":"no"}`); !res.IsError {
		t.Error("B wrote to a volume it made read-only")
	}
}

// When the dynamic root is not shared, the volume's files are on the other
// replica's disk. The run is told that, not that the volume does not exist;
// and set_mode still closes it, because it changes the record every replica
// reads.
func TestEphemeralVolume_OnAnotherReplicasDiskSaysSo(t *testing.T) {
	a, b := twoServersOneStore(t, false)
	const root = "r_tree"
	ctxA, ctxB := a.runCtx(root), b.runCtx(root)

	if res := execTool(t, a.tool, ctxA, `{"op":"create","name":"scratch","ephemeral":true,"mode":"rw"}`); res.IsError {
		t.Fatalf("create on A: %s", res.Text)
	}
	got := execTool(t, &builtin.Read{}, ctxB, `{"volume":"scratch","path":"note.txt"}`)
	if !got.IsError || !strings.Contains(got.Text, "another server") {
		t.Fatalf("B reading a volume whose files are on A: %s, want it said they are on another server", got.Text)
	}
	if b.set.Has("scratch") {
		t.Error("B took a volume it cannot reach into its set")
	}
	if res := execTool(t, b.tool, ctxB, `{"op":"set_mode","name":"scratch","mode":"ro"}`); res.IsError {
		t.Fatalf("set_mode on B for a volume on A's disk: %s", res.Text)
	}
	if res := execTool(t, &builtin.Write{}, ctxA, `{"volume":"scratch","path":"x.txt","content":"no"}`); !res.IsError {
		t.Error("A wrote to a volume B made read-only")
	}
}

// The record names a volume; the path is this replica's own. A row whose
// stored path points elsewhere (a writer of the database, or a replica with
// another layout) does not move the volume there.
func TestEphemeralVolume_ThePathIsDerivedNotTakenFromTheRecord(t *testing.T) {
	a, b := twoServersOneStore(t, true)
	const root = "r_tree"
	res := execTool(t, a.tool, a.runCtx(root), `{"op":"create","name":"scratch","ephemeral":true,"mode":"rw"}`)
	if res.IsError {
		t.Fatalf("create on A: %s", res.Text)
	}
	var made struct{ Path string }
	_ = json.Unmarshal([]byte(res.Text), &made)
	if err := os.WriteFile(filepath.Join(made.Path, "note.txt"), []byte("from A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real path (no /var -> /private/var link on macOS), so a Read through
	// it would succeed if the stored path were followed.
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "secret.txt"), []byte("not yours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := a.tool.Store.EphemeralVolumeListByRun(context.Background(), root)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	row := rows[0]
	row.Definition, _ = json.Marshal(map[string]string{"path": elsewhere, "mode": "rw"})
	if _, err := a.tool.Store.EphemeralVolumeDeleteByRun(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tool.Store.EphemeralVolumeCreate(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	ctxB := b.runCtx(root)
	if got := execTool(t, &builtin.Read{}, ctxB, `{"volume":"scratch","path":"secret.txt"}`); !got.IsError {
		t.Errorf("a rewritten path was followed: %s", got.Text)
	}
	if got := execTool(t, &builtin.Read{}, ctxB, `{"volume":"scratch","path":"note.txt"}`); got.IsError || !strings.Contains(got.Text, "from A") {
		t.Errorf("the volume's own directory: %s", got.Text)
	}
}
