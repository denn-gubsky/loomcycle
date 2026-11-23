package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW P8, §11 "Sharing", through the tools: the owner is the fixture's
// run (a_alice, the full grant and share), the peer a second run of alice.

var wsSharer = tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Share: true}

type shareFixture struct {
	*fcFixture
	peer context.Context
}

// newShareFixture: the owner holds workspace w with notes.txt; the peer is
// another run of alice (agent a_peer) granted pol.
func newShareFixture(t *testing.T, pol tools.WorkspacePolicyValue) *shareFixture {
	t.Helper()
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.ctx = tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = wsSharer })
	f.open(t, "w")
	f.wsPut(t, "w", "notes.txt", "hello\n")
	return &shareFixture{fcFixture: f, peer: f.runAs(t, "alice", "a_peer", pol)}
}

// runAs is a run of user carrying agentID, on the fixture's inbox, with
// File, Read, Write and Workspace and the workspace grant pol.
func (f *fcFixture) runAs(t *testing.T, user, agentID string, pol tools.WorkspacePolicyValue) context.Context {
	t.Helper()
	r, err := f.ws.st.CreateRun(context.Background(), f.ws.sess, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(f.ctx, tools.RunIdentityValue{UserID: user, AgentID: agentID, TenantID: "acme", RootRunID: "root-1"})
	ctx = tools.WithRunID(ctx, r.ID)
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.Tools = []string{"File", "Read", "Write", "Workspace"}
		p.Workspace = pol
	})
}

func (f *shareFixture) wsCall(t *testing.T, ctx context.Context, in string) tools.Result {
	t.Helper()
	return f.fcFixture.ws.call(t, ctx, in)
}

func (f *shareFixture) copyAs(t *testing.T, ctx context.Context, from, to string) tools.Result {
	t.Helper()
	_, res := f.exec(t, ctx, map[string]any{"op": "copy", "from": from, "to": to})
	return res
}

func wantShareRefusal(t *testing.T, what string, res tools.Result, fragment string) {
	t.Helper()
	if !res.IsError || !strings.Contains(res.Text, fragment) {
		t.Errorf("%s: %v %s; want a refusal naming %q", what, res.IsError, res.Text, fragment)
	}
}

func TestWorkspaceShare_ReadShareCopiesOutAndRefusesAWriteAndACommand(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true})
	wantShareRefusal(t, "copy before any share", f.copyAs(t, f.peer, "workspace:w@a_alice:notes.txt", "volume:inbox:out.txt"), "shared")
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read"}`)

	if res := f.copyAs(t, f.peer, "workspace:w@a_alice:notes.txt", "volume:inbox:out.txt"); res.IsError {
		t.Fatalf("copy out through a read share: %s", res.Text)
	}
	if res := f.wsCall(t, f.peer, `{"op":"read","name":"w@a_alice","path":"notes.txt"}`); res.IsError || !strings.Contains(res.Text, "hello") {
		t.Errorf("Workspace read through a read share: %s", res.Text)
	}
	if res := f.wsCall(t, f.peer, `{"op":"grep","name":"w@a_alice","pattern":"hel"}`); res.IsError {
		t.Errorf("Workspace grep through a read share: %s", res.Text)
	}
	wantShareRefusal(t, "copy in", f.copyAs(t, f.peer, "workspace:w@a_alice:notes.txt", "workspace:w@a_alice:in.txt"), "read-only")
	wantShareRefusal(t, "Workspace write", f.wsCall(t, f.peer, `{"op":"write","name":"w@a_alice","path":"x.txt","content":"x"}`), "read_only_share")
	wantShareRefusal(t, "File rm", func() tools.Result {
		_, r := f.exec(t, f.peer, map[string]any{"op": "rm", "path": "workspace:w@a_alice:notes.txt"})
		return r
	}(), "read-only")
	for _, op := range []string{`{"op":"exec","name":"w@a_alice","command":"true"}`, `{"op":"release","name":"w@a_alice"}`,
		`{"op":"status","name":"w@a_alice"}`, `{"op":"share","name":"w@a_alice","with":"a_alice","access":"read"}`} {
		// share fails the grant check first (the peer may not share);
		// the rest are refused as ops no share allows.
		wantShareRefusal(t, op, f.wsCall(t, f.peer, op), `"error":"not_`)
	}
	if got, _ := f.wsGet(t, "w", "notes.txt"); got != "hello\n" {
		t.Errorf("the owner's file is %q", got)
	}
}

