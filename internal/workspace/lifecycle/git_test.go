package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// gitForge stands in for a forge, recording each request's method and path.
type gitForge struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newGitForge(t *testing.T) *gitForge {
	t.Helper()
	f := &gitForge{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		_, _ = io.WriteString(w, "0000")
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *gitForge) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func gitManager(t *testing.T, f *gitForge) *Manager {
	t.Helper()
	p, err := egress.NewGitProxy(egress.GitOptions{
		PrivateHostAllowlist: []string{"127.0.0.1/32"},
		TLSConfig:            &tls.Config{RootCAs: f.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{}
	m.SetGitProxy(p)
	return m
}

func gitWorkspaceRow(f *gitForge, id, tenant string, state store.WorkspaceState) store.WorkspaceRow {
	return store.WorkspaceRow{ID: id, TenantID: tenant, Kind: store.WorkspaceKindGit, State: state,
		Git: &store.WorkspaceGit{URL: f.URL + "/acme/app.git", Branch: "loom/a", Mirror: "0123456789abcdef0123456789abcdef"}}
}

var fetchShapes = []egress.GitShape{egress.GitAdvertiseUpload, egress.GitUploadPack}

func mint(t *testing.T, m *Manager, row store.WorkspaceRow, forOpen bool, shapes ...egress.GitShape) *GitTicket {
	t.Helper()
	tk, err := m.MintGitTicket(GitTicketSpec{Row: row, ForOpen: forOpen, TTL: time.Minute,
		Grant: egress.GitGrant{RepoURL: row.Git.URL, Username: "x-access-token", Token: "tok-git-lifecycle", Shapes: shapes}})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(tk.End)
	return tk
}

func advertise() egress.GitRequest {
	return egress.GitRequest{Shape: egress.GitAdvertiseUpload, Header: http.Header{}}
}

func wantTicketRefusal(t *testing.T, err error) {
	t.Helper()
	var ref *egress.Refusal
	if !errors.As(err, &ref) || ref.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 refusal", err)
	}
}

func gitEgress(t *testing.T, m *Manager, row store.WorkspaceRow, ticket []byte, req egress.GitRequest) error {
	t.Helper()
	resp, err := m.GitEgress(context.Background(), row, ticket, req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return err
}

// A live ticket of the workspace's own op reaches the forge.
func TestGitTicket_ALiveTicketReachesTheForge(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, row, false, fetchShapes...)
	if err := gitEgress(t, m, row, tk.Secret(), advertise()); err != nil {
		t.Fatalf("a live ticket: %v", err)
	}
	if f.n() != 1 {
		t.Fatalf("the forge saw %d requests", f.n())
	}
}

// No ticket, a forged one, a cut one: refused before any network call.
func TestGitTicket_NoTicketOrAForgedOneIsRefused(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, row, false, fetchShapes...)
	forged := make([]byte, len(tk.Secret()))
	_, _ = rand.Read(forged)
	for name, ticket := range map[string][]byte{
		"none": nil, "empty": {}, "forged": forged, "cut": tk.Secret()[:len(tk.Secret())-1],
		"longer": append(append([]byte(nil), tk.Secret()...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			wantTicketRefusal(t, gitEgress(t, m, row, ticket, advertise()))
		})
	}
	if f.n() != 0 {
		t.Fatalf("a refused request reached the forge (%d)", f.n())
	}
}

// A ticket is good only while its op runs: used after the op ends, or after
// its deadline, it is refused, so a worker cannot replay an op's way out.
func TestGitTicket_AReplayAfterTheOpEndsIsRefused(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, row, false, fetchShapes...)
	secret := tk.Secret()
	tk.End()
	wantTicketRefusal(t, gitEgress(t, m, row, secret, advertise()))

	now := time.Now()
	m.now = func() time.Time { return now }
	late := mint(t, m, row, false, fetchShapes...)
	now = now.Add(2 * time.Minute)
	wantTicketRefusal(t, gitEgress(t, m, row, late.Secret(), advertise()))
	if f.n() != 0 {
		t.Fatalf("a replay reached the forge (%d)", f.n())
	}
}

// An ended ticket is refused even by a lookup that still sees it registered
// (one that copied the list just before the End): the ticket's own mark
// decides, not only its absence from the registry.
func TestGitTicket_AnEndedTicketStillListedIsRefused(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, row, false, fetchShapes...)
	tk.ended.Store(true) // ended, not yet removed
	wantTicketRefusal(t, gitEgress(t, m, row, tk.Secret(), advertise()))
	if f.n() != 0 {
		t.Fatalf("an ended ticket reached the forge (%d)", f.n())
	}
}

