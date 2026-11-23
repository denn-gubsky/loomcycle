package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// forgetEngine is the host engine that records which sessions were ended.
type forgetEngine struct {
	*engine.Host
	mu     sync.Mutex
	forgot []string
}

func (e *forgetEngine) Forget(ctx context.Context, id string) error {
	e.mu.Lock()
	e.forgot = append(e.forgot, id)
	e.mu.Unlock()
	return e.Host.Forget(ctx, id)
}

func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", n)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkpoint(t *testing.T, w *worker, id string, args workspace.CheckpointArgs) workspace.CheckpointAnswer {
	t.Helper()
	a := fop(t, w, id, workspace.OpCheckpoint, args)
	mustOK(t, a, "checkpoint")
	var ca workspace.CheckpointAnswer
	if err := json.Unmarshal(a.output, &ca); err != nil {
		t.Fatal(err)
	}
	return ca
}

// A checkpoint counts its full size against its workspace's quota, beside
// the live files: a second one that would pass the quota is refused, a
// write that fits the files but not beside the checkpoint is refused, and
// deleting the checkpoint gives the room back. Without this a checkpoint
// would be a way to hold any number of bytes outside the quota.
func TestCheckpoint_CountsAgainstTheWorkspaceQuota(t *testing.T) {
	w, root := newTestWorker(t)
	if err := w.setLimit("ws1", 1000); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "ws1", "data"), 400)
	ca := checkpoint(t, w, "ws1", workspace.CheckpointArgs{Label: "first"})
	if ca.Bytes != 400 || ca.Usage.CheckpointBytes != 400 || ca.Usage.UsedBytes != 400 {
		t.Fatalf("the checkpoint answered %+v, want 400 bytes counted beside 400 used", ca)
	}
	if a := fop(t, w, "ws1", workspace.OpCheckpoint, workspace.CheckpointArgs{}); a.class != workspace.ClassOverQuota {
		t.Errorf("a second checkpoint past the quota = %q %q, want %s", a.class, a.detail, workspace.ClassOverQuota)
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "more", Content: strings.Repeat("y", 300)}); a.class != workspace.ClassOverQuota {
		t.Errorf("a write that fits only without the checkpoint = %q, want %s", a.class, workspace.ClassOverQuota)
	}
	d := fop(t, w, "ws1", workspace.OpDeleteCheckpoint, workspace.CheckpointRef{Checkpoint: ca.ID})
	mustOK(t, d, "delete_checkpoint")
	var da workspace.DeleteCheckpointAnswer
	_ = json.Unmarshal(d.output, &da)
	if !da.Deleted || da.Usage.CheckpointBytes != 0 {
		t.Errorf("delete_checkpoint answered %+v", da)
	}
	mustOK(t, runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "more", Content: strings.Repeat("y", 300)}), "write after the delete")
}

// A command can fill a workspace past its quota; a checkpoint of it is then
// refused, not recorded.
func TestCheckpoint_OfAWorkspaceOverItsQuotaIsRefused(t *testing.T) {
	w, root := newTestWorker(t)
	if err := w.setLimit("ws1", 1000); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "ws1", "data"), 1200)
	if a := fop(t, w, "ws1", workspace.OpCheckpoint, workspace.CheckpointArgs{}); a.class != workspace.ClassOverQuota {
		t.Errorf("a checkpoint of a workspace over its quota = %q, want %s", a.class, workspace.ClassOverQuota)
	}
	if got := w.checkpointList("ws1"); len(got) != 0 {
		t.Errorf("a refused checkpoint left %d", len(got))
	}
}

