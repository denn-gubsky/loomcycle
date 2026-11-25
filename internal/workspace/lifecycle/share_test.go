package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// runAs makes a run row of user carrying agentID and returns its Owner.
func (f *fixture) runAs(t *testing.T, user, agentID string) Owner {
	t.Helper()
	r, err := f.st.CreateRun(context.Background(), f.sess, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	return Owner{TenantID: "acme", UserID: user, RunID: r.ID, AgentID: agentID, RootRunID: r.ID}
}

type auditLog struct {
	mu  sync.Mutex
	evs []audit.Event
}

func (a *auditLog) Record(ev audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
	return nil
}

func (a *auditLog) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.evs {
		out = append(out, e.Action)
	}
	return out
}

// shareFixture: alice's owner run with workspace "work" holding notes.txt,
// and a second run of alice to share it with.
func shareFixture(t *testing.T) (*fixture, Owner, Owner) {
	t.Helper()
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	owner := f.runAs(t, "alice", "ag_owner")
	peer := f.runAs(t, "alice", "ag_peer")
	f.open(t, owner, "work")
	body := "hello\n"
	if _, err := f.m.PutFile(context.Background(), owner, "work", workspace.PutArgs{Path: "notes.txt", Size: int64(len(body))}, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return f, owner, peer
}

func (f *fixture) share(t *testing.T, owner Owner, name string, peer Owner, access string) {
	t.Helper()
	p, err := f.m.ResolvePeer(context.Background(), owner, peer.AgentID)
	if err != nil {
		t.Fatalf("resolve peer: %v", err)
	}
	if _, err := f.m.Share(context.Background(), owner, name, p, access, false); err != nil {
		t.Fatalf("share: %v", err)
	}
}

func readNotes(f *fixture, o Owner, ref string) (string, error) {
	var buf bytes.Buffer
	_, err := f.m.GetFile(context.Background(), o, ref, workspace.GetArgs{Path: "notes.txt"}, &buf, 0)
	return buf.String(), err
}

// writeOps are every write path the manager has, each tried through ref.
func writeOps(f *fixture, o Owner, ref string) map[string]error {
	ctx := context.Background()
	out := map[string]error{}
	_, out["put"] = f.m.PutFile(ctx, o, ref, workspace.PutArgs{Path: "x.txt", Size: 1}, strings.NewReader("x"))
	out["mkdir"] = f.m.Mkdir(ctx, o, ref, workspace.MkdirArgs{Path: "d"})
	_, out["remove"] = f.m.RemoveFile(ctx, o, ref, "notes.txt")
	_, out["stage"] = f.m.Stage(ctx, o, ref)
	_, out["commit"] = f.m.Commit(ctx, o, ref, workspace.CommitArgs{Stage: "s", Path: "p"})
	out["discard"] = f.m.Discard(ctx, o, ref, "s")
	_, out["splice"] = f.m.SpliceMarkdown(ctx, o, ref, workspace.MDSpliceArgs{Path: "notes.txt"}, workspace.MDSpliceInput{})
	_, out["resolve write"] = f.m.Resolve(ctx, o, ref, true)
	return out
}

// RFC GW §11 "Sharing": with a read share the peer reads the owner's file
// and every write path is refused; with read_write it writes.
func TestShare_ReadShareReadsAndRefusesEveryWrite(t *testing.T) {
	f, owner, peer := shareFixture(t)
	ref := SharedName("work", owner.AgentID)
	if _, err := readNotes(f, peer, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read before any share: err = %v, want ErrNotFound", err)
	}
	f.share(t, owner, "work", peer, store.WorkspaceShareRead)
	if got, err := readNotes(f, peer, ref); err != nil || got != "hello\n" {
		t.Fatalf("read through a read share = %q, %v", got, err)
	}
	for op, err := range writeOps(f, peer, ref) {
		if !errors.Is(err, ErrShareReadOnly) {
			t.Errorf("%s through a read share: err = %v, want ErrShareReadOnly", op, err)
		}
	}
	if got, _ := readNotes(f, owner, "work"); got != "hello\n" {
		t.Errorf("the owner's file changed: %q", got)
	}

	f.share(t, owner, "work", peer, store.WorkspaceShareReadWrite)
	if _, err := f.m.PutFile(context.Background(), peer, ref, workspace.PutArgs{Path: "from-peer.txt", Size: 2}, strings.NewReader("hi")); err != nil {
		t.Fatalf("put through a read_write share: %v", err)
	}
	var buf bytes.Buffer
	if _, err := f.m.GetFile(context.Background(), owner, "work", workspace.GetArgs{Path: "from-peer.txt"}, &buf, 0); err != nil || buf.String() != "hi" {
		t.Errorf("the owner reads the peer's file = %q, %v", buf.String(), err)
	}
	if row, _ := f.m.Get(context.Background(), owner, "work"); len(row.Shares) != 1 || row.Shares[0].Access != store.WorkspaceShareReadWrite {
		t.Errorf("a second share with the same run should replace the first: %+v", row.Shares)
	}
}

// A share's target is a live run of the same user, not the caller: another
// user's run, an ended run, an unknown agent id and itself are refused, all
// with one answer; a run with no user shares with nobody.
func TestResolvePeer_OnlyALiveRunOfTheSameUser(t *testing.T) {
	f, owner, _ := shareFixture(t)
	bob := f.runAs(t, "bob", "ag_bob")
	ended := f.runAs(t, "alice", "ag_ended")
	f.finish(t, ended.RunID, store.RunCompleted)
	other := Owner{TenantID: "other", UserID: "alice", RunID: "r_x", AgentID: "ag_x"}
	ctx := context.Background()
	for name, agentID := range map[string]string{"another user": bob.AgentID, "ended": ended.AgentID, "unknown": "ag_nobody", "itself": owner.AgentID, "empty": ""} {
		if _, err := f.m.ResolvePeer(ctx, owner, agentID); !errors.Is(err, ErrShareTarget) {
			t.Errorf("%s: err = %v, want ErrShareTarget", name, err)
		}
	}
	// Another tenant's run of a user with the same id is not the same user.
	if _, err := f.m.ResolvePeer(ctx, other, owner.AgentID); !errors.Is(err, ErrShareTarget) {
		t.Errorf("another tenant: err = %v, want ErrShareTarget", err)
	}
	noUser := owner
	noUser.UserID = ""
	if _, err := f.m.ResolvePeer(ctx, noUser, "ag_peer"); !errors.Is(err, ErrShareNoUser) {
		t.Errorf("no user: err = %v, want ErrShareNoUser", err)
	}
	if _, err := f.m.Share(ctx, noUser, "work", Peer{RunID: "r_y", AgentID: "ag_y"}, store.WorkspaceShareRead, false); !errors.Is(err, ErrShareNoUser) {
		t.Errorf("share with no user: err = %v, want ErrShareNoUser", err)
	}
}

// A row that lists a run of another user (written by anything but Share)
// still reaches nothing for it: the check is the user, not only the list.
func TestShare_ARowListingAnotherUsersRunGivesItNothing(t *testing.T) {
	f, owner, _ := shareFixture(t)
	bob := f.runAs(t, "bob", "ag_bob")
	row, _ := f.m.Get(context.Background(), owner, "work")
	forged := []store.WorkspaceShare{{RunID: bob.RunID, AgentID: bob.AgentID, Access: store.WorkspaceShareReadWrite}}
	if ok, err := f.st.WorkspaceSetShares(context.Background(), "acme", row.ID, row.Shares, forged); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := readNotes(f, bob, SharedName("work", owner.AgentID)); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob's read: err = %v, want ErrNotFound", err)
	}
}

