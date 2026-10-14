package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A git source fills a new ephemeral volume with one commit of a repository.
//
// The point of doing it here, in the tool, and not by an agent running
// `git clone` through a shell: the forge token is then only ever a value in
// this process, sent to one host. An agent asks for a repository and never
// holds what opens it. Everything that makes that true for the fetch itself
// is in internal/gitsource; this file decides WHO may ask, for WHAT, and
// leaves nothing behind when it goes wrong.

// GitSourceScope is the volume_def_scopes value that lets an agent create a
// volume from a git source. The value "any" does not imply it: an agent that
// could make scratch volumes before this existed did not thereby gain a way
// to make the server fetch from the network.
const GitSourceScope = "source:git"

// gitSourceMetaDir is the top-level directory of a git-sourced volume that
// belongs to the runtime. A path of the commit under this name is left out.
const gitSourceMetaDir = "_meta"

// GitCredentialFunc turns a credential reference ("$cred:<name>" or
// "$ghapp:<name>") into the user name and token to send to targetURL, for the
// run on ctx. It must refuse a credential that is not restricted to URLs
// covering targetURL: the URL of a clone can come from untrusted text, and
// the restriction is what stops the token following it.
type GitCredentialFunc func(ctx context.Context, ref, targetURL string) (username, token string, err error)

// The two refusals a GitCredentialFunc reports by class. Anything else it
// returns is reported as credential_unresolvable.
var (
	ErrGitCredentialURLNotAllowed = errors.New("credential_url_not_allowed")
	ErrGitCredentialUnresolvable  = errors.New("credential_unresolvable")
)

// GitSourceConfig is what the operator allows. A nil config, or one with no
// hosts, means the feature is off.
type GitSourceConfig struct {
	// Hosts are the hosts a repository may be fetched from, as "host" or
	// "host:port", compared without regard to case.
	Hosts []string
	// MaxBytes bounds the bytes fetched and, separately, the bytes written.
	// A request may ask for less, never more.
	MaxBytes int64
	// MaxObjectBytes bounds any one object once inflated; it is what bounds
	// the memory a fetch can take.
	MaxObjectBytes int64
	// MaxFileBytes is the largest file written; a larger one is left out.
	MaxFileBytes int64
	// MaxFiles bounds the files, links and submodule directories written.
	MaxFiles int
	// MaxHistory bounds how many commits deep a clone looks for a commit or
	// a merge base. Zero means the package's default.
	MaxHistory int
	// Timeout bounds one clone, waiting for a slot included.
	Timeout time.Duration
	// Slots limits how many clones run at once: a clone holds one slot.
	Slots chan struct{}
	// Transport performs the requests. It is where the operator's network
	// rules live (no private addresses).
	Transport http.RoundTripper
	// Credential resolves a credential reference. Nil refuses every request
	// that names one.
	Credential GitCredentialFunc
}

var gitCredentialRefRe = regexp.MustCompile(`^\$(cred|ghapp):([A-Za-z0-9_-]{1,128})$`)