// A ticket is its workspace's and its tenant's: used for another workspace of
// the tenant, or presented with a row of the same id in another tenant, it is
// refused.
func TestGitTicket_AnotherWorkspacesOrTenantsUseIsRefused(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	a := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, a, false, fetchShapes...)
	b := gitWorkspaceRow(f, "ws_b", "acme", store.WorkspaceReady)
	_ = mint(t, m, b, false, fetchShapes...)
	wantTicketRefusal(t, gitEgress(t, m, b, tk.Secret(), advertise()))
	otherTenant := gitWorkspaceRow(f, "ws_a", "other", store.WorkspaceReady)
	wantTicketRefusal(t, gitEgress(t, m, otherTenant, tk.Secret(), advertise()))
	if f.n() != 0 {
		t.Fatalf("a request reached the forge (%d)", f.n())
	}
}

// A fetch's ticket cannot push: the shapes are the op's.
func TestGitTicket_AFetchTicketCannotPush(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, row, false, fetchShapes...)
	push := egress.GitRequest{Shape: egress.GitReceivePack, Header: http.Header{"Content-Type": {"application/x-git-receive-pack-request"}}, Body: strings.NewReader("0000")}
	wantTicketRefusal(t, gitEgress(t, m, row, tk.Secret(), push))
	wantTicketRefusal(t, gitEgress(t, m, row, tk.Secret(), egress.GitRequest{Shape: egress.GitAdvertiseReceive, Header: http.Header{}}))
	if f.n() != 0 {
		t.Fatalf("a push reached the forge (%d)", f.n())
	}
}

// A row that is still opening is reached only by the open's own ticket.
func TestGitTicket_AnOpeningRowNeedsTheOpensTicket(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	ready := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	opening := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceOpening)
	fetch := mint(t, m, ready, false, fetchShapes...)
	wantTicketRefusal(t, gitEgress(t, m, opening, fetch.Secret(), advertise()))
	open := mint(t, m, opening, true, fetchShapes...)
	if err := gitEgress(t, m, opening, open.Secret(), advertise()); err != nil {
		t.Fatalf("the open's ticket on its opening row: %v", err)
	}
}

// The repository is the row's: a row whose repository is not the one the
// ticket was minted for, and a row that is no git workspace, are refused.
func TestGitTicket_TheRowsRepositoryMustBeTheTickets(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	tk := mint(t, m, row, false, fetchShapes...)
	moved := row
	moved.Git = &store.WorkspaceGit{URL: f.URL + "/acme/other.git", Branch: "loom/a", Mirror: row.Git.Mirror}
	wantTicketRefusal(t, gitEgress(t, m, moved, tk.Secret(), advertise()))
	general := row
	general.Kind, general.Git = store.WorkspaceKindGeneral, nil
	wantTicketRefusal(t, gitEgress(t, m, general, tk.Secret(), advertise()))
	if f.n() != 0 {
		t.Fatalf("a request reached the forge (%d)", f.n())
	}
}

// A ticket minted for a row that is no git workspace, or with no shapes, is
// refused at mint.
func TestMintGitTicket_RefusesWhatItCannotBind(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceReady)
	for name, spec := range map[string]GitTicketSpec{
		"general row": {Row: store.WorkspaceRow{ID: "ws_a", TenantID: "acme"}, TTL: time.Minute, Grant: egress.GitGrant{RepoURL: row.Git.URL, Shapes: fetchShapes}},
		"other repo":  {Row: row, TTL: time.Minute, Grant: egress.GitGrant{RepoURL: f.URL + "/x.git", Shapes: fetchShapes}},
		"no shapes":   {Row: row, TTL: time.Minute, Grant: egress.GitGrant{RepoURL: row.Git.URL}},
		"no ttl":      {Row: row, Grant: egress.GitGrant{RepoURL: row.Git.URL, Shapes: fetchShapes}},
	} {
		if _, err := m.MintGitTicket(spec); err == nil {
			t.Errorf("%s: minted", name)
		}
	}
}

// An open's proof of access is the forge's own answers as the core read
// them: an advertisement and an upload-pack, both answered 200, during the
// open's ticket.
func TestGitTicket_ProofNeedsTheForgesAdvertisementAndUpload(t *testing.T) {
	f := newGitForge(t)
	m := gitManager(t, f)
	row := gitWorkspaceRow(f, "ws_a", "acme", store.WorkspaceOpening)
	tk := mint(t, m, row, true, fetchShapes...)
	if tk.Proved() {
		t.Fatal("proved before any request")
	}
	if err := gitEgress(t, m, row, tk.Secret(), advertise()); err != nil {
		t.Fatal(err)
	}
	if tk.Proved() {
		t.Fatal("proved by an advertisement alone")
	}
	up := egress.GitRequest{Shape: egress.GitUploadPack, Header: http.Header{"Content-Type": {"application/x-git-upload-pack-request"}}, Body: strings.NewReader("0000"), ContentLength: 4}
	if err := gitEgress(t, m, row, tk.Secret(), up); err != nil {
		t.Fatal(err)
	}
	if !tk.Proved() {
		t.Fatal("not proved after an advertisement and an upload")
	}
}
