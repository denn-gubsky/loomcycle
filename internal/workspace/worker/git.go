package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/rootbilly"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// git.go — a git workspace on the worker (RFC GW §4.2, P11). Its mirror is a
// bare repository under <root>/.loomcycle-mirrors/<key>, one per (tenant,
// repository), filled only from the forge; its working tree is the
// workspace's directory, with its own git directory over the mirror. The
// supervisor reads both with gitrepo, never through the workspace's own
// configuration or alternates file. Its network work goes, request by
// request, to the core on the connection the op came on, carrying the op's
// ticket: the worker holds no credential and names no URL.

// mirrorsDir is where the mirrors live, beside the workspaces. Its name has a
// dot, which no workspace id does, so it is never taken for a workspace.
const mirrorsDir = ".loomcycle-mirrors"

// gitMarkerSuffix names the file beside a git workspace's directory that holds
// its mirror's key: a file, never a directory, so nothing counting the
// root's directories takes it for a workspace.
const gitMarkerSuffix = ".git-mirror"

type opClientKey struct{}

// withOpClient puts the connection an op came on in its context.
func withOpClient(ctx context.Context, c workspacepb.WorkspaceServiceClient) context.Context {
	return context.WithValue(ctx, opClientKey{}, c)
}

func opClient(ctx context.Context) workspacepb.WorkspaceServiceClient {
	c, _ := ctx.Value(opClientKey{}).(workspacepb.WorkspaceServiceClient)
	return c
}

// mirrorFS is the root-confined filesystem of mirror key, made when absent.
func (w *worker) mirrorFS(key string) (billy.Filesystem, func(), error) {
	if !gitrepo.ValidMirrorKey(key) {
		return nil, nil, errors.New("a malformed mirror key")
	}
	rel := filepath.Join(mirrorsDir, key)
	if err := os.MkdirAll(filepath.Join(w.opt.Root, rel), 0o700); err != nil {
		return nil, nil, err
	}
	v, err := fsops.OpenVolumeFS(w.opt.Root, "")
	if err != nil {
		return nil, nil, err
	}
	fs, err := rootbilly.New(v, rel)
	if err != nil {
		v.Close()
		return nil, nil, err
	}
	return fs, func() { v.Close() }, nil
}

// mirrorObjects is where a session sees mirror key's objects: one fixed path
// in a container (mounted read-only), the mirror's own on the host engine.
func (w *worker) mirrorObjects(key string) string {
	if w.opt.Engine == workspace.EngineContainer {
		return engine.MirrorObjectsMount
	}
	return filepath.Join(w.opt.Root, mirrorsDir, key, "objects")
}

// MirrorKeyOf is the mirror key a git workspace's marker holds, "" for any
// other workspace. The container engine reads it to mount the mirror.
func (w *worker) MirrorKeyOf(id string) string {
	if !workspace.ValidID(id) {
		return ""
	}
	// #nosec G304 -- id is a ValidID (no separator, no dot), so the path is a marker beside the workspaces in the worker's root
	b, err := os.ReadFile(filepath.Join(w.opt.Root, id+gitMarkerSuffix))
	if err != nil {
		return ""
	}
	key := strings.TrimSpace(string(b))
	if !gitrepo.ValidMirrorKey(key) {
		return ""
	}
	return key
}

// gitNetwork is how a git op reaches the forge: through the core, on the
// op's connection, under its ticket.
func (w *worker) gitNetwork(ctx context.Context, id, rawURL string, ticket []byte, maxObject, maxRead int64) (gitrepo.Network, func(), error) {
	client := opClient(ctx)
	if client == nil {
		return gitrepo.Network{}, nil, errors.New("the op came on no connection")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return gitrepo.Network{}, nil, err
	}
	spool, err := os.MkdirTemp(w.opt.Root, ".loomcycle-gitspool-")
	if err != nil {
		return gitrepo.Network{}, nil, err
	}
	n := gitrepo.Network{
		URL: rawURL,
		Guard: gitsource.Guard{Host: gitsource.HostKey(u), MaxReadBytes: maxRead,
			Transport: &gitTransport{w: w, client: client, workspaceID: id, ticket: ticket}},
		MaxObjectBytes: maxObject,
		SpoolDir:       spool,
	}
	return n, func() { _ = os.RemoveAll(spool) }, nil
}

// gitTransport is go-git's HTTP transport for one op: each request becomes
// an Egress stream naming one of the four git shapes and the op's ticket.
// The URL go-git built is read only for its shape; the core builds the
// request from the workspace's row.
type gitTransport struct {
	w           *worker
	client      workspacepb.WorkspaceServiceClient
	workspaceID string
	ticket      []byte
}

