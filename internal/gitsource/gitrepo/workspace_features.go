package gitrepo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5"
	formatcfg "github.com/go-git/go-git/v5/plumbing/format/config"
)

// ErrUnsupportedFeature is a workspace repository using something the typed
// git ops cannot read (RFC GW §4.2): `git` in the workspace's session turned
// it on, and go-git would fail on it or, worse, misread the repository.
var ErrUnsupportedFeature = errors.New("unsupported_repository_feature")

// FeatureError names the feature and the command that turns it off.
type FeatureError struct {
	Feature string
	Undo    string
}

func (e *FeatureError) Error() string {
	return fmt.Sprintf("%s: %s (undo with: %s)", ErrUnsupportedFeature, e.Feature, e.Undo)
}

func (e *FeatureError) Unwrap() error { return ErrUnsupportedFeature }

// maxConfigScan bounds the read of a workspace's .git/config. A config
// git's own commands write is a few hundred bytes.
const maxConfigScan = 64 << 10

// scanWorkspaceFeatures refuses, by name, what go-git cannot open in the
// workspace git directory dot. It only reads: nothing in .git/config is
// applied anywhere (the configuration is the row's, workspace_store.go).
func scanWorkspaceFeatures(dot billy.Filesystem) error {
	// A link where git keeps a directory would carry the store elsewhere;
	// the filesystem refuses to follow one, so name it here first.
	for _, p := range []string{"objects", "refs", "info"} {
		if fi, err := dot.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return &FeatureError{Feature: ".git/" + p + " is a symbolic link", Undo: "replace .git/" + p + " with a directory"}
		}
	}
	for _, f := range []struct{ path, feature, undo string }{
		{"commondir", "a linked worktree (.git/commondir)", "work in the workspace's own checkout"},
		{"reftable", "the reftable ref store", "git refs migrate --ref-format=files"},
		{"info/sparse-checkout", "sparse checkout", "git sparse-checkout disable"},
	} {
		if _, err := dot.Lstat(f.path); err == nil {
			return &FeatureError{Feature: f.feature, Undo: f.undo}
		}
	}
	if ents, err := dot.ReadDir("."); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "sharedindex.") {
				return &FeatureError{Feature: "a split index", Undo: "git update-index --no-split-index"}
			}
		}
	}
	return scanConfig(dot)
}

func scanConfig(dot billy.Filesystem) error {
	f, err := dot.Open("config")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: read .git/config: %v", ErrNotARepository, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigScan+1))
	if err != nil {
		return fmt.Errorf("%w: read .git/config: %v", ErrNotARepository, err)
	}
	if len(b) > maxConfigScan {
		return &FeatureError{Feature: fmt.Sprintf(".git/config over %d KiB", maxConfigScan>>10), Undo: "remove what was added to .git/config"}
	}
	var c formatcfg.Config
	if err := formatcfg.NewDecoder(bytes.NewReader(b)).Decode(&c); err != nil {
		return &FeatureError{Feature: ".git/config that cannot be read", Undo: "fix the syntax of .git/config"}
	}
	for _, s := range c.Sections {
		switch strings.ToLower(s.Name) {
		case "extensions":
			if len(s.Options) > 0 {
				k := s.Options[0].Key
				return &FeatureError{Feature: "extensions." + k, Undo: "git config --unset extensions." + k}
			}
		case "include", "includeif":
			// An included file can set what this scan looks for, and git
			// reads it where this scan does not.
			return &FeatureError{Feature: "an included config file ([" + s.Name + "])", Undo: "remove the [" + s.Name + "] section from .git/config"}
		case "core":
			if v := s.Option("repositoryformatversion"); v != "" {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n > 1 {
					return &FeatureError{Feature: "core.repositoryformatversion " + v, Undo: "git config core.repositoryformatversion 0"}
				}
			}
			if boolOption(s, "sparsecheckout") {
				return &FeatureError{Feature: "sparse checkout", Undo: "git sparse-checkout disable"}
			}
			if boolOption(s, "splitindex") {
				return &FeatureError{Feature: "a split index", Undo: "git update-index --no-split-index"}
			}
			if s.HasOption("worktree") {
				return &FeatureError{Feature: "core.worktree", Undo: "git config --unset core.worktree"}
			}
		}
		for _, sub := range s.Subsections {
			if strings.EqualFold(s.Name, "extensions") || strings.EqualFold(s.Name, "include") {
				return &FeatureError{Feature: s.Name + "." + sub.Name, Undo: "remove it from .git/config"}
			}
		}
	}
	return nil
}

// boolOption reads a git boolean as git does: a key with no value is true.
func boolOption(s *formatcfg.Section, key string) bool {
	if !s.HasOption(key) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s.Option(key))) {
	case "", "true", "yes", "on", "1":
		return true
	}
	return false
}