// GitCredentialResolver builds the GitCredentialFunc the runtime uses.
//
// A reference resolves, for the run's own identity, only when the stored
// credential is restricted to URLs and the clone's URL is under one of them.
// An unrestricted credential is refused: its owner never said where it may
// go, and the URL of a clone is not something to take on trust.
//
// "$ghapp:<name>" names a stored GitHub App config; mintApp turns it into a
// short-lived installation token for the one repository being cloned, with
// read access to its contents only (GitHubAppMinter.TokenForRepository).
// register is called with the token so the transcript redactor masks any
// later echo of it.
func GitCredentialResolver(
	engine *credential.Engine,
	mintApp func(ctx context.Context, tenantID, cfgJSON, repository string) (string, error),
	register func(string),
) GitCredentialFunc {
	return func(ctx context.Context, ref, targetURL string) (string, string, error) {
		m := gitCredentialRefRe.FindStringSubmatch(ref)
		if m == nil {
			return "", "", fmt.Errorf("%w: a credential is named as $cred:<name> or $ghapp:<name>", ErrGitCredentialUnresolvable)
		}
		kind, name := m[1], m[2]
		ri := tools.RunIdentity(ctx)
		// The fetch's own transport follows no redirect at all (gitsource), so
		// the pin a restricted credential asks for is already kept.
		res, found, err := engine.ResolveFor(netguard.WithPinnable(ctx), ri.TenantID, tools.AgentName(ctx), ri.UserID, name, targetURL)
		switch {
		case errors.Is(err, credential.ErrURLNotAllowed):
			return "", "", ErrGitCredentialURLNotAllowed
		case err != nil:
			return "", "", fmt.Errorf("%w: %v", ErrGitCredentialUnresolvable, err)
		case !found:
			return "", "", fmt.Errorf("%w: no such credential for this run", ErrGitCredentialUnresolvable)
		case res.AllowedURLs == nil:
			// Stored with no allowed_urls at all.
			return "", "", ErrGitCredentialURLNotAllowed
		}
		u, perr := url.Parse(targetURL)
		if perr != nil {
			return "", "", fmt.Errorf("%w: %v", ErrGitCredentialUnresolvable, perr)
		}
		token := res.Value
		if kind == "ghapp" {
			if mintApp == nil {
				return "", "", fmt.Errorf("%w: GitHub App credentials are not available", ErrGitCredentialUnresolvable)
			}
			if token, err = mintApp(ctx, ri.TenantID, res.Value, repositoryName(u.Path)); err != nil {
				return "", "", fmt.Errorf("%w: %v", ErrGitCredentialUnresolvable, err)
			}
		}
		if register != nil {
			register(token)
		}
		return gitAuthUsername(u.Hostname()), token, nil
	}
}

// repositoryName is the repository's name in a forge URL's path: its last
// segment, without ".git". "/acme/app.git" is "app".
func repositoryName(urlPath string) string {
	segs := strings.Split(strings.Trim(urlPath, "/"), "/")
	return strings.TrimSuffix(segs[len(segs)-1], ".git")
}

// gitAuthUsername is the basic-auth user name sent with a token. Forges take
// the token as the password and differ on what the user name should be:
// GitHub documents "x-access-token" for an App's token, and accepts any name
// with a personal one; GitLab documents "oauth2". Gitea accepts a token with
// either. None of this has been run against a private repository yet.
func gitAuthUsername(host string) string {
	for _, label := range strings.Split(strings.ToLower(host), ".") {
		if label == "gitlab" {
			return "oauth2"
		}
	}
	return "x-access-token"
}

// gitSourceInput is the `source` argument of a create. Decoded strictly: a
// field this build does not know is refused, so a request written for a later
// version (a diff base, say) is not carried out as if the field were not there.
type gitSourceInput struct {
	Git *struct {
		URL        string `json:"url"`
		Commit     string `json:"commit"`
		Ref        string `json:"ref"`
		Credential string `json:"credential"`
		MaxBytes   int64  `json:"max_bytes"`
		// DiffBase asks for a diff: "merge-base:default", or a full sha.
		DiffBase string `json:"diff_base"`
		// Outputs names the diff's two files inside the volume. Both must be
		// under _meta/, the runtime's own directory of the volume.
		Outputs *struct {
			Diff    string `json:"diff"`
			Numstat string `json:"numstat"`
		} `json:"outputs"`
	} `json:"git"`
}

// gitSourceRefusal is a refusal that names its class, the one word a model
// is given about what went wrong.
func gitSourceRefusal(class, detail string) tools.Result {
	if detail == "" {
		return errResult("create: git source: " + class)
	}
	return errResult("create: git source: " + class + ": " + detail)
}