// A command that grows the files past what the quota leaves beside the
// checkpoints marks the workspace full: the next command and write are
// refused until room is made.
func TestCheckpoint_ACommandPastTheRoomLeftMarksTheWorkspaceFull(t *testing.T) {
	w, root := newTestWorker(t)
	if err := w.setLimit("ws1", 1000); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "ws1", "data")
	writeFile(t, data, 400)
	checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	real, err := filepath.EvalSymlinks(filepath.Join(root, "ws1"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The command: the file grows to 700, which fits the quota alone but
	// not beside the 400 the checkpoint holds.
	mustOK(t, w.commandInQuota(ctx, "ws1", real, func() answer { writeFile(t, data, 700); return answer{} }), "command")
	if a := w.commandInQuota(ctx, "ws1", real, func() answer { return answer{} }); a.class != workspace.ClassOverQuota {
		t.Errorf("the next command = %q, want %s", a.class, workspace.ClassOverQuota)
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "x", Content: "1"}); a.class != workspace.ClassOverQuota {
		t.Errorf("a write = %q, want %s", a.class, workspace.ClassOverQuota)
	}
}

// A restore replaces the live files with the checkpoint's, so the
// checkpoint's size must fit beside every checkpoint held.
func TestRestore_PastTheQuotaIsRefused(t *testing.T) {
	w, root := newTestWorker(t)
	if err := w.setLimit("ws1", 1000); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "ws1", "data")
	writeFile(t, data, 400)
	ck1 := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	mustOK(t, fop(t, w, "ws1", workspace.OpFRemove, workspace.FRemoveArgs{Path: "data"}), "rm")
	mustOK(t, runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "small", Content: strings.Repeat("z", 300)}), "write")
	checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	mustOK(t, fop(t, w, "ws1", workspace.OpFRemove, workspace.FRemoveArgs{Path: "small"}), "rm")
	if a := fop(t, w, "ws1", workspace.OpRestore, workspace.CheckpointRef{Checkpoint: ck1.ID}); a.class != workspace.ClassOverQuota {
		t.Errorf("a restore of 400 bytes beside 700 held under a quota of 1000 = %q %q, want %s", a.class, a.detail, workspace.ClassOverQuota)
	}
	if _, err := os.Stat(data); err == nil {
		t.Error("a refused restore put files back")
	}
}

// The workspace keeps at most the checkpoints the core allows.
func TestCheckpoint_PastTheCountBoundIsRefused(t *testing.T) {
	w, root := newTestWorker(t)
	writeFile(t, filepath.Join(root, "ws1", "a"), 10)
	for range 2 {
		checkpoint(t, w, "ws1", workspace.CheckpointArgs{Max: 2})
	}
	if a := fop(t, w, "ws1", workspace.OpCheckpoint, workspace.CheckpointArgs{Max: 2}); a.class != workspace.ClassCheckpointLimit {
		t.Errorf("a third checkpoint with max 2 = %q, want %s", a.class, workspace.ClassCheckpointLimit)
	}
	for range workspace.DefaultMaxCheckpoints - 2 {
		checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	}
	if a := fop(t, w, "ws1", workspace.OpCheckpoint, workspace.CheckpointArgs{}); a.class != workspace.ClassCheckpointLimit {
		t.Errorf("checkpoint %d with the default bound = %q, want %s", workspace.DefaultMaxCheckpoints+1, a.class, workspace.ClassCheckpointLimit)
	}
}

