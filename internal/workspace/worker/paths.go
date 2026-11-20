package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// workDir is the hidden folder in a workspace that holds what the raw-byte
// ops have in flight: a put's temporary file (tmpDir) and staging folders
// (stageDir). Keeping them in one place, rather than beside each target,
// is what lets a worker clean up after a crash by removing one folder per
// workspace at start. It is inside the workspace, so a rename from it into
// place stays on one file system and is atomic.
const (
	workDir  = ".loomcycle-work"
	tmpDir   = workDir + "/tmp"
	stageDir = workDir + "/stage"
)

// errLink is a path that crosses a link. The raw-byte ops never follow one,
// in or out of the workspace: a link is reported, not read through.
var errLink = errors.New("is a link; workspace file ops do not follow links")

// errNotFolder is a path whose folder part names a file.
var errNotFolder = errors.New("is not a folder")

// opPath checks a path an op names and returns it cleaned, with the OS
// separator. "" and "." are the root, allowed only when root is true. An
// absolute path, a `..` component, a NUL and the work folder are refused
// here, before anything is opened; links are refused as the path is walked.
func opPath(p string, root bool) (string, *answer) {
	if strings.ContainsRune(p, 0) {
		return "", refusal(workspace.ClassInvalid, "a path may not hold a NUL")
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return "", refusal(workspace.ClassInvalid, "path %q: give a path relative to the workspace, not an absolute one", p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return "", refusal(workspace.ClassInvalid, "path %q: `..` is not allowed", p)
		}
	}
	clean := path.Clean("/" + p)[1:]
	if clean == "" {
		if !root {
			return "", refusal(workspace.ClassInvalid, "the op needs a path in the workspace, not its root")
		}
		return ".", nil
	}
	if clean == workDir || strings.HasPrefix(clean, workDir+"/") {
		return "", refusal(workspace.ClassInvalid, "path %q: %s is the worker's own folder", p, workDir)
	}
	return filepath.FromSlash(clean), nil
}

func refusal(class, format string, args ...any) *answer {
	a := refuse(class, format, args...)
	return &a
}

// openDir opens the folder rel (relative to the workspace's root) one
// component at a time through fsops, refusing a link at any step. With
// create, missing folders are made. The caller closes the result.
func openDir(v *fsops.VolumeFS, rel string, create bool) (*os.Root, error) {
	cur, err := v.Step(v.Root(), ".")
	if err != nil {
		return nil, err
	}
	done := ""
	for _, name := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if name == "." || name == "" {
			continue
		}
		done = filepath.Join(done, name)
		next, err := stepDir(v, cur, name, done, create)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

func stepDir(v *fsops.VolumeFS, parent *os.Root, name, shown string, create bool) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist) && create:
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	case errors.Is(err, fs.ErrNotExist):
		return nil, pathNotFound(shown)
	case err != nil:
		return nil, err
	case fi.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%q %w", filepath.ToSlash(shown), errLink)
	case !fi.IsDir():
		return nil, fmt.Errorf("%q %w", filepath.ToSlash(shown), errNotFolder)
	}
	return v.Step(parent, name)
}

// errPathNotFound marks an error as path_not_found.
type errPathNotFound struct{ path string }

func (e errPathNotFound) Error() string { return fmt.Sprintf("%q is not in the workspace", e.path) }

func pathNotFound(rel string) error { return errPathNotFound{path: filepath.ToSlash(rel)} }

// classify turns an error of the path walk into an answer.
func classify(err error, format string, args ...any) answer {
	var nf errPathNotFound
	msg := fmt.Sprintf(format, args...)
	switch {
	case errors.As(err, &nf):
		return refuse(workspace.ClassPathNotFound, "%s: %v", msg, err)
	case errors.Is(err, fsops.ErrNotRegular):
		return refuse(workspace.ClassNotRegular, "%s: %v", msg, err)
	case errors.Is(err, errLink), errors.Is(err, errNotFolder), errors.Is(err, fsops.ErrPathChanged):
		return refuse(workspace.ClassInvalid, "%s: %v", msg, err)
	}
	return refuse(workspace.ClassFailed, "%s: %v", msg, err)
}

// lstatIn is the entry name in dir, refusing nothing: the caller decides
// what a link or a folder means for its op.
func lstatIn(dir *os.Root, name string) (os.FileInfo, error) {
	fi, err := dir.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return fi, err
}

// randomName is 16 random bytes in hex: a stage's id, a temporary file's
// name.
func randomName() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// validStage reports whether s may be a stage's id (randomName's form). It
// is all a stage's name may hold, so a stage never names a path.
func validStage(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// stagePath is the path of a put or mkdir: in the workspace, or in a stage.
func stagePath(stage, rel string) (string, *answer) {
	if stage == "" {
		return rel, nil
	}
	if !validStage(stage) {
		return "", refusal(workspace.ClassInvalid, "malformed stage id")
	}
	return filepath.Join(filepath.FromSlash(stageDir), stage, rel), nil
}

func entryType(m os.FileMode) string {
	switch {
	case m.IsRegular():
		return workspace.EntryFile
	case m.IsDir():
		return workspace.EntryDir
	case m&os.ModeSymlink != 0:
		return workspace.EntryLink
	}
	return workspace.EntryOther
}

func fileInfo(rel string, fi os.FileInfo) workspace.FileInfo {
	out := workspace.FileInfo{Path: filepath.ToSlash(rel), Type: entryType(fi.Mode()), Mode: uint32(fi.Mode().Perm()), MTime: fi.ModTime().UTC()}
	if fi.Mode().IsRegular() {
		out.Size = fi.Size()
	}
	return out
}
