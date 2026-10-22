package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/go-git/go-billy/v5"
)

// The Git tool (RFC FB) works on git repositories inside the volumes an
// agent is bound to: status, history and diffs; staging, commits, branches
// and resets; and, when the agent's git_remote allows it, clone, fetch, pull
// and push.
//
// Why a tool and not `git` through a shell: a host command's environment is
// readable by the command, and the command is the agent's to choose, so a
// credential handed to `git` can be printed; and real git runs what a
// repository's configuration names (hooks, fsmonitor, filter drivers,
// credential helpers), which an agent that can write .git/config can set.
// Here the library is go-git, in process: it starts no process, and the
// credential is a value in this process sent to one host.
//
// What keeps each guarantee:
//
//   - The worktree and .git are read and written through volumeFS (an
//     os.Root walked one verified component at a time), never by path
//     string, so a symlink swap cannot carry a write out of the volume.
//   - The URL dialled is the one validated here: https, a host the operator
//     lists, and the library is called with it explicitly, so a remote
//     rewritten in .git/config (url.insteadOf included) is checked as the
//     library would read it. Every other scheme is refused process-wide.
//   - The credential resolves only when it is restricted to URLs covering
//     the target, so even a remote redirected to another allowed URL cannot
//     carry the token anywhere its owner did not allow.
//   - A failure reaches the model as a class, never a server's text.

// Git is the tool. Source is nil when the operator lists no git hosts: local
// repository work still runs; the network is off.
type Git struct {
	// Source is shared with git-source volumes: fetch hosts, byte and object
	// limits, timeout, concurrency slots, the transport and the read
	// credential resolver.
	Source *GitSourceConfig
	// PushHosts are the hosts a push may reach (LOOMCYCLE_GIT_PUSH_HOSTS).
	// Empty: no push. Push is the direction data leaves in, so it has its own
	// list.
	PushHosts []string
	// PushMaxBytes bounds the bytes one push may send.
	PushMaxBytes int64
	// PushCredential resolves a push's credential; "$ghapp:" mints a token
	// that may write. Kept apart from Source.Credential so no read path can
	// be handed a write token.
	PushCredential GitCredentialFunc
	// AuthorName and AuthorEmail sign every commit and tag the tool makes
	// (LOOMCYCLE_GIT_AUTHOR_NAME / _EMAIL). Empty: the agent's name and an
	// address under .invalid.
	AuthorName, AuthorEmail string
	// Audit records every network operation, refused ones included. Nil
	// records nothing.
	Audit audit.Sink

	// locks serialises the ops on one repository. go-git rewrites the index
	// in place with no lock file, and the loop runs a batch of tool calls
	// concurrently: two adds at once lost one of them.
	locksMu sync.Mutex
	locks   map[string]*gitRepoLock
}

// gitRepoLock is one repository's lock: a one-slot channel, so a waiter
// gives up when its run is cancelled. n counts holders and waiters, so the
// entry is dropped when nobody needs it.
type gitRepoLock struct {
	slot chan struct{}
	n    int
}

