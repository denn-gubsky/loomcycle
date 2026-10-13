// Package gitsource fetches one commit of a git repository over HTTPS and
// writes its tree to a directory, without running git and without handing
// the credential to any other process.
//
// It exists so an agent can be given a repository to read while never
// holding what opens it. The alternative, a `git clone` run by an agent
// through a shell tool, puts the forge token in that process's environment,
// where any text the agent reads can ask it to print the token.
//
// What the package guarantees, and how:
//
//   - The token goes to one host and nowhere else. Every request of a fetch
//     passes through this package's transport, which refuses a request to any
//     other host and follows no redirect.
//   - Nothing from the repository is executed. The library used (go-git) runs
//     no hook, filter driver, LFS step or submodule fetch on this path, and
//     the working tree is written here, file by file, not by a checkout.
//   - What is written stays inside the destination directory: see WriteTree.
//   - The work is bounded: bytes fetched, bytes written, files, and time
//     (the context).
//
// The package knows nothing of volumes, credentials or agents. A caller
// resolves the credential, chooses the directories and the limits, and
// decides who may ask.
package gitsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// The classes of failure a caller may report. They are a closed list so that
// what reaches a model is one of these words and never a remote's own text,
// which can echo a request. The wrapped detail is for the operator's log.
var (
	ErrInvalidRequest = errors.New("invalid_request")
	ErrAuthFailed     = errors.New("auth_failed")
	ErrNotFound       = errors.New("not_found")
	ErrCommitNotFound = errors.New("commit_not_found")
	ErrTooLarge       = errors.New("source_too_large")
	ErrTimeout        = errors.New("timeout")
	ErrFetchFailed    = errors.New("fetch_failed")
)

// Class returns the class of an error from this package: one of the Err*
// values' text, or "fetch_failed" for anything else.
func Class(err error) string {
	for _, c := range []error{ErrInvalidRequest, ErrAuthFailed, ErrNotFound, ErrCommitNotFound, ErrTooLarge, ErrTimeout} {
		if errors.Is(err, c) {
			return c.Error()
		}
	}
	return ErrFetchFailed.Error()
}

// Request names what to fetch and how far the fetch may go.
type Request struct {
	// URL is the repository: https only, no user or password, no query.
	URL string
	// Commit is the full sha (40 or 64 hex) of the commit to fetch.
	Commit string
	// Ref is where to look for Commit when the server will not serve a
	// commit by its sha ("refs/heads/x"). Optional.
	Ref string
	// Username and Token are sent as HTTP basic auth to the repository's
	// host. An empty Token fetches anonymously.
	Username, Token string
	// StoreDir receives git's objects. It must not be inside the directory
	// the tree is written to, so no reader of the tree ever sees a .git. It
	// is created here; removing it is the caller's job (Fetched.Close).
	StoreDir string
	// Transport performs the requests. Required: the caller supplies the one
	// that enforces its network rules (which addresses may be dialled).
	Transport http.RoundTripper
	// MaxFetchBytes bounds the response bytes of the whole fetch. Required.
	MaxFetchBytes int64
	// MaxHistory bounds how many commits deep the fallback by Ref looks for
	// Commit. Zero means 512.
	MaxHistory int
}

// Fetched is a commit that has been fetched and can be written out.
type Fetched struct {
	repo     *git.Repository
	storeDir string
	// Head is the fetched commit's sha.
	Head string
	// FetchedBytes is how many response bytes the fetch read.
	FetchedBytes int64
	commit       *object.Commit
}

// Close removes the object store. Safe to call more than once.
func (f *Fetched) Close() error {
	if f == nil || f.storeDir == "" {
		return nil
	}
	dir := f.storeDir
	f.storeDir = ""
	return os.RemoveAll(dir)
}

var shaRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// validate checks a request without touching the network and returns the
// repository's host.
func (r Request) validate() (host string, err error) {
	bad := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, a...)...)
	}
	u, perr := url.Parse(r.URL)
	if perr != nil || u.Scheme != "https" || u.Host == "" {
		return bad("the repository URL must be an https URL")
	}
	if u.User != nil {
		return bad("the repository URL must not carry a user or a password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return bad("the repository URL must not carry a query or a fragment")
	}
	if !shaRe.MatchString(r.Commit) {
		return bad("commit must be a full lower-case sha")
	}
	if r.Ref != "" && (!strings.HasPrefix(r.Ref, "refs/") || strings.ContainsAny(r.Ref, " :^~?*[\\\x00") || strings.Contains(r.Ref, "..")) {
		return bad("ref must be a full ref name such as refs/heads/main")
	}
	if r.StoreDir == "" || r.Transport == nil || r.MaxFetchBytes <= 0 {
		return bad("a store directory, a transport and a fetch limit are required")
	}
	return strings.ToLower(u.Host), nil
}