// execCreateFromGit creates the ephemeral volume `name` holding one commit.
// It is all or nothing: on any failure the directory, the object store and
// any row are gone and the name is free again.
func (v *VolumeDef) execCreateFromGit(ctx context.Context, in volumeDefInput, mode string) (tools.Result, error) {
	// ---- checks that touch neither the disk nor the network ----
	cfg := v.Git
	if cfg == nil || len(cfg.Hosts) == 0 || cfg.Transport == nil {
		return gitSourceRefusal("git_source_disabled", "the operator has not enabled git sources (LOOMCYCLE_GIT_SOURCE_HOSTS)"), nil
	}
	if !volumeScopeGranted(tools.VolumeDefPolicy(ctx).Scopes, GitSourceScope) {
		return errResult(fmt.Sprintf("VolumeDef tool: creating a volume from a git source needs %q in this agent's volume_def_scopes", GitSourceScope)), nil
	}
	if !in.Ephemeral {
		return gitSourceRefusal("invalid_request", "a git source is for an ephemeral volume; pass ephemeral: true"), nil
	}
	var src gitSourceInput
	dec := json.NewDecoder(strings.NewReader(string(in.Source)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&src); err != nil {
		return gitSourceRefusal("invalid_request", "source: "+err.Error()), nil
	}
	if src.Git == nil {
		return gitSourceRefusal("invalid_request", `source needs a "git" object`), nil
	}
	g := src.Git
	u, err := url.Parse(g.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return gitSourceRefusal("invalid_request", "url must be an https URL"), nil
	}
	if !hostListed(cfg.Hosts, u.Host) {
		return gitSourceRefusal("host_not_allowed", ""), nil
	}
	diffRel, numstatRel := gitSourceMetaDir+"/diff.patch", gitSourceMetaDir+"/numstat.json"
	if g.Outputs != nil {
		if g.DiffBase == "" {
			return gitSourceRefusal("invalid_request", "outputs names the diff's files; it needs diff_base"), nil
		}
		if g.Outputs.Diff != "" {
			diffRel = g.Outputs.Diff
		}
		if g.Outputs.Numstat != "" {
			numstatRel = g.Outputs.Numstat
		}
	}
	if g.DiffBase != "" {
		for _, rel := range []string{diffRel, numstatRel} {
			if !validMetaOutput(rel) {
				return gitSourceRefusal("invalid_request", fmt.Sprintf("an output must be a plain file name under %s/ (got %q)", gitSourceMetaDir, rel)), nil
			}
		}
		if diffRel == numstatRel {
			return gitSourceRefusal("invalid_request", "the two outputs must be different files"), nil
		}
	}
	maxBytes := cfg.MaxBytes
	if g.MaxBytes < 0 {
		return gitSourceRefusal("invalid_request", "max_bytes must be positive"), nil
	}
	if g.MaxBytes > 0 && g.MaxBytes < maxBytes {
		maxBytes = g.MaxBytes // a request may lower the limit, never raise it
	}

	rootRunID := tools.RunIdentity(ctx).RootRunID
	set := tools.EphemeralVolumes(ctx)
	if rootRunID == "" || set == nil {
		return errResult("create: ephemeral volumes require an active run"), nil
	}
	if res, refused := v.refuseEphemeralDuplicate(ctx, set, rootRunID, in.Name); refused {
		return res, nil
	}

	var username, token string
	if g.Credential != "" {
		if cfg.Credential == nil {
			return gitSourceRefusal("credential_unresolvable", ""), nil
		}
		username, token, err = cfg.Credential(ctx, g.Credential, g.URL)
		switch {
		case errors.Is(err, ErrGitCredentialURLNotAllowed):
			return gitSourceRefusal("credential_url_not_allowed", ""), nil
		case err != nil:
			log.Printf("volumedef: git source: credential for host %s could not be resolved: %v", u.Host, err)
			return gitSourceRefusal("credential_unresolvable", ""), nil
		}
	}

	// ---- from here on there is something to undo ----
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if cfg.Slots != nil {
		select {
		case cfg.Slots <- struct{}{}:
			defer func() { <-cfg.Slots }()
		case <-ctx.Done():
			return gitSourceRefusal("timeout", "waiting for another clone to finish"), nil
		}
	}

	dynRoot, err := v.dynamicRoot()
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	path, undo, err := dynvol.ProvisionEphemeral(dynRoot, rootRunID, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	// The object store is a sibling of the volume, under the run's own
	// directory, so the run's purge removes it if this process dies here. Its
	// name starts with a dot, which no volume name can, so no volume name
	// resolves to it.
	storeDir := filepath.Join(filepath.Dir(path), ".gitstore-"+in.Name)
	cleanup := func() {
		_ = os.RemoveAll(storeDir)
		// The volume's directory, removed whole: it holds a partly written
		// tree. Fenced like every other delete of a volume: the path is
		// re-checked to be strictly inside this run's ephemeral directory.
		if _, err := fencedRemoveAll(dynRoot, filepath.Dir(path), path, "git source cleanup"); err != nil {
			log.Printf("volumedef: git source: cleanup of %s: %v", in.Name, err)
		}
		undo()
	}
	fail := func(err error) (tools.Result, error) {
		cleanup()
		class := gitsource.Class(err)
		// The remote's own words can echo the request, so the model gets the
		// class and the operator the detail, with the repository's path cut
		// out of it: a no-trace run's repository is not for the log either.
		log.Printf("volumedef: git source: host=%s class=%s: %s", u.Host, class, strings.ReplaceAll(err.Error(), g.URL, "https://"+u.Host+"/<repository>"))
		return gitSourceRefusal(class, ""), nil
	}

	fetched, err := gitsource.Fetch(ctx, gitsource.Request{
		URL: g.URL, Commit: g.Commit, Ref: g.Ref,
		Username: username, Token: token,
		StoreDir:       storeDir,
		Transport:      cfg.Transport,
		MaxFetchBytes:  maxBytes,
		MaxObjectBytes: cfg.MaxObjectBytes,
		MaxHistory:     cfg.MaxHistory,
		DiffBase:       g.DiffBase,
	})
	if err != nil {
		return fail(err)
	}
	tree, err := fetched.WriteTree(ctx, path, gitsource.TreeLimits{
		MaxBytes: maxBytes, MaxFiles: cfg.MaxFiles, MaxFileBytes: cfg.MaxFileBytes,
		Reserved: []string{gitSourceMetaDir},
	})
	if err != nil {
		_ = fetched.Close()
		return fail(err)
	}
	var diff gitsource.DiffResult
	if g.DiffBase != "" {
		// The patch shares the volume's byte limit with the tree: what is
		// written in all stays under it.
		diff, err = fetched.WriteDiff(ctx, path, diffRel, numstatRel, gitsource.DiffLimits{
			MaxBytes: maxBytes - tree.Bytes, MaxFileBytes: cfg.MaxFileBytes, MaxFiles: cfg.MaxFiles,
			Reserved: []string{gitSourceMetaDir},
		})
		if err != nil {
			_ = fetched.Close()
			return fail(err)
		}
	}
	_ = fetched.Close()
	omittedPath := gitSourceMetaDir + "/omitted.json"
	if err := writeOmitted(path, omittedPath, tree); err != nil {
		return fail(fmt.Errorf("write %s: %w", omittedPath, err))
	}

	res, err := v.recordEphemeral(ctx, set, rootRunID, in.Name, path, mode)
	if err != nil {
		cleanup()
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	outputs := map[string]any{"omitted": omittedPath}
	source := map[string]any{
		"head":    fetched.Head,
		"files":   tree.Files,
		"bytes":   tree.Bytes,
		"omitted": tree.OmittedCount,
		"outputs": outputs,
	}
	if g.DiffBase != "" {
		source["base"] = fetched.Base
		source["changed_files"] = diff.Files
		outputs["diff"] = diffRel
		outputs["numstat"] = numstatRel
	}
	res["source"] = source
	return okJSON(res)
}

// writeOmitted writes the list of what was left out of the tree, so an agent
// reading the volume can tell "not in the commit" from "not written".
func writeOmitted(volumeRoot, rel string, tree gitsource.TreeResult) error {
	root, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	omitted := tree.Omitted
	if omitted == nil {
		omitted = []gitsource.Omitted{}
	}
	body, err := json.MarshalIndent(map[string]any{
		"count":   tree.OmittedCount,
		"listed":  len(omitted),
		"omitted": omitted,
	}, "", "  ")
	if err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// validMetaOutput reports whether rel names a file directly under the
// runtime's own directory of the volume, other than the list of omitted
// paths. The commit's paths can never be written there, so an output cannot
// collide with one, nor be passed off as one.
func validMetaOutput(rel string) bool {
	name, ok := strings.CutPrefix(rel, gitSourceMetaDir+"/")
	if !ok || name == "" || name == "omitted.json" || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return len(name) <= 64
}

func volumeScopeGranted(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// hostListed reports whether host ("name" or "name:port") is on the
// operator's list. An entry without a port covers the host on the default
// port only; another port has to be listed with it.
func hostListed(hosts []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, ":443"))
	for _, h := range hosts {
		if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ":443")) == host {
			return true
		}
	}
	return false
}