func gitShapeOf(r *http.Request) (workspacepb.GitShape, bool) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
		return workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD, true
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/info/refs") && r.URL.Query().Get("service") == "git-receive-pack":
		return workspacepb.GitShape_GIT_SHAPE_ADVERTISE_RECEIVE, true
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git-upload-pack"):
		return workspacepb.GitShape_GIT_SHAPE_UPLOAD_PACK, true
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git-receive-pack"):
		return workspacepb.GitShape_GIT_SHAPE_RECEIVE_PACK, true
	}
	return 0, false
}

func (t *gitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	shape, ok := gitShapeOf(r)
	if !ok {
		return nil, fmt.Errorf("not a git request this worker makes: %s", r.Method)
	}
	s, err := t.client.Egress(r.Context())
	if err != nil {
		return nil, err
	}
	hasBody := r.Body != nil && r.Body != http.NoBody
	start := &workspacepb.EgressStart{WorkerId: t.w.opt.ID, WorkspaceId: t.workspaceID, GitShape: shape, Ticket: t.ticket,
		HasBody: hasBody, ContentLength: r.ContentLength}
	for name, vals := range r.Header {
		for _, v := range vals {
			start.Headers = append(start.Headers, &workspacepb.Header{Name: name, Value: v})
		}
	}
	if err := s.Send(&workspacepb.EgressFromWorker{Msg: &workspacepb.EgressFromWorker_Start{Start: start}}); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if hasBody {
		sendEgressBody(s, r.Body)
	} else if err := s.CloseSend(); err != nil {
		return nil, err
	}
	m, err := s.Recv()
	if err != nil {
		return nil, err
	}
	head := m.GetHead()
	if head == nil {
		return nil, errors.New("the core's answer did not start with its head")
	}
	resp := &http.Response{StatusCode: int(head.GetStatus()), Header: http.Header{}, Request: r, ContentLength: -1,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	resp.Status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	for _, h := range head.GetHeaders() {
		resp.Header.Add(h.GetName(), h.GetValue())
	}
	pr, pw := io.Pipe()
	safego.Go(r.Context(), "workspace.worker.git-answer", func() {
		// After a panic go-git's read must still end: a no-op once the pipe
		// is closed below.
		defer pw.CloseWithError(errors.New("the core's answer ended early"))
		for {
			m, err := s.Recv()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			switch {
			case m.GetBody() != nil:
				if _, err := pw.Write(m.GetBody().GetData()); err != nil {
					return
				}
			case m.GetDone() != nil:
				if e := m.GetDone().GetError(); e != "" {
					pw.CloseWithError(errors.New(e))
					return
				}
				_ = pw.Close()
				return
			default:
				pw.CloseWithError(errors.New("an unexpected frame in the core's answer"))
				return
			}
		}
	})
	resp.Body = pr
	return resp, nil
}

// gitFetch fills a mirror from the forge.
func (w *worker) gitFetch(ctx context.Context, id string, raw []byte) answer {
	var in workspace.GitFetchArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	fs, done, err := w.lockedMirrorFS(in.Mirror)
	if err != nil {
		return refuse(workspace.ClassInvalid, "the mirror: %v", err)
	}
	defer done()
	m, err := gitrepo.OpenMirror(fs)
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the mirror: %v", err)
	}
	n, cleanup, err := w.gitNetwork(ctx, id, in.URL, in.Ticket, in.MaxObjectBytes, in.MaxReadBytes)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer cleanup()
	capped, a := w.capFetch(in.Mirror, in.MaxMirrorBytes, &n)
	if a.class != "" {
		return a
	}
	res, err := m.Fetch(ctx, n)
	if err != nil {
		if capped && gitClass(err) == gitTooLarge {
			return mirrorFull()
		}
		return refuse(gitClass(err), "fetching the mirror failed")
	}
	if in.Prove {
		if err := proveUpload(ctx, n); err != nil {
			return refuse(workspace.ClassFailed, "the forge did not answer an upload-pack: %v", err)
		}
	}
	return jsonAnswer(workspace.GitFetchAnswer{UpToDate: res.UpToDate, Updated: len(res.Updated)})
}

// proveUpload sends an upload-pack that asks for nothing (a flush alone),
// which the forge answers, empty, only for a credential it lets read: what
// the core's proof of access needs when the mirror was already up to date.
// The answer is read to its end, the point at which the core counts it.
func proveUpload(ctx context.Context, n gitrepo.Network) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(n.URL, "/")+"/git-upload-pack", strings.NewReader("0000"))
	if err != nil {
		return err
	}
	req.ContentLength = 4
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	resp, err := n.Guard.Transport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, proveAnswerMax))
	return err
}