// RFC GW §11: after unshare, and after the owner stops — before the sweeper
// releases its workspace — the peer is refused.
func TestShare_EndsWithUnshareAndWithTheOwner(t *testing.T) {
	f, owner, peer := shareFixture(t)
	ref := SharedName("work", owner.AgentID)
	ctx := context.Background()
	f.share(t, owner, "work", peer, store.WorkspaceShareRead)
	if ok, err := f.m.Unshare(ctx, owner, "work", peer.AgentID); err != nil || !ok {
		t.Fatalf("unshare = %v, %v", ok, err)
	}
	if _, err := readNotes(f, peer, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after unshare: err = %v, want ErrNotFound", err)
	}
	if ok, err := f.m.Unshare(ctx, owner, "work", peer.AgentID); err != nil || ok {
		t.Errorf("a second unshare = %v, %v; want false, nil", ok, err)
	}

	f.share(t, owner, "work", peer, store.WorkspaceShareRead)
	f.finish(t, owner.RunID, store.RunCompleted)
	if _, err := readNotes(f, peer, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the owner ended: err = %v, want ErrNotFound", err)
	}
}

// RFC GW §5.2: a share is on the row, so another replica's manager honours
// it, and a paused owner keeps it.
func TestShare_HoldsOnAnotherReplicaAndWhileTheOwnerIsPaused(t *testing.T) {
	f, owner, peer := shareFixture(t)
	f.share(t, owner, "work", peer, store.WorkspaceShareRead)
	if err := f.st.SetRunPauseState(context.Background(), owner.RunID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	replica := New(f.h.D, f.st, Limits{MaxPerRun: 4}, nil)
	var buf bytes.Buffer
	if _, err := replica.GetFile(context.Background(), peer, SharedName("work", owner.AgentID), workspace.GetArgs{Path: "notes.txt"}, &buf, 0); err != nil || buf.String() != "hello\n" {
		t.Errorf("read on another replica with the owner paused = %q, %v", buf.String(), err)
	}
}

// What only the owner may do is not reachable by a shared name: Get (the
// pin, which release, status, exec and share go through) refuses it.
func TestShare_ASharedNameIsNeverTheOwnersPin(t *testing.T) {
	f, owner, peer := shareFixture(t)
	f.share(t, owner, "work", peer, store.WorkspaceShareReadWrite)
	if _, err := f.m.Get(context.Background(), peer, SharedName("work", owner.AgentID)); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a shared name: err = %v, want ErrNotFound", err)
	}
	if _, err := f.m.Share(context.Background(), peer, SharedName("work", owner.AgentID), Peer{RunID: owner.RunID, AgentID: owner.AgentID}, store.WorkspaceShareRead, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("re-share by the peer: err = %v, want ErrNotFound", err)
	}
	if _, err := f.m.Unshare(context.Background(), peer, SharedName("work", owner.AgentID), peer.AgentID); !errors.Is(err, ErrNotFound) {
		t.Errorf("unshare by the peer: err = %v, want ErrNotFound", err)
	}
}