// Fetch fetches Request.Commit into Request.StoreDir.
//
// It asks for the commit by its sha at depth 1, which is all a tree needs. A
// server that refuses that is asked for Request.Ref instead, deepening until
// the commit is there or MaxHistory is reached.
//
// On any error the store directory is removed.
func Fetch(ctx context.Context, req Request) (_ *Fetched, err error) {
	host, err := req.validate()
	if err != nil {
		return nil, err
	}
	installTransport()
	st := &call{rt: req.Transport, host: host, max: req.MaxFetchBytes}
	ctx = context.WithValue(ctx, callKey{}, st)

	if err := os.MkdirAll(req.StoreDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: store directory: %v", ErrFetchFailed, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(req.StoreDir)
		}
	}()
	repo, err := git.PlainInit(req.StoreDir, true)
	if err != nil {
		return nil, fmt.Errorf("%w: init store: %v", ErrFetchFailed, err)
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{req.URL}})
	if err != nil {
		return nil, fmt.Errorf("%w: remote: %v", ErrFetchFailed, err)
	}
	var auth transport.AuthMethod
	if req.Token != "" {
		auth = &githttp.BasicAuth{Username: req.Username, Password: req.Token}
	}
	fetch := func(spec string, depth int) error {
		ferr := remote.FetchContext(ctx, &git.FetchOptions{
			RefSpecs: []config.RefSpec{config.RefSpec(spec)},
			Depth:    depth,
			Tags:     git.NoTags,
			Force:    true,
			Auth:     auth,
		})
		if errors.Is(ferr, git.NoErrAlreadyUpToDate) {
			return nil
		}
		return ferr
	}
	hash := plumbing.NewHash(req.Commit)

	ferr := fetch(req.Commit+":refs/loomcycle/head", 1)
	if ferr != nil {
		c := classify(ctx, st, ferr)
		if !errors.Is(c, ErrFetchFailed) || !shaNotServed(ferr) {
			// Auth, size, time, a repository that is not there, or a failure
			// that says nothing about the commit (a refused connection). The
			// fallback cannot help, and calling it "commit not found" would
			// send the caller looking in the wrong place.
			return nil, c
		}
		if req.Ref == "" {
			return nil, fmt.Errorf("%w: the server did not serve the commit by its sha, and no ref was given: %v", ErrCommitNotFound, ferr)
		}
		// The server may simply not serve a commit by sha. Look for it on the
		// ref, a little deeper each time.
		maxHistory := req.MaxHistory
		if maxHistory <= 0 {
			maxHistory = 512
		}
		found := false
		for depth := 1; ; depth *= 8 {
			if depth > maxHistory {
				depth = maxHistory
			}
			if rerr := fetch(req.Ref+":refs/loomcycle/head", depth); rerr != nil {
				return nil, classify(ctx, st, rerr)
			}
			if _, cerr := repo.CommitObject(hash); cerr == nil {
				found = true
				break
			}
			if depth >= maxHistory {
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: not within %d commits of %s", ErrCommitNotFound, maxHistory, req.Ref)
		}
	}
	// A reader that stopped looking at errors once it had what it needed
	// would let a fetch over its limit end "successfully". The limit is the
	// caller's promise about memory and disk, so it is checked here too.
	if st.over.Load() {
		return nil, fmt.Errorf("%w: the fetch passed %d bytes", ErrTooLarge, st.max)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("%w: the fetch did not deliver the commit: %v", ErrCommitNotFound, err)
	}
	return &Fetched{repo: repo, storeDir: req.StoreDir, Head: req.Commit, FetchedBytes: st.read.Load(), commit: commit}, nil
}

// shaNotServed reports whether a fetch by sha failed because of the commit
// itself: the server does not serve commits by sha at all, or does and has
// no such commit it will give out. Those two are the cases a ref can help
// with. The second has no error value in the library, only the server's
// message.
func shaNotServed(err error) bool {
	return errors.Is(err, git.ErrExactSHA1NotSupported) || strings.Contains(err.Error(), "not our ref")
}

// classify turns an error of a fetch into one of the package's classes,
// keeping the original as detail for the operator.
func classify(ctx context.Context, st *call, err error) error {
	switch {
	case errors.Is(err, errOverLimit) || st.over.Load():
		return fmt.Errorf("%w: the fetch passed %d bytes", ErrTooLarge, st.max)
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	case errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("%w: cancelled: %v", ErrFetchFailed, err)
	case errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w: %v", ErrAuthFailed, err)
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return fmt.Errorf("%w: %v", ErrFetchFailed, err)
}