func TestWorkspaceShare_ReadWriteCopiesInAndStillRefusesACommand(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true})
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read_write"}`)
	if res := f.copyAs(t, f.peer, "workspace:w@a_alice:notes.txt", "workspace:w@a_alice:copy.txt"); res.IsError {
		t.Fatalf("copy within the shared workspace: %s", res.Text)
	}
	if got, ok := f.wsGet(t, "w", "copy.txt"); !ok || got != "hello\n" {
		t.Errorf("the owner reads the peer's copy = %q %v", got, ok)
	}
	f.ws.mustOK(t, f.peer, `{"op":"write","name":"w@a_alice","path":"by-peer.txt","content":"hi"}`)
	if got, _ := f.wsGet(t, "w", "by-peer.txt"); got != "hi" {
		t.Errorf("by-peer.txt = %q", got)
	}
	wantShareRefusal(t, "exec", f.wsCall(t, f.peer, `{"op":"exec","name":"w@a_alice","command":"touch pwned"}`), "not_shared_op")
	// An interpreter cell runs code as exec does (RFC GW P5), so a share
	// never reaches it either.
	wantShareRefusal(t, "run_cell", f.wsCall(t, f.peer, `{"op":"run_cell","name":"w@a_alice","code":"open('pwned','w')"}`), "not_shared_op")
	wantShareRefusal(t, "reset_kernel", f.wsCall(t, f.peer, `{"op":"reset_kernel","name":"w@a_alice"}`), "not_shared_op")
	if f.wsHas(t, "w", "pwned") {
		t.Error("the peer's command ran")
	}
}

// A run of another user is refused as a share's target; the answer is the
// one for an agent id nobody holds.
func TestWorkspaceShare_AnotherUsersRunIsRefusedAsATarget(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	bob := f.runAs(t, "bob", "a_bob", tools.WorkspacePolicyValue{Read: true})
	for _, with := range []string{"a_bob", "a_nobody", "a_alice"} {
		wantShareRefusal(t, "share with "+with, f.wsCall(t, f.ctx, `{"op":"share","name":"w","with":"`+with+`","access":"read"}`), "invalid_share_target")
	}
	wantShareRefusal(t, "bob's copy", f.copyAs(t, bob, "workspace:w@a_alice:notes.txt", "volume:inbox:b.txt"), "shared")
}

// After unshare, and after the owner stops, the peer is refused.
func TestWorkspaceShare_UnshareAndTheOwnersEndRefuseThePeer(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read"}`)
	if res := f.ws.mustOK(t, f.ctx, `{"op":"unshare","name":"w","with":"a_peer"}`); !strings.Contains(res.Text, `"unshared":true`) {
		t.Errorf("unshare = %s", res.Text)
	}
	wantShareRefusal(t, "read after unshare", f.wsCall(t, f.peer, `{"op":"read","name":"w@a_alice","path":"notes.txt"}`), "not_found")
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read"}`)
	if err := f.ws.st.FinishRun(context.Background(), f.runID, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	wantShareRefusal(t, "read after the owner ended", f.wsCall(t, f.peer, `{"op":"read","name":"w@a_alice","path":"notes.txt"}`), "not_found")
	wantShareRefusal(t, "copy after the owner ended", f.copyAs(t, f.peer, "workspace:w@a_alice:notes.txt", "volume:inbox:late.txt"), "shared")
}

// Both sides need the grant: an owner without share cannot share, nor hand
// over write it does not hold; a peer without read gains nothing.
func TestWorkspaceShare_BothDefinitionsMustAllowIt(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Write: true})
	noShare := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace.Share = false })
	wantShareRefusal(t, "share without the grant", f.wsCall(t, noShare, `{"op":"share","name":"w","with":"a_peer","access":"read"}`), "not_granted")
	readOnly := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace.Write = false })
	wantShareRefusal(t, "read_write by an owner that cannot write", f.wsCall(t, readOnly, `{"op":"share","name":"w","with":"a_peer","access":"read_write"}`), "not_granted")

	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read_write"}`)
	wantShareRefusal(t, "Workspace read by a peer without read", f.wsCall(t, f.peer, `{"op":"read","name":"w@a_alice","path":"notes.txt"}`), "not_granted")
	wantShareRefusal(t, "File copy out by a peer without read", f.copyAs(t, f.peer, "workspace:w@a_alice:notes.txt", "volume:inbox:o.txt"), "does not allow read")
}

func TestWorkspaceShare_ConnectIsRefusedUntilServersExist(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	wantShareRefusal(t, "connect", f.wsCall(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read","connect":true}`), "not_yet")
}

// The peer's list names what is shared with it, by the name to use; the
// owner's shows its shares.
func TestWorkspaceShare_ListShowsSharesBothWays(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read"}`)
	var peer struct {
		SharedWithMe []map[string]string `json:"shared_with_me"`
	}
	if err := json.Unmarshal([]byte(f.ws.mustOK(t, f.peer, `{"op":"list"}`).Text), &peer); err != nil {
		t.Fatal(err)
	}
	if len(peer.SharedWithMe) != 1 || peer.SharedWithMe[0]["name"] != "w@a_alice" || peer.SharedWithMe[0]["access"] != "read" {
		t.Errorf("peer's list = %+v", peer.SharedWithMe)
	}
	if res := f.ws.mustOK(t, f.ctx, `{"op":"list"}`); !strings.Contains(res.Text, `"shared_with":[{"access":"read","with":"a_peer"}]`) {
		t.Errorf("owner's list = %s", res.Text)
	}
}