func TestShare_IsBoundedPerWorkspace(t *testing.T) {
	f, owner, _ := shareFixture(t)
	ctx := context.Background()
	for i := range MaxSharesPerWorkspace {
		p := Peer{RunID: "r_" + string(rune('a'+i)), AgentID: "ag"}
		if _, err := f.m.Share(ctx, owner, "work", p, store.WorkspaceShareRead, false); err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
	}
	if _, err := f.m.Share(ctx, owner, "work", Peer{RunID: "r_over", AgentID: "ag"}, store.WorkspaceShareRead, false); !errors.Is(err, ErrShareLimit) {
		t.Errorf("one share past the bound: err = %v, want ErrShareLimit", err)
	}
	// Changing an existing share's access is not a new one.
	if _, err := f.m.Share(ctx, owner, "work", Peer{RunID: "r_a", AgentID: "ag"}, store.WorkspaceShareReadWrite, false); err != nil {
		t.Errorf("re-share at the bound: %v", err)
	}
}

// SharedWith lists what is shared with the run, and the audit names both
// runs, the workspace and the access for a share, a use and an unshare.
func TestShare_ListedForThePeerAndAudited(t *testing.T) {
	f, owner, peer := shareFixture(t)
	log := &auditLog{}
	f.m.SetAudit(log)
	f.share(t, owner, "work", peer, store.WorkspaceShareRead)
	got, err := f.m.SharedWith(context.Background(), peer)
	if err != nil || len(got) != 1 || got[0].Row.Name != "work" || got[0].Row.OwnerAgentID != owner.AgentID || got[0].Share.Access != store.WorkspaceShareRead {
		t.Fatalf("shared with the peer = %+v, %v", got, err)
	}
	if mine, _ := f.m.SharedWith(context.Background(), owner); len(mine) != 0 {
		t.Errorf("shared with the owner = %+v, want none", mine)
	}
	if err := f.m.UseShared(context.Background(), peer, SharedName("work", owner.AgentID), false, "read", "File"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.UseShared(context.Background(), peer, SharedName("work", owner.AgentID), true, "write", "File"); !errors.Is(err, ErrShareReadOnly) {
		t.Errorf("a write use through a read share: err = %v", err)
	}
	if _, err := f.m.Unshare(context.Background(), owner, "work", peer.AgentID); err != nil {
		t.Fatal(err)
	}
	if acts := strings.Join(log.actions(), ","); acts != "workspace_share,workspace_share_use,workspace_unshare" {
		t.Fatalf("audit actions = %s", acts)
	}
	for _, ev := range log.evs {
		if ev.WorkspaceOwnerRunID != owner.RunID || ev.WorkspacePeerRunID != peer.RunID || ev.WorkspaceID == "" ||
			ev.TargetName != "workspace:work" || ev.WorkspaceAccess != store.WorkspaceShareRead {
			t.Errorf("%s names %+v", ev.Action, ev)
		}
	}
	if op := log.evs[1].WorkspaceOp; op != "read via File" {
		t.Errorf("use op = %q", op)
	}
}
