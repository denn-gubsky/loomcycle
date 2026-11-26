package worker

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/rootbilly"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// git_ops.go — a git workspace's network ops on the worker (RFC GW P11 plan
// §3.7). The repository and branch are the core's, from the row; the
// workspace's own configuration is never read. A failure answers its class
// with a fixed detail: what the forge said goes to this worker's log only,
// never back to the model.

// gitWorkspace opens workspace id's repository over its mirror, under the
// mirror's lock. The mirror must be the one the workspace was opened over:
// a core that names another is refused.
func (w *worker) gitWorkspace(id, dir string, in workspace.GitNetArgs) (*gitrepo.Repo, *gitrepo.Mirror, func(), answer) {
	if key := w.MirrorKeyOf(id); key == "" || key != in.Mirror {
		return nil, nil, nil, refuse(workspace.ClassInvalid, "this workspace is no git workspace over that mirror")
	}
	fs, done, err := w.mirrorFS(in.Mirror)
	if err != nil {
		return nil, nil, nil, refuse(workspace.ClassInvalid, "the mirror: %v", err)
	}
	unlock := w.lockMirror(in.Mirror)
	release := func() { unlock(); done() }
	m, err := gitrepo.OpenMirror(fs)
	if err != nil {
		release()
		return nil, nil, nil, refuse(workspace.ClassFailed, "opening the mirror: %v", err)
	}
	v, err := fsops.OpenVolumeFS(dir, "")
	if err != nil {
		release()
		return nil, nil, nil, refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	wt, err := rootbilly.New(v, "")
	if err != nil {
		_ = v.Close()
		release()
		return nil, nil, nil, refuse(workspace.ClassFailed, "opening the workspace: %v", err)
	}
	r, err := gitrepo.OpenWorkspace(wt, m, gitrepo.WorkspaceSpec{ID: id, URL: in.URL, Branch: in.Branch})
	if err != nil {
		_ = v.Close()
		release()
		return nil, nil, nil, refuse(gitClass(err), "%v", err)
	}
	return r, m, func() { _ = v.Close(); release() }, answer{}
}

// gitSync fetches the mirror and moves the workspace's tracking refs; with
// pull, it then fast-forwards the current branch to its tracking ref.
func (w *worker) gitSync(ctx context.Context, id, dir string, raw []byte, pull bool) answer {
	var in workspace.GitNetArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	r, m, done, a := w.gitWorkspace(id, dir, in)
	if a.class != "" {
		return a
	}
	defer done()
	n, cleanup, err := w.gitNetwork(ctx, id, in.URL, in.Ticket, in.MaxObjectBytes, in.MaxReadBytes)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer cleanup()
	if _, err := m.Fetch(ctx, n); err != nil {
		w.log.Warn("git: fetching a workspace's mirror failed", "workspace", id, "err", err)
		return refuse(gitClass(err), "fetching from the forge failed")
	}
	upd, err := r.TrackMirror(m, id)
	if err != nil {
		return refuse(gitClass(err), "%v", err)
	}
	out := workspace.GitNetAnswer{Updated: []string{}}
	for _, u := range upd {
		out.Updated = append(out.Updated, u.Name)
	}
	if !pull {
		out.UpToDate = len(out.Updated) == 0
		return jsonAnswer(out)
	}
	head, err := r.Head()
	if err != nil {
		return refuse(gitClass(err), "%v", err)
	}
	if head.Branch == "" {
		return refuse(workspace.ClassInvalid, "HEAD is on no branch; check a branch out to pull it")
	}
	h, err := r.MergeFF("refs/remotes/origin/" + head.Branch)
	switch {
	case errors.Is(err, gitrepo.ErrNotFastForward):
		return refuse(gitClass(err), "%s and origin/%s have diverged", head.Branch, head.Branch)
	case errors.Is(err, gitrepo.ErrInvalidRevision):
		return refuse(gitClass(err), "the forge has no branch %s", head.Branch)
	case err != nil:
		return refuse(gitClass(err), "%v", err)
	}
	out.Head, out.UpToDate = h, h == head.Hash
	return jsonAnswer(out)
}

// gitPush pushes the workspace's own branch, the row's, to the same name.
// Without force go-git refuses a push that is no fast-forward before it is
// sent.
func (w *worker) gitPush(ctx context.Context, id, dir string, raw []byte) answer {
	var in workspace.GitNetArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	r, _, done, a := w.gitWorkspace(id, dir, in)
	if a.class != "" {
		return a
	}
	defer done()
	n, cleanup, err := w.gitNetwork(ctx, id, in.URL, in.Ticket, in.MaxObjectBytes, in.MaxReadBytes)
	if err != nil {
		return refuse(workspace.ClassFailed, "%v", err)
	}
	defer cleanup()
	n.Guard.MaxWriteBytes = in.MaxPushBytes
	own := "refs/heads/" + in.Branch
	res, err := r.Push(ctx, n, gitrepo.PushOptions{RefSpecs: []string{own + ":" + own}, Force: in.Force})
	if err != nil {
		w.log.Warn("git: a workspace's push failed", "workspace", id, "err", err)
		return refuse(gitClass(err), "the push did not go through")
	}
	out := workspace.GitNetAnswer{UpToDate: res.UpToDate, Updated: []string{}}
	for _, u := range res.Updated {
		out.Updated = append(out.Updated, u.Name)
	}
	return jsonAnswer(out)
}

// gitClassName is the class a git op answers for gitrepo class c. gitsource's
// not_found (no such repository) is renamed: the core reads the workspace's
// own not_found as a directory gone from its worker.
func gitClassName(c string) string {
	if c == workspace.ClassNotFound {
		return workspace.ClassRepositoryNotFound
	}
	return c
}

// gitClass is err's class as a git op answers it.
func gitClass(err error) string { return gitClassName(gitrepo.Class(err)) }