// A checkpoint is found only under the workspace the core named: another
// workspace's id, and anything shaped like a path, is not a checkpoint.
func TestCheckpoint_AnIDCannotReachAnotherWorkspacesCheckpointOrAPath(t *testing.T) {
	w, root := newTestWorker(t)
	if err := os.Mkdir(filepath.Join(root, "ws2"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "ws1", "mine"), 10)
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	for _, id := range []string{ck.ID, "../ws1/" + ck.ID, ".", "..", "ck_", "ck_../x", "/etc", filepath.Join(w.checkpointsOf("ws1"), ck.ID)} {
		if a := fop(t, w, "ws2", workspace.OpRestore, workspace.CheckpointRef{Checkpoint: id}); a.class != workspace.ClassCheckpointNotFound {
			t.Errorf("restore of %q in another workspace = %q, want %s", id, a.class, workspace.ClassCheckpointNotFound)
		}
		if a := fop(t, w, "ws3", workspace.OpFork, workspace.ForkArgs{From: "ws2", Checkpoint: id}); a.class != workspace.ClassCheckpointNotFound {
			t.Errorf("fork of ws2 from %q = %q, want %s", id, a.class, workspace.ClassCheckpointNotFound)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ws2", "mine")); err == nil {
		t.Error("another workspace's checkpoint was restored")
	}
	if _, err := os.Stat(filepath.Join(root, "ws3")); err == nil {
		t.Error("a refused fork left its folder")
	}
}

// A link is recorded as a link: its target, outside the workspace, is
// neither copied nor counted, and a restore puts the link back as a link.
func TestCheckpoint_ALinkIsCopiedAsALinkNeverItsTarget(t *testing.T) {
	w, root := newTestWorker(t)
	link := filepath.Join(root, "ws1", "out")
	if err := os.Symlink(filepath.Join(root, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(link)
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	if ck.Bytes != fi.Size() || ck.Files != 0 {
		t.Errorf("the checkpoint counts %d bytes and %d files, want the link's own %d and no file", ck.Bytes, ck.Files, fi.Size())
	}
	inCk := filepath.Join(w.checkpointsOf("ws1"), ck.ID, "out")
	if st, err := os.Lstat(inCk); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the checkpoint's entry is not a link: %v", err)
	}
	mustOK(t, fop(t, w, "ws1", workspace.OpRestore, workspace.CheckpointRef{Checkpoint: ck.ID}), "restore")
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the restored entry is not a link: %v", err)
	}
}

// A restore brings back a deleted file, drops one made since, ends the
// workspace's session (where a running server lives), and keeps the
// checkpoint to be restored again.
func TestRestore_BringsBackTheFilesAndStopsTheSession(t *testing.T) {
	w, root := newTestWorker(t)
	eng := &forgetEngine{Host: &engine.Host{}}
	w.engine = eng
	ws := filepath.Join(root, "ws1")
	writeFile(t, filepath.Join(ws, "keep", "a.txt"), 5)
	if err := os.Chmod(filepath.Join(ws, "keep", "a.txt"), 0o750); err != nil {
		t.Fatal(err)
	}
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	if err := os.RemoveAll(filepath.Join(ws, "keep")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "new.txt"), 3)
	a := fop(t, w, "ws1", workspace.OpRestore, workspace.CheckpointRef{Checkpoint: ck.ID})
	mustOK(t, a, "restore")
	var ra workspace.RestoreAnswer
	_ = json.Unmarshal(a.output, &ra)
	if !ra.SessionStopped || ra.Usage.UsedBytes != 5 {
		t.Errorf("restore answered %+v", ra)
	}
	st, err := os.Stat(filepath.Join(ws, "keep", "a.txt"))
	if err != nil || st.Mode().Perm() != 0o750 {
		t.Errorf("the deleted file is not back with its mode: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "new.txt")); err == nil {
		t.Error("a file made after the checkpoint survived the restore")
	}
	if !slices.Equal(eng.forgot, []string{"ws1"}) {
		t.Errorf("sessions ended: %v, want ws1's", eng.forgot)
	}
	if got := w.checkpointList("ws1"); len(got) != 1 {
		t.Errorf("after the restore %d checkpoints are kept, want 1", len(got))
	}
	if ents, _ := os.ReadDir(filepath.Join(w.checkpointsRoot(), checkpointTmp)); len(ents) != 0 {
		t.Errorf("the restore left %d entries in the work folder", len(ents))
	}
}

// mechanisms are the checkpoint mechanisms the test machine's disk offers;
// copy always.
func mechanisms(t *testing.T) []string {
	out := []string{workspace.MechanismCopy}
	dir := t.TempDir()
	if cloneDirSupported(dir) {
		out = append(out, workspace.MechanismClone)
	}
	if reflinkSupported(dir) {
		out = append(out, workspace.MechanismReflink)
	}
	return out
}

// Two forks changed differently do not see each other, and the original's
// files stay as they were: a write, an append and a chmod in the fork,
// on every mechanism the disk offers (RFC GW §11; spike F found hard links
// fail each of these).
func TestFork_ChangesInAForkLeaveTheOriginalAndTheCheckpoint(t *testing.T) {
	for _, mech := range mechanisms(t) {
		t.Run(mech, func(t *testing.T) {
			w, root := newTestWorker(t)
			w.mech = mech
			orig := filepath.Join(root, "ws1")
			writeFile(t, filepath.Join(orig, "f"), 8)
			writeFile(t, filepath.Join(orig, "g"), 4)
			ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
			if ck.Mechanism != mech {
				t.Errorf("the checkpoint reports %q, want %q", ck.Mechanism, mech)
			}
			mustOK(t, fop(t, w, "wsa", workspace.OpFork, workspace.ForkArgs{From: "ws1"}), "fork")
			mustOK(t, fop(t, w, "wsb", workspace.OpFork, workspace.ForkArgs{From: "ws1", Checkpoint: ck.ID}), "fork from the checkpoint")
			for _, fork := range []string{"wsa", "wsb"} {
				d := filepath.Join(root, fork)
				if err := os.WriteFile(filepath.Join(d, "f"), []byte(fork), 0o644); err != nil {
					t.Fatal(err)
				}
				af, err := os.OpenFile(filepath.Join(d, "g"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = af.WriteString("appended")
				af.Close()
				if err := os.Chmod(filepath.Join(d, "g"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range []string{orig, filepath.Join(w.checkpointsOf("ws1"), ck.ID)} {
				if b, _ := os.ReadFile(filepath.Join(d, "f")); string(b) != "xxxxxxxx" {
					t.Errorf("%s/f = %q after the forks' writes", d, b)
				}
				b, _ := os.ReadFile(filepath.Join(d, "g"))
				st, _ := os.Stat(filepath.Join(d, "g"))
				if string(b) != "xxxx" || st.Mode().Perm() != 0o644 {
					t.Errorf("%s/g = %q mode %v after the forks' append and chmod", d, b, st.Mode().Perm())
				}
			}
			if a, _ := os.ReadFile(filepath.Join(root, "wsa", "f")); string(a) != "wsa" {
				t.Errorf("one fork sees the other's write: %q", a)
			}
		})
	}
}

// A fork must fit its own quota; one that does not leaves nothing behind.
func TestFork_PastTheForksQuotaLeavesNothing(t *testing.T) {
	w, root := newTestWorker(t)
	writeFile(t, filepath.Join(root, "ws1", "big"), 600)
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	for _, args := range []workspace.ForkArgs{{From: "ws1", DiskQuotaBytes: 500}, {From: "ws1", Checkpoint: ck.ID, DiskQuotaBytes: 500}} {
		if a := fop(t, w, "ws3", workspace.OpFork, args); a.class != workspace.ClassOverQuota {
			t.Errorf("fork %+v = %q, want %s", args, a.class, workspace.ClassOverQuota)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "ws3")); err == nil {
		t.Error("a refused fork left its folder")
	}
	if _, err := os.Lstat(w.quotaFile("ws3")); err == nil {
		t.Error("a refused fork left its quota")
	}
	if ents, _ := os.ReadDir(filepath.Join(w.checkpointsRoot(), checkpointTmp)); len(ents) != 0 {
		t.Errorf("a refused fork left %d entries in the work folder", len(ents))
	}
	a := fop(t, w, "ws3", workspace.OpFork, workspace.ForkArgs{From: "ws1", DiskQuotaBytes: 700})
	mustOK(t, a, "fork that fits")
	if w.limitOf("ws3") != 700 {
		t.Errorf("the fork's quota = %d, want 700", w.limitOf("ws3"))
	}
	if len(w.checkpointList("ws3")) != 0 {
		t.Error("a fork carried its source's checkpoints")
	}
}

// A fork is a new workspace on this worker: refused past capacity, and its
// source must be another workspace that is here.
func TestFork_RefusesPastCapacityAndABadSource(t *testing.T) {
	w, root := newTestWorker(t)
	w.opt.Capacity = 1
	if a := fop(t, w, "ws2", workspace.OpFork, workspace.ForkArgs{From: "ws1"}); a.class != workspace.ClassAtCapacity {
		t.Errorf("fork at capacity = %q, want %s", a.class, workspace.ClassAtCapacity)
	}
	w.opt.Capacity = 4
	for _, from := range []string{"ws1", "../ws1", "nope"} {
		want := workspace.ClassInvalid
		if from == "nope" {
			want = workspace.ClassNotFound
		}
		id := "ws1"
		if from != "ws1" {
			id = "ws2"
		}
		if a := fop(t, w, id, workspace.OpFork, workspace.ForkArgs{From: from}); a.class != want {
			t.Errorf("fork of %q into %s = %q, want %s", from, id, a.class, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "ws2")); err == nil {
		t.Error("a refused fork left its folder")
	}
}

// Under the host engine a fork's Python environment names the fork's folder,
// so the fork's pip installs into the fork (spike F); under the container
// engine nothing is rewritten, since every session sees /workspace.
func TestFork_RewritesAPythonEnvironmentOnlyOnTheHostEngine(t *testing.T) {
	for _, eng := range []string{workspace.EngineHost, workspace.EngineContainer} {
		t.Run(eng, func(t *testing.T) {
			w, root := newTestWorker(t)
			w.opt.Engine = eng
			real, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(real, "ws1")
			venv := filepath.Join(root, "ws1", ".venv")
			if err := os.MkdirAll(filepath.Join(venv, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := "home = /usr/bin\ncommand = /usr/bin/python3 -m venv " + src + "/.venv\n"
			_ = os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte(cfg), 0o644)
			_ = os.WriteFile(filepath.Join(venv, "bin", "pip"), []byte("#!"+src+"/.venv/bin/python\nimport pip\n"), 0o755)
			_ = os.WriteFile(filepath.Join(venv, "bin", "blob"), []byte("\x00"+src), 0o644)
			_ = os.Symlink("/usr/bin/python3", filepath.Join(venv, "bin", "python"))
			a := fop(t, w, "ws2", workspace.OpFork, workspace.ForkArgs{From: "ws1"})
			mustOK(t, a, "fork")
			var fa workspace.ForkAnswer
			_ = json.Unmarshal(a.output, &fa)
			pip, _ := os.ReadFile(filepath.Join(root, "ws2", ".venv", "bin", "pip"))
			fork := filepath.Join(real, "ws2")
			if eng == workspace.EngineContainer {
				if len(fa.Rewritten) != 0 || !strings.Contains(string(pip), src) {
					t.Errorf("the container engine rewrote %v; pip = %q", fa.Rewritten, pip)
				}
				return
			}
			if !slices.Equal(fa.Rewritten, []string{".venv/pyvenv.cfg", ".venv/bin/pip"}) {
				t.Errorf("rewritten = %v", fa.Rewritten)
			}
			if string(pip) != "#!"+fork+"/.venv/bin/python\nimport pip\n" {
				t.Errorf("the fork's pip = %q", pip)
			}
			if c, _ := os.ReadFile(filepath.Join(root, "ws2", ".venv", "pyvenv.cfg")); strings.Contains(string(c), src+"/") {
				t.Errorf("the fork's pyvenv.cfg still names the source: %q", c)
			}
			if l, _ := os.Readlink(filepath.Join(root, "ws2", ".venv", "bin", "python")); l != "/usr/bin/python3" {
				t.Errorf("the link was changed: %q", l)
			}
			if b, _ := os.ReadFile(filepath.Join(root, "ws1", ".venv", "bin", "pip")); !strings.Contains(string(b), src) {
				t.Errorf("the source's pip was changed: %q", b)
			}
		})
	}
}

// The work folder (staging, partial puts) is never recorded.
func TestCheckpoint_LeavesOutTheWorkFolder(t *testing.T) {
	w, root := newTestWorker(t)
	writeFile(t, filepath.Join(root, "ws1", workDir, "stage", "x"), 50)
	writeFile(t, filepath.Join(root, "ws1", "kept"), 5)
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	if ck.Bytes != 5 {
		t.Errorf("the checkpoint counts %d bytes, want 5", ck.Bytes)
	}
	if _, err := os.Lstat(filepath.Join(w.checkpointsOf("ws1"), ck.ID, workDir)); err == nil {
		t.Error("the work folder was recorded")
	}
}

// Checkpoints are kept beside the workspaces, never inside one, where a
// session or a file op could reach them.
func TestCheckpoint_IsKeptOutsideTheWorkspace(t *testing.T) {
	w, root := newTestWorker(t)
	writeFile(t, filepath.Join(root, "ws1", "a"), 1)
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	dir := filepath.Join(w.checkpointsOf("ws1"), ck.ID)
	rel, err := filepath.Rel(filepath.Join(root, "ws1"), dir)
	if err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("the checkpoint is at %s, inside the workspace", dir)
	}
	if got := listRoot(t, filepath.Join(root, "ws1")); got != "a" {
		t.Errorf("the workspace holds %q after a checkpoint", got)
	}
	if held, _ := w.held(); !slices.Equal(held, []string{"ws1"}) {
		t.Errorf("held() = %v: the checkpoints' folder is taken for a workspace", held)
	}
}

// Every way a workspace goes takes its checkpoints with it: the remove op,
// the removal the dispatcher asks at attach, and a start that finds them
// with no workspace.
func TestCheckpoint_GoesWithItsWorkspace(t *testing.T) {
	w, root := newTestWorker(t)
	for _, id := range []string{"ws2", "ws3", "ws4"} {
		if err := os.Mkdir(filepath.Join(root, id), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, id, "a"), 1)
		checkpoint(t, w, id, workspace.CheckpointArgs{})
	}
	mustOK(t, w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws2", Op: workspace.OpRemove}, nil), "remove")
	if gone := w.removeAll([]string{"ws3"}, discardLogger()); !slices.Equal(gone, []string{"ws3"}) {
		t.Fatalf("removeAll = %v", gone)
	}
	for _, id := range []string{"ws2", "ws3"} {
		if _, err := os.Lstat(w.checkpointsOf(id)); err == nil {
			t.Errorf("%s's checkpoints outlived its removal", id)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "ws4")); err != nil {
		t.Fatal(err)
	}
	w.cleanWork()
	for _, id := range []string{"ws2", "ws3", "ws4"} {
		if _, err := os.Lstat(w.checkpointsOf(id)); err == nil {
			t.Errorf("%s's checkpoints outlived it", id)
		}
	}
}

// A worker killed part way leaves work in progress, a checkpoint without its
// record and a record without its checkpoint; the next start removes them
// and keeps the finished checkpoints.
func TestCleanWork_RemovesUnfinishedCheckpoints(t *testing.T) {
	w, root := newTestWorker(t)
	writeFile(t, filepath.Join(root, "ws1", "a"), 1)
	ck := checkpoint(t, w, "ws1", workspace.CheckpointArgs{})
	dir := w.checkpointsOf("ws1")
	writeFile(t, filepath.Join(w.checkpointsRoot(), checkpointTmp, "fork-1", "x"), 1)
	writeFile(t, filepath.Join(dir, "ck_0000000000000001", "x"), 1)
	_ = os.WriteFile(filepath.Join(dir, "ck_0000000000000002.json"), []byte(`{"id":"ck_0000000000000002","bytes":1}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ck.ID+".json.tmp"), []byte(`{}`), 0o600)
	w.cleanWork()
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if want := []string{ck.ID, ck.ID + ".json"}; !slices.Equal(names, want) {
		t.Errorf("after the start the checkpoints' folder holds %v, want %v", names, want)
	}
	if _, err := os.Lstat(filepath.Join(w.checkpointsRoot(), checkpointTmp, "fork-1")); err == nil {
		t.Error("work in progress survived the start")
	}
}

// A label is short printable text.
func TestCheckpoint_RefusesABadLabel(t *testing.T) {
	w, _ := newTestWorker(t)
	for _, l := range []string{strings.Repeat("a", workspace.MaxCheckpointLabel+1), "a\nb", "\x1b[31m"} {
		if a := fop(t, w, "ws1", workspace.OpCheckpoint, workspace.CheckpointArgs{Label: l}); a.class != workspace.ClassInvalid {
			t.Errorf("label %q = %q, want invalid", l, a.class)
		}
	}
}
