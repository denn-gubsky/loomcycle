package gitrepo

import (
	"errors"
	"fmt"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TagOptions are TagCreate's.
type TagOptions struct {
	Name string
	Rev  string // "" = HEAD
	// Message makes an annotated tag; empty makes a lightweight one.
	Message string
	// Tagger is required for an annotated tag: the library would otherwise
	// read it from the user's and the system's git config.
	Tagger Signature
	// Signer signs an annotated tag; nil leaves it unsigned.
	Signer git.Signer
	// Force replaces a tag of the same name.
	Force bool
}

// validTagName checks a short tag name.
func validTagName(name string) error {
	if validBranchName(name) != nil || plumbing.NewTagReferenceName(name).Validate() != nil {
		return fmt.Errorf("%w: invalid tag name %q", ErrInvalidArgument, truncate(name, 200))
	}
	return nil
}

// TagCreate tags the commit rev names. The tag object, when there is one,
// is built here rather than by the library, whose tags are signed only with
// an OpenPGP key it holds; o.Signer is handed the tag's encoding without a
// signature, as git signs it, and what it returns is stored as the tag's
// signature.
func (r *Repo) TagCreate(o TagOptions) (TagInfo, error) {
	if err := validTagName(o.Name); err != nil {
		return TagInfo{}, err
	}
	annotated := o.Message != ""
	if annotated && strings.TrimSpace(o.Message) == "" {
		return TagInfo{}, fmt.Errorf("%w: a tag message must have text", ErrInvalidArgument)
	}
	if !annotated && o.Signer != nil {
		return TagInfo{}, fmt.Errorf("%w: only an annotated tag (one with a message) can be signed", ErrInvalidArgument)
	}
	ref := plumbing.NewTagReferenceName(o.Name)
	if _, err := r.repo.Reference(ref, false); err == nil && !o.Force {
		return TagInfo{}, fmt.Errorf("%w: tag %s exists", ErrAlreadyExists, o.Name)
	}
	rev := o.Rev
	if rev == "" {
		rev = "HEAD"
	}
	c, err := r.resolveCommit(rev)
	if err != nil {
		return TagInfo{}, err
	}
	info := TagInfo{Name: o.Name, Hash: c.Hash.String(), Annotated: annotated}
	target := c.Hash
	if annotated {
		tagger, err := signature(o.Tagger, "tagger")
		if err != nil {
			return TagInfo{}, err
		}
		tag := &object.Tag{
			Name:       o.Name,
			Tagger:     *tagger,
			Message:    strings.TrimSpace(o.Message) + "\n", // as git writes one
			TargetType: plumbing.CommitObject,
			Target:     c.Hash,
		}
		if o.Signer != nil {
			payload := &plumbing.MemoryObject{}
			if err := tag.EncodeWithoutSignature(payload); err != nil {
				return TagInfo{}, fmt.Errorf("encode the tag: %w", err)
			}
			rd, err := payload.Reader()
			if err != nil {
				return TagInfo{}, fmt.Errorf("encode the tag: %w", err)
			}
			sig, err := o.Signer.Sign(rd)
			if err != nil {
				return TagInfo{}, fmt.Errorf("sign the tag: %w", err)
			}
			tag.PGPSignature = string(sig)
		}
		obj := r.repo.Storer.NewEncodedObject()
		if err := tag.Encode(obj); err != nil {
			return TagInfo{}, fmt.Errorf("encode the tag: %w", err)
		}
		if target, err = r.repo.Storer.SetEncodedObject(obj); err != nil {
			return TagInfo{}, fmt.Errorf("store the tag: %w", err)
		}
		info.Message = tag.Message
	}
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(ref, target)); err != nil {
		return TagInfo{}, fmt.Errorf("create tag: %w", err)
	}
	return info, nil
}

// TagDelete deletes a tag.
func (r *Repo) TagDelete(name string) error {
	if err := validTagName(name); err != nil {
		return err
	}
	ref := plumbing.NewTagReferenceName(name)
	if _, err := r.repo.Reference(ref, false); err != nil {
		return fmt.Errorf("%w: no tag %s", ErrInvalidArgument, name)
	}
	if err := r.repo.Storer.RemoveReference(ref); err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	return nil
}

// checkRemoteURL is a sanity check only: the caller validates what it will
// dial, and nothing here dials a configured URL.
func checkRemoteURL(url string) error {
	if url == "" || len(url) > maxArgLen || hasControl(url) {
		return fmt.Errorf("%w: invalid remote URL", ErrInvalidArgument)
	}
	return nil
}

// RemoteAdd configures a remote with url, stored as given, and git's
// default fetch refspec.
func (r *Repo) RemoteAdd(name, url string) error {
	if err := validRemoteName(name); err != nil {
		return err
	}
	if err := checkRemoteURL(url); err != nil {
		return err
	}
	_, err := r.repo.CreateRemote(&config.RemoteConfig{Name: name, URLs: []string{url}})
	if errors.Is(err, git.ErrRemoteExists) {
		return fmt.Errorf("%w: remote %s exists", ErrAlreadyExists, name)
	}
	if err != nil {
		return fmt.Errorf("add remote: %w", err)
	}
	return nil
}

// RemoteRemove removes a remote as `git remote remove` does: its section of
// the config, its remote-tracking refs, and the upstream settings of the
// branches that tracked it.
func (r *Repo) RemoteRemove(name string) error {
	if err := validRemoteName(name); err != nil {
		return err
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if _, ok := cfg.Remotes[name]; !ok {
		return fmt.Errorf("%w: no remote %q", ErrInvalidArgument, name)
	}
	delete(cfg.Remotes, name)
	for _, b := range cfg.Branches {
		if b.Remote == name {
			b.Remote, b.Merge = "", ""
		}
	}
	if err := r.repo.Storer.SetConfig(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	refs, err := r.refSnapshot()
	if err != nil {
		return err
	}
	prefix := "refs/remotes/" + name + "/"
	for ref := range refs {
		if strings.HasPrefix(ref.String(), prefix) {
			if err := r.repo.Storer.RemoveReference(ref); err != nil {
				return fmt.Errorf("remove %s: %w", ref, err)
			}
		}
	}
	return nil
}

// RemoteSetURL replaces every URL of a remote, push URLs among them, with
// url. The remote's section is rebuilt with its fetch refspecs; options
// this package does not model (tagopt, prune, ...) are dropped. The
// library's own section keeps the URLs it read before an insteadOf rewrite
// and would write those back, ignoring the new one.
func (r *Repo) RemoteSetURL(name, url string) error {
	if err := validRemoteName(name); err != nil {
		return err
	}
	if err := checkRemoteURL(url); err != nil {
		return err
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	rc, ok := cfg.Remotes[name]
	if !ok {
		return fmt.Errorf("%w: no remote %q", ErrInvalidArgument, name)
	}
	cfg.Remotes[name] = &config.RemoteConfig{Name: name, URLs: []string{url}, Fetch: rc.Fetch, Mirror: rc.Mirror}
	if err := r.repo.Storer.SetConfig(cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
