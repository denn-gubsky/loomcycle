package worker

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
)

// The bounds of the environment rewrite: a file of a Python environment's
// bin/ is a script or a link, never this big, and a fork holds few
// environments.
const (
	maxVenvFile      = 1 << 20
	maxVenvRewritten = 200
)

// rewriteVenvs moves the absolute paths a Python virtual environment holds
// from the forked workspace's folder (any of from) to the fork's (to), in
// the copy at dir, and returns the files it changed, relative to dir.
//
// Only the host engine needs it (RFC GW §4.3): there a workspace is a folder
// of the worker's machine, and a forked environment's scripts still name
// the original's python, so the fork's pip installed into the original in
// spike F. A container session mounts every workspace at /workspace, so
// its paths are right as they are.
//
// What is rewritten: pyvenv.cfg and the regular files directly in bin/ of
// each folder holding a pyvenv.cfg, under maxVenvFile and with no NUL byte
// (text: shebangs, activate scripts). Links are never followed or changed.
// A fork keeps every path at the same length (workspace ids are), so the
// sizes and the quota count do not move.
func rewriteVenvs(dir string, from []string, to string) []string {
	v, err := fsops.OpenVolumeFS(dir, "")
	if err != nil {
		return nil
	}
	defer v.Close()
	var venvs []string
	walkTree(v, v.Root(), "", func(_ *os.Root, rel string, fi os.FileInfo) bool {
		if fi.Mode().IsRegular() && filepath.Base(rel) == "pyvenv.cfg" {
			venvs = append(venvs, filepath.Dir(rel))
		}
		return true
	}, func(rel string) bool { return rel != workDir })
	var olds [][]byte
	for _, f := range from {
		if f != "" && f != to && !slices.ContainsFunc(olds, func(o []byte) bool { return string(o) == f }) {
			olds = append(olds, []byte(f))
		}
	}
	// The longer first, so a path that holds another is replaced whole.
	slices.SortFunc(olds, func(a, b []byte) int { return len(b) - len(a) })
	var out []string
	for _, venv := range venvs {
		files := []string{filepath.Join(venv, "pyvenv.cfg")}
		if ents, err := readDirIn(v, filepath.Join(venv, "bin")); err == nil {
			for _, e := range ents {
				files = append(files, filepath.Join(venv, "bin", e))
			}
		}
		for _, f := range files {
			if len(out) >= maxVenvRewritten {
				return out
			}
			if rewriteFile(v, f, olds, []byte(to)) {
				out = append(out, filepath.ToSlash(f))
			}
		}
	}
	return out
}

func readDirIn(v *fsops.VolumeFS, rel string) ([]string, error) {
	d, err := v.OpenDir(rel)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}

// rewriteFile replaces each of olds with to in the regular file rel; it
// reports whether it changed the file.
func rewriteFile(v *fsops.VolumeFS, rel string, olds [][]byte, to []byte) bool {
	parent, err := v.OpenDir(filepath.Dir(rel))
	if err != nil {
		return false
	}
	defer parent.Close()
	base := filepath.Base(rel)
	fi, err := parent.Lstat(base)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxVenvFile {
		return false
	}
	in, err := v.OpenIn(parent, base)
	if err != nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(in, maxVenvFile+1))
	in.Close()
	if err != nil || len(body) > maxVenvFile || bytes.IndexByte(body, 0) >= 0 {
		return false
	}
	changed := body
	for _, old := range olds {
		changed = bytes.ReplaceAll(changed, old, to)
	}
	if bytes.Equal(changed, body) {
		return false
	}
	f, err := parent.OpenFile(base, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return false
	}
	_, werr := f.Write(changed)
	cerr := f.Close()
	_ = parent.Chtimes(base, fi.ModTime(), fi.ModTime())
	return werr == nil && cerr == nil
}