// proveAnswerMax bounds what proveUpload reads: the forge's answer to a
// request for nothing is empty.
const proveAnswerMax = 64 << 10

// gitOpen makes a workspace's directory a git working tree over its mirror.
func (w *worker) gitOpen(ctx context.Context, id, dir string, raw []byte) answer {
	var in workspace.GitOpenArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	if _, err := os.Lstat(dir); err != nil {
		return refuse(workspace.ClassNotFound, "the workspace is not open")
	}
	fs, done, err := w.lockedMirrorFS(in.Mirror)
	if err != nil {
		return refuse(workspace.ClassInvalid, "the mirror: %v", err)
	}
	defer done()
	m, err := gitrepo.OpenMirror(fs)
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the mirror: %v", err)
	}
	base, err := m.Resolve(in.BaseRef)
	if err != nil {
		return refuse(gitClass(err), "%v", err)
	}
	v, err := fsops.OpenVolumeFS(dir, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	defer v.Close()
	wt, err := rootbilly.New(v, "")
	if err != nil {
		return refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	// The marker first, so a removal that follows a failure below still
	// finds the mirror whose pin it must drop.
	if err := os.WriteFile(filepath.Join(w.opt.Root, id+gitMarkerSuffix), []byte(in.Mirror+"\n"), 0o600); err != nil {
		return refuse(workspace.ClassFailed, "recording the workspace's mirror: %v", err)
	}
	spec := gitrepo.WorkspaceSpec{ID: id, URL: in.URL, Branch: in.Branch, AlternatesPath: w.mirrorObjects(in.Mirror),
		Limits: gitrepo.Limits{MaxFiles: in.MaxFiles, MaxBytes: in.MaxBytes}}
	if _, err := gitrepo.CreateWorkspace(wt, m, spec, base); err != nil {
		return refuse(gitClass(err), "%v", err)
	}
	return jsonAnswer(workspace.GitOpenAnswer{Base: base.String()})
}

// dropGit removes a git workspace's pins and its marker, at its removal.
func (w *worker) dropGit(id string) error {
	key := w.MirrorKeyOf(id)
	marker := filepath.Join(w.opt.Root, id+gitMarkerSuffix)
	if key == "" {
		_ = os.Remove(marker)
		return nil
	}
	fs, done, err := w.lockedMirrorFS(key)
	if err != nil {
		return err
	}
	defer done()
	m, err := gitrepo.OpenMirror(fs)
	if err != nil {
		return err
	}
	if err := m.Unpin(id); err != nil {
		return err
	}
	return os.Remove(marker)
}

// dropStrayPins removes, at start, the pins of workspaces this worker no
// longer holds, and their markers.
func (w *worker) dropStrayPins(held map[string]bool) {
	ents, err := os.ReadDir(filepath.Join(w.opt.Root, mirrorsDir))
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !gitrepo.ValidMirrorKey(e.Name()) {
			continue
		}
		fs, done, err := w.lockedMirrorFS(e.Name())
		if err != nil {
			continue
		}
		if m, err := gitrepo.OpenMirror(fs); err == nil {
			if ids, err := m.Pinned(); err == nil {
				for _, id := range ids {
					if !held[id] {
						_ = m.Unpin(id)
					}
				}
			}
		}
		done()
	}
	markers, _ := filepath.Glob(filepath.Join(w.opt.Root, "*"+gitMarkerSuffix))
	for _, p := range markers {
		if id := strings.TrimSuffix(filepath.Base(p), gitMarkerSuffix); !held[id] {
			_ = os.Remove(p)
		}
	}
}

// lockedMirrorFS is mirrorFS under the mirror's lock, taken first, so the
// idle sweep (sweepMirrors) can never remove a mirror an op is about to
// use; it marks the mirror used. done releases both.
func (w *worker) lockedMirrorFS(key string) (billy.Filesystem, func(), error) {
	unlock := w.lockMirror(key)
	fs, done, err := w.mirrorFS(key)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	w.touchMirror(key)
	return fs, func() { done(); unlock() }, nil
}

// lockMirror serialises the ops on one mirror on this worker: go-git writes
// refs and packs with no lock of its own.
func (w *worker) lockMirror(key string) func() {
	w.mirrorMu.Lock()
	if w.mirrorLocks == nil {
		w.mirrorLocks = map[string]chan struct{}{}
	}
	ch, ok := w.mirrorLocks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		w.mirrorLocks[key] = ch
	}
	w.mirrorMu.Unlock()
	ch <- struct{}{}
	return func() { <-ch }
}