// lockRepo takes the repository's lock, keyed by its real path, and returns
// the release. It fails only when ctx ends first.
func (g *Git) lockRepo(ctx context.Context, key string) (func(), error) {
	g.locksMu.Lock()
	if g.locks == nil {
		g.locks = map[string]*gitRepoLock{}
	}
	l := g.locks[key]
	if l == nil {
		l = &gitRepoLock{slot: make(chan struct{}, 1)}
		g.locks[key] = l
	}
	l.n++
	g.locksMu.Unlock()
	forget := func() {
		g.locksMu.Lock()
		if l.n--; l.n == 0 {
			delete(g.locks, key)
		}
		g.locksMu.Unlock()
	}
	select {
	case l.slot <- struct{}{}:
		return func() { <-l.slot; forget() }, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}

const (
	// gitMaxListed caps every list a result carries (files, branches,
	// tags, remotes, refs); a longer one is cut and marked truncated.
	gitMaxListed         = 1000
	gitDefaultLogMax     = 20
	gitMaxLogMax         = 200
	gitDefaultDiffBytes  = 64 << 10
	gitMaxDiffBytes      = 1 << 20
	gitFetchUploadBytes  = 16 << 20 // a fetch's negotiation (wants, haves)
	gitDefaultPushBytes  = 100 << 20
	gitOpTimeoutFallback = 5 * time.Minute
)

func (g *Git) Name() string { return "Git" }

func (g *Git) Description() string {
	return "Work on a git repository inside a volume you are bound to, without a shell. " +
		"Pick the operation with `op`; `volume` names the volume (default: your default one) and `repo` the repository's directory in it (default: the volume root). " +
		"Read: status, log, show, diff, branch_list, tag_list, remote_list, merge_base, rev_parse. " +
		"Change (read-write volume): init, add, rm, mv, restore, reset, commit, checkout, branch_create, branch_delete, merge (fast-forward only), clean. " +
		"Network (only when your git_remote allows it, to hosts the operator lists): clone, fetch, pull (fast-forward only), push, ls_remote. " +
		"A network op takes `credential` as \"$cred:<name>\" or \"$ghapp:<name>\": the runtime resolves and sends it; you never see it, and it never goes into the repository. " +
		"Cherry-pick, rebase, revert, stash and non-fast-forward merges are not available. Read file contents with Read, not with this tool. " +
		"See the `git` help topic for every op's arguments."
}

func (g *Git) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"op":          {"type": "string", "enum": ["status","log","show","diff","branch_list","tag_list","remote_list","merge_base","rev_parse","init","add","rm","mv","restore","reset","commit","checkout","branch_create","branch_delete","merge","clean","clone","fetch","pull","push","ls_remote"]},
			"volume":      {"type": "string", "description": "Volume name; omit for your default volume. Context op=self lists your volumes."},
			"repo":        {"type": "string", "description": "The repository's directory, relative to the volume root. Omit for the volume root. clone and init create it if missing; clone needs it empty."},
			"paths":       {"type": "array", "items": {"type": "string"}, "description": "Repository-relative paths: add, rm, restore, reset (mixed), diff, log (first one)."},
			"rev":         {"type": "string", "description": "A revision (hash, branch, tag, HEAD~2): show, log, reset, merge, checkout (detached, or start point with create), branch_create, rev_parse."},
			"from":        {"type": "string", "description": "diff: the older revision; mv: the source path; merge_base: the first revision."},
			"to":          {"type": "string", "description": "diff: the newer revision; mv: the destination path; merge_base: the second revision."},
			"branch":      {"type": "string", "description": "checkout/branch_create/branch_delete: the branch; clone/pull: the remote branch."},
			"create":      {"type": "boolean", "description": "checkout: create branch at rev (or HEAD) and switch to it."},
			"force":       {"type": "boolean", "description": "checkout: discard local changes; branch_delete: delete an unmerged branch; push: overwrite the remote branch."},
			"force_with_lease": {"type": "boolean", "description": "push: overwrite only if the remote branch is where this repository last saw it."},
			"staged":      {"type": "boolean", "description": "diff: index vs HEAD; restore: unstage."},
			"worktree":    {"type": "boolean", "description": "restore: discard worktree changes (default when staged is not set)."},
			"cached":      {"type": "boolean", "description": "rm: untrack but keep the file."},
			"mode":        {"type": "string", "enum": ["soft","mixed","hard"], "description": "reset mode (default mixed)."},
			"message":     {"type": "string", "description": "commit message."},
			"all":         {"type": "boolean", "description": "commit: stage tracked changes first; log: every branch."},
			"allow_empty": {"type": "boolean"},
			"amend":       {"type": "boolean"},
			"max":         {"type": "integer", "description": "log: how many commits (default 20, at most 200)."},
			"max_bytes":   {"type": "integer", "description": "diff/show: output cap in bytes (default 65536, at most 1048576)."},
			"since":       {"type": "string", "description": "log: RFC 3339 time."},
			"until":       {"type": "string", "description": "log: RFC 3339 time."},
			"dirs":        {"type": "boolean", "description": "clean: untracked directories too."},
			"dry_run":     {"type": "boolean", "description": "clean: list, do not remove."},
			"url":         {"type": "string", "description": "clone/ls_remote: https URL of the repository."},
			"remote":      {"type": "string", "description": "fetch/pull/push: remote name (default origin)."},
			"credential":  {"type": "string", "description": "Network ops: \"$cred:<name>\" or \"$ghapp:<name>\". Omit for a public repository."},
			"depth":       {"type": "integer", "description": "clone/fetch: shallow depth."},
			"single_branch": {"type": "boolean"},
			"no_checkout": {"type": "boolean"},
			"refspecs":    {"type": "array", "items": {"type": "string"}, "description": "fetch/push refspecs. push default: the current branch."},
			"prune":       {"type": "boolean"},
			"tags":        {"type": "boolean", "description": "fetch: fetch tags; push: push tags."},
			"initial_branch": {"type": "string", "description": "init: the first branch (default main)."}
		},
		"required": ["op"]
	}`)
}

type gitInput struct {
	Op             string   `json:"op"`
	Volume         string   `json:"volume"`
	Repo           string   `json:"repo"`
	Paths          []string `json:"paths"`
	Rev            string   `json:"rev"`
	From           string   `json:"from"`
	To             string   `json:"to"`
	Branch         string   `json:"branch"`
	Create         bool     `json:"create"`
	Force          bool     `json:"force"`
	ForceWithLease bool     `json:"force_with_lease"`
	Staged         bool     `json:"staged"`
	Worktree       bool     `json:"worktree"`
	Cached         bool     `json:"cached"`
	Mode           string   `json:"mode"`
	Message        string   `json:"message"`
	All            bool     `json:"all"`
	AllowEmpty     bool     `json:"allow_empty"`
	Amend          bool     `json:"amend"`
	Max            int      `json:"max"`
	MaxBytes       int      `json:"max_bytes"`
	Since          string   `json:"since"`
	Until          string   `json:"until"`
	Dirs           bool     `json:"dirs"`
	DryRun         bool     `json:"dry_run"`
	URL            string   `json:"url"`
	Remote         string   `json:"remote"`
	Credential     string   `json:"credential"`
	Depth          int      `json:"depth"`
	SingleBranch   bool     `json:"single_branch"`
	NoCheckout     bool     `json:"no_checkout"`
	RefSpecs       []string `json:"refspecs"`
	Prune          bool     `json:"prune"`
	Tags           bool     `json:"tags"`
	InitialBranch  string   `json:"initial_branch"`
}

// gitOpKind sorts the ops by what they need: a read-write volume, the
// network, both.
type gitOpKind int

const (
	gitRead gitOpKind = iota
	gitWrite
	gitFetch // network, inbound; needs a read-write volume except ls_remote
	gitPush
)

var gitOps = map[string]gitOpKind{
	"status": gitRead, "log": gitRead, "show": gitRead, "diff": gitRead,
	"branch_list": gitRead, "tag_list": gitRead, "remote_list": gitRead,
	"merge_base": gitRead, "rev_parse": gitRead,
	"init": gitWrite, "add": gitWrite, "rm": gitWrite, "mv": gitWrite,
	"restore": gitWrite, "reset": gitWrite, "commit": gitWrite,
	"checkout": gitWrite, "branch_create": gitWrite, "branch_delete": gitWrite,
	"merge": gitWrite, "clean": gitWrite,
	"clone": gitFetch, "fetch": gitFetch, "pull": gitFetch, "ls_remote": gitFetch,
	"push": gitPush,
}

// gitRefusal is a failure the model sees: a class, and a detail only when it
// is safe to show (never a server's text).
func gitRefusal(class, detail string) tools.Result {
	out := map[string]string{"error": class}
	if detail != "" {
		out["detail"] = detail
	}
	b, _ := json.Marshal(out)
	return tools.Result{Text: string(b), IsError: true}
}

func gitOK(v any) tools.Result {
	b, err := json.Marshal(v)
	if err != nil {
		return gitRefusal("git_failed", "could not encode the result")
	}
	return tools.Result{Text: string(b)}
}

func (g *Git) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var in gitInput
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return gitRefusal("invalid_argument", "input: "+err.Error()), nil
	}
	kind, ok := gitOps[in.Op]
	if !ok {
		return gitRefusal("invalid_argument", fmt.Sprintf("unknown op %q", in.Op)), nil
	}
	if kind == gitFetch || kind == gitPush {
		return g.network(ctx, in, kind), nil
	}
	repo, done, res := g.openRepo(ctx, in, kind == gitWrite, in.Op == "init")
	if repo == nil && in.Op != "init" {
		return res, nil
	}
	defer done()
	if in.Op == "init" {
		return res, nil
	}
	return g.local(ctx, repo, in), nil
}

// repoDir resolves in.Volume and in.Repo to the volume's filesystem and the
// repository directory's path relative to its root, and takes that
// repository's lock. create makes the directory when it is missing (init,
// clone). The caller calls release, which unlocks and closes vfs.
func (g *Git) repoDir(ctx context.Context, in gitInput, needWrite, create bool) (vfs *volumeFS, rel string, release func(), res tools.Result, ok bool) {
	vfs, rel, res, ok = g.repoDirUnlocked(ctx, in, needWrite, create)
	if !ok {
		return nil, "", nil, res, false
	}
	unlock, err := g.lockRepo(ctx, filepath.Join(vfs.dir, rel))
	if err != nil {
		vfs.Close()
		return nil, "", nil, gitRefusal("timeout", "waiting for another Git operation on this repository"), false
	}
	return vfs, rel, func() { unlock(); vfs.Close() }, tools.Result{}, true
}

func (g *Git) repoDirUnlocked(ctx context.Context, in gitInput, needWrite, create bool) (vfs *volumeFS, rel string, res tools.Result, ok bool) {
	root, fence, err := effectiveRoot(ctx, in.Volume, needWrite)
	if err != nil {
		return nil, "", gitRefusal("volume_refused", err.Error()), false
	}
	target := in.Repo
	if target == "" {
		target = "."
	}
	var resolved string
	if create {
		resolved, err = resolveParentInsideRoot(root, target)
	} else {
		resolved, err = resolveInsideRoot(root, target)
	}
	if err == nil {
		err = outsideFence(fence, resolved)
	}
	if err != nil {
		return nil, "", gitRefusal("invalid_argument", err.Error()), false
	}
	vfs, err = openVolumeFS(root, fence)
	if err != nil {
		return nil, "", gitRefusal("volume_refused", err.Error()), false
	}
	rel, err = vfs.rel(resolved)
	if err != nil {
		vfs.Close()
		return nil, "", gitRefusal("invalid_argument", err.Error()), false
	}
	if create {
		top, err := newVolumeBilly(vfs, "")
		if err == nil {
			err = top.MkdirAll(filepath.ToSlash(rel), 0o755)
		}
		if err != nil {
			vfs.Close()
			return nil, "", gitRefusal("invalid_argument", "create the repository directory: "+err.Error()), false
		}
	}
	return vfs, rel, tools.Result{}, true
}

// openRepo opens the repository for a local op, or (init) creates it and
// returns the init result in res with a nil repo.
func (g *Git) openRepo(ctx context.Context, in gitInput, needWrite, isInit bool) (*gitrepo.Repo, func(), tools.Result) {
	vfs, rel, release, res, ok := g.repoDir(ctx, in, needWrite, isInit)
	if !ok {
		return nil, func() {}, res
	}
	done := release
	wt, err := newVolumeBilly(vfs, rel)
	if err != nil {
		done()
		return nil, func() {}, gitRefusal("invalid_argument", err.Error())
	}
	if isInit {
		r, err := gitrepo.Init(wt, in.InitialBranch)
		if err != nil {
			done()
			return nil, func() {}, gitLocalRefusal(err)
		}
		// A new repository's HEAD is unborn, so Head names no branch yet;
		// report the one the first commit will create.
		branch := in.InitialBranch
		if h, herr := r.Head(); herr == nil && h.Branch != "" {
			branch = h.Branch
		}
		if branch == "" {
			branch = "main"
		}
		return nil, done, gitOK(map[string]any{"op": "init", "repo": in.Repo, "branch": branch})
	}
	r, err := gitrepo.Open(wt)
	if err != nil {
		done()
		return nil, func() {}, gitLocalRefusal(err)
	}
	r.SetLimits(g.limits())
	return r, done, tools.Result{}
}

// gitLocalRefusal reports a local op's failure. go-git's text about a local
// repository names paths and revisions the agent gave, so it is shown.
func gitLocalRefusal(err error) tools.Result {
	return gitRefusal(gitrepo.Class(err), err.Error())
}

func (g *Git) local(ctx context.Context, r *gitrepo.Repo, in gitInput) tools.Result {
	maxBytes := in.MaxBytes
	if maxBytes <= 0 {
		maxBytes = gitDefaultDiffBytes
	}
	if maxBytes > gitMaxDiffBytes {
		maxBytes = gitMaxDiffBytes
	}
	switch in.Op {
	case "status":
		head, err := r.Head()
		if err != nil {
			return gitLocalRefusal(err)
		}
		st, err := r.Status()
		if err != nil {
			return gitLocalRefusal(err)
		}
		files, truncated := capList(st)
		return gitOK(map[string]any{"head": head, "files": files, "clean": len(st) == 0, "truncated": truncated})
	case "log":
		max := in.Max
		if max <= 0 {
			max = gitDefaultLogMax
		}
		if max > gitMaxLogMax {
			max = gitMaxLogMax
		}
		o := gitrepo.LogOptions{Rev: in.Rev, Max: max, All: in.All}
		if len(in.Paths) > 0 {
			o.Path = in.Paths[0]
		}
		for _, t := range []struct {
			s   string
			dst **time.Time
		}{{in.Since, &o.Since}, {in.Until, &o.Until}} {
			if t.s == "" {
				continue
			}
			v, err := time.Parse(time.RFC3339, t.s)
			if err != nil {
				return gitRefusal("invalid_argument", "since/until must be RFC 3339 times")
			}
			*t.dst = &v
		}
		commits, err := r.Log(o)
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"commits": commits})
	case "show":
		rev := in.Rev
		if rev == "" {
			rev = "HEAD"
		}
		info, patch, truncated, err := r.Show(rev, maxBytes)
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"commit": info, "patch": patch, "truncated": truncated})
	case "diff":
		patch, truncated, err := r.Diff(gitrepo.DiffOptions{From: in.From, To: in.To, Staged: in.Staged, Paths: in.Paths, MaxBytes: maxBytes})
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"patch": patch, "truncated": truncated})
	case "branch_list":
		b, err := r.Branches()
		if err != nil {
			return gitLocalRefusal(err)
		}
		b, truncated := capList(b)
		return gitOK(map[string]any{"branches": b, "truncated": truncated})
	case "tag_list":
		t, err := r.Tags()
		if err != nil {
			return gitLocalRefusal(err)
		}
		t, truncated := capList(t)
		return gitOK(map[string]any{"tags": t, "truncated": truncated})
	case "remote_list":
		rs, err := r.Remotes()
		if err != nil {
			return gitLocalRefusal(err)
		}
		// A remote URL written by someone else (an operator's CI clone) can
		// carry a token in its user part; the model is not shown it.
		for i := range rs {
			rs[i].URL = withoutUserinfo(rs[i].URL)
		}
		rs, truncated := capList(rs)
		return gitOK(map[string]any{"remotes": rs, "truncated": truncated})
	case "merge_base":
		bases, err := r.MergeBase(in.From, in.To)
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"merge_bases": bases})
	case "rev_parse":
		h, err := r.RevParse(in.Rev)
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"hash": h})
	case "add":
		if err := r.Add(in.Paths); err != nil {
			return gitLocalRefusal(err)
		}
	case "rm":
		if err := r.Remove(in.Paths, in.Cached); err != nil {
			return gitLocalRefusal(err)
		}
	case "mv":
		if err := r.Move(in.From, in.To); err != nil {
			return gitLocalRefusal(err)
		}
	case "restore":
		worktree := in.Worktree || !in.Staged
		if err := r.Restore(in.Paths, in.Staged, worktree); err != nil {
			return gitLocalRefusal(err)
		}
	case "reset":
		mode := in.Mode
		if mode == "" {
			mode = "mixed"
		}
		if err := r.Reset(mode, in.Rev, in.Paths); err != nil {
			return gitLocalRefusal(err)
		}
	case "commit":
		sig := g.signature(ctx)
		h, err := r.Commit(gitrepo.CommitOptions{
			Message: withRunTrailer(in.Message, tools.RunID(ctx)),
			Author:  sig, Committer: sig,
			All: in.All, AllowEmpty: in.AllowEmpty, Amend: in.Amend,
		})
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"op": "commit", "hash": h})
	case "checkout":
		if err := r.Checkout(gitrepo.CheckoutOptions{Branch: in.Branch, Rev: in.Rev, Create: in.Create, Force: in.Force}); err != nil {
			return gitLocalRefusal(err)
		}
	case "branch_create":
		if err := r.BranchCreate(in.Branch, in.Rev); err != nil {
			return gitLocalRefusal(err)
		}
	case "branch_delete":
		if err := r.BranchDelete(in.Branch, in.Force); err != nil {
			return gitLocalRefusal(err)
		}
	case "merge":
		h, err := r.MergeFF(in.Rev)
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"op": "merge", "head": h})
	case "clean":
		removed, err := r.Clean(in.Dirs, in.DryRun)
		if err != nil {
			return gitLocalRefusal(err)
		}
		return gitOK(map[string]any{"op": "clean", "dry_run": in.DryRun, "paths": removed})
	}
	if err := ctx.Err(); err != nil {
		return gitRefusal("timeout", "")
	}
	// A change that reports no value of its own answers with the status
	// after it, which is what the agent checks next anyway.
	head, _ := r.Head()
	st, err := r.Status()
	if err != nil {
		return gitOK(map[string]any{"op": in.Op, "ok": true})
	}
	files, truncated := capList(st)
	return gitOK(map[string]any{"op": in.Op, "ok": true, "head": head, "files": files, "truncated": truncated})
}

// capList cuts a list to gitMaxListed and says whether it did.
func capList[T any](xs []T) ([]T, bool) {
	if len(xs) > gitMaxListed {
		return xs[:gitMaxListed], true
	}
	return xs, false
}

// withoutUserinfo is a URL rebuilt from its scheme, host and path only. A
// token can sit in the user part, the query or the fragment, and a URL a
// parser reads as opaque or hostless can carry one anywhere: so nothing is
// removed from what was given, the parts known to be safe are copied, and
// anything else is replaced, not shown.
func withoutUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "(url not shown)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// limits are the expansion limits a repository's content is held to: the
// operator's git-source file limit, and the library's byte default.
func (g *Git) limits() gitrepo.Limits {
	var l gitrepo.Limits
	if g.Source != nil {
		l.MaxFiles = g.Source.MaxFiles
	}
	return l
}

// signature is who the tool commits as: the operator's identity, else the
// agent's name at an address that can never be delivered.
func (g *Git) signature(ctx context.Context) gitrepo.Signature {
	name, email := g.AuthorName, g.AuthorEmail
	agent := tools.AgentName(ctx)
	if agent == "" {
		agent = "loomcycle"
	}
	if name == "" {
		name = agent
	}
	if email == "" {
		email = gitEmailLocal(agent) + "@loomcycle.invalid"
	}
	return gitrepo.Signature{Name: name, Email: email, When: time.Now()}
}

// gitEmailLocal makes an agent name safe as an address's local part.
func gitEmailLocal(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b = append(b, r)
		default:
			b = append(b, '-')
		}
	}
	if len(b) == 0 {
		return "agent"
	}
	return string(b)
}

// withRunTrailer adds a trailer naming the run, so a reviewer can trace a
// commit to the run that made it (RFC FB D7). A message that already carries
// it is left alone (an amend).
func withRunTrailer(msg, runID string) string {
	if runID == "" {
		return msg
	}
	trailer := "Loomcycle-Run: " + runID
	if strings.Contains(msg, trailer) {
		return msg
	}
	return strings.TrimRight(msg, "\n") + "\n\n" + trailer + "\n"
}

// ---- network ----

// network runs clone, fetch, pull, push and ls_remote.
func (g *Git) network(ctx context.Context, in gitInput, kind gitOpKind) tools.Result {
	// Every attempt is audited, a refused one too: an agent trying a
	// credential on a URL it does not cover, or a host the operator does
	// not list, is what an operator most wants to see.
	ev := g.auditEvent(ctx, in, "", "")
	refuse := func(res tools.Result) tools.Result {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(res.Text), &e)
		ev.GitOutcome = e.Error
		g.record(ev)
		return res
	}
	deny := func(class, detail string) tools.Result { return refuse(gitRefusal(class, detail)) }
	pol := tools.PolicyOf(ctx).GitRemote
	need := "fetch"
	if kind == gitPush {
		need = "push"
	}
	if !pol.Allows(need) {
		return deny("git_remote_not_allowed", fmt.Sprintf("this agent's git_remote does not include %q", need))
	}
	src := g.Source
	if src == nil || len(src.Hosts) == 0 || src.Transport == nil {
		return deny("git_network_disabled", "the operator has not enabled git hosts (LOOMCYCLE_GIT_SOURCE_HOSTS)")
	}
	hosts := src.Hosts
	if kind == gitPush {
		if len(g.PushHosts) == 0 {
			return deny("git_push_disabled", "the operator has not enabled push (LOOMCYCLE_GIT_PUSH_HOSTS)")
		}
		hosts = g.PushHosts
	}

	timeout := src.Timeout
	if timeout <= 0 {
		timeout = gitOpTimeoutFallback
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The repository, except for ls_remote, which needs none, and clone,
	// which makes it.
	var (
		repo *gitrepo.Repo
		wt   billy.Filesystem
	)
	if in.Op != "ls_remote" {
		vfs, rel, release, res, ok := g.repoDir(ctx, in, true, in.Op == "clone")
		if !ok {
			return refuse(res)
		}
		defer release()
		var err error
		if wt, err = newVolumeBilly(vfs, rel); err != nil {
			return deny("invalid_argument", err.Error())
		}
		if in.Op == "clone" {
			if ents, err := wt.ReadDir("/"); err != nil || len(ents) > 0 {
				return deny("already_exists", "clone needs an empty or new directory")
			}
		} else if repo, err = gitrepo.Open(wt); err != nil {
			return refuse(gitLocalRefusal(err))
		} else {
			repo.SetLimits(g.limits())
		}
	}

	// The URL to dial: given (clone, ls_remote) or the remote's, read as the
	// library would read it, rewrites included.
	remote := in.Remote
	if remote == "" {
		remote = "origin"
	}
	rawURL := in.URL
	if repo != nil {
		if in.URL != "" {
			return deny("invalid_argument", "url is for clone and ls_remote; fetch, pull and push use the remote's URL")
		}
		u, err := repo.RemoteURL(remote)
		if err != nil {
			return refuse(gitLocalRefusal(err))
		}
		rawURL = u
	}
	ev.GitRepository = redactURLPath(rawURL)
	host, err := gitsource.ValidateURL(rawURL)
	if err != nil {
		return deny("invalid_argument", strings.TrimPrefix(err.Error(), gitsource.ErrInvalidRequest.Error()+": "))
	}
	ev.GitHost = host
	if !hostListed(hosts, host) {
		return deny("host_not_allowed", "")
	}

	// The credential, resolved for this run and this URL only.
	var username, token string
	if in.Credential != "" {
		resolve := src.Credential
		if kind == gitPush {
			resolve = g.PushCredential
		}
		if resolve == nil {
			return deny("credential_unresolvable", "")
		}
		username, token, err = resolve(ctx, in.Credential, rawURL)
		switch {
		case errors.Is(err, ErrGitCredentialURLNotAllowed):
			return deny("credential_url_not_allowed", "")
		case errors.Is(err, errGitCredentialIsAppConfig):
			return deny("credential_unresolvable", errGitCredentialIsAppConfig.Error())
		case err != nil:
			log.Printf("git: credential for host %s could not be resolved: %v", host, err)
			return deny("credential_unresolvable", "")
		}
	}

	if src.Slots != nil {
		select {
		case src.Slots <- struct{}{}:
			defer func() { <-src.Slots }()
		case <-ctx.Done():
			return deny("timeout", "waiting for a free git slot")
		}
	}
	spool, err := os.MkdirTemp("", "loomcycle-git-")
	if err != nil {
		return deny("git_failed", "spool directory")
	}
	defer os.RemoveAll(spool)

	n := gitrepo.Network{
		URL: rawURL, Username: username, Token: token,
		Guard: gitsource.Guard{
			Host: host, Transport: src.Transport,
			MaxReadBytes: src.MaxBytes, MaxWriteBytes: gitFetchUploadBytes,
		},
		MaxObjectBytes: src.MaxObjectBytes,
		SpoolDir:       spool,
	}
	if kind == gitPush {
		n.Guard.MaxWriteBytes = g.PushMaxBytes
		if n.Guard.MaxWriteBytes <= 0 {
			n.Guard.MaxWriteBytes = gitDefaultPushBytes
		}
	}

	out, err := g.runNetwork(ctx, in, repo, wt, n, remote)
	if err != nil {
		class := gitrepo.Class(err)
		ev.GitOutcome = class
		g.record(ev)
		// The detail can hold a server's words: the operator's log only.
		log.Printf("git: %s %s on %s failed: %v", in.Op, redactURLPath(rawURL), host, err)
		if class == gitrepo.ErrNotFastForward.Error() || class == gitrepo.ErrWorktreeDirty.Error() ||
			class == gitrepo.ErrInvalidArgument.Error() || class == gitrepo.ErrAlreadyExists.Error() {
			return gitRefusal(class, localDetail(err))
		}
		return gitRefusal(class, "")
	}
	ev.GitOutcome = "ok"
	if refs, ok := out["updated"].([]gitrepo.RefUpdate); ok {
		for _, u := range refs {
			ev.GitRefs = append(ev.GitRefs, u.Name)
		}
	}
	g.record(ev)
	return gitOK(out)
}

func (g *Git) runNetwork(ctx context.Context, in gitInput, repo *gitrepo.Repo, wt billy.Filesystem, n gitrepo.Network, remote string) (map[string]any, error) {
	switch in.Op {
	case "ls_remote":
		refs, err := gitrepo.LsRemote(ctx, n)
		refs, truncated := capList(refs)
		return map[string]any{"op": "ls_remote", "refs": refs, "truncated": truncated}, err
	case "clone":
		r, err := gitrepo.Clone(ctx, wt, n, gitrepo.CloneOptions{
			RemoteName: in.Remote, Branch: in.Branch, Depth: in.Depth,
			SingleBranch: in.SingleBranch, NoCheckout: in.NoCheckout,
			Limits: g.limits(),
		})
		if err != nil {
			return nil, err
		}
		head, _ := r.Head()
		return map[string]any{"op": "clone", "head": head}, nil
	case "fetch":
		res, err := repo.Fetch(ctx, n, gitrepo.FetchOptions{Remote: remote, RefSpecs: in.RefSpecs, Depth: in.Depth, Prune: in.Prune, Tags: in.Tags})
		out := map[string]any{"op": "fetch", "up_to_date": res.UpToDate, "updated": res.Updated}
		if len(res.KeptTags) > 0 {
			out["kept_tags"] = res.KeptTags
		}
		return out, err
	case "pull":
		res, err := repo.Pull(ctx, n, gitrepo.PullOptions{Remote: remote, Branch: in.Branch})
		return map[string]any{"op": "pull", "up_to_date": res.UpToDate, "head": res.Head}, err
	case "push":
		res, err := repo.Push(ctx, n, gitrepo.PushOptions{Remote: remote, RefSpecs: in.RefSpecs, Force: in.Force, ForceWithLease: in.ForceWithLease, Tags: in.Tags})
		return map[string]any{"op": "push", "up_to_date": res.UpToDate, "updated": res.Updated}, err
	}
	return nil, fmt.Errorf("%w: unknown network op %q", gitrepo.ErrInvalidArgument, in.Op)
}

// localDetail is the text of a failure raised in this process before or
// without a server's words (a non-fast-forward, a dirty worktree, a bad
// argument), cut short. Network classes never get here: their detail can
// hold what a server said.
func localDetail(err error) string {
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// auditEvent starts the record of a network operation: who, what, where.
// Never a credential's value; its reference only.
func (g *Git) auditEvent(ctx context.Context, in gitInput, rawURL, host string) audit.Event {
	ident := tools.RunIdentity(ctx)
	ev := audit.Event{
		TS:            time.Now().UTC(),
		Action:        "git_" + in.Op,
		ActorTenant:   ident.TenantID,
		ActorSubject:  ident.UserID,
		TargetTenant:  ident.TenantID,
		GitHost:       host,
		GitRepository: redactURLPath(rawURL),
		GitRunID:      tools.RunID(ctx),
		GitAgent:      tools.AgentName(ctx),
		GitCredential: in.Credential,
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	return ev
}

func (g *Git) record(ev audit.Event) {
	if g.Audit == nil {
		return
	}
	if err := g.Audit.Record(ev); err != nil {
		log.Printf("git: audit record failed: %v", err)
	}
}

// redactURLPath is a URL as it may be logged: host and path, nothing else (a
// validated URL carries no user, query or fragment, but a failed one might).
func redactURLPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	return u.Host + u.Path
}
