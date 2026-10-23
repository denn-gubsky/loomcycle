// Package gomodules finds the Go modules this repository builds from: the
// root module, plus every module the root's go.mod replaces with a directory
// inside the tree (RFC FE).
//
// Go tools run from the root see only the root module. `go list ./...`,
// `go test ./...` and `go vet ./...` stop at a directory holding its own
// go.mod, so once a package moves into a nested module, a tool that does not
// visit that module skips it without saying so. Every CI script, the Makefile
// and the layer test therefore iterate the modules this package returns.
//
// The set is read from the root go.mod, not kept as a list: a nested module is
// wired into the build by a `replace <path> => ./<dir>` directive, so the
// directive that makes it part of the binary is also what makes the tooling
// visit it. A module the root does not replace (deploy/builder, a separate
// image) is not part of the build and is not returned.
package gomodules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Module is one in-tree module.
type Module struct {
	// Dir is the module's directory relative to the repository root,
	// slash-separated. The root module's is ".".
	Dir string
	// Path is the module path its go.mod declares.
	Path string
}

// modFile is the subset of `go mod edit -json` this package reads.
type modFile struct {
	Module struct {
		Path string
	}
	Replace []struct {
		Old struct{ Path string }
		New struct{ Path, Version string }
	}
}

// InTree returns the root module first, then every module the root go.mod
// replaces with a directory under root, sorted by directory.
//
// A replacement by a module version, or by a directory outside the tree
// ("../x", an absolute path), is not an in-tree module and is skipped. A
// replacement directory with no go.mod, or whose go.mod declares a module
// path other than the one replaced, is an error: Go refuses to build either,
// and reporting it here names the directive.
func InTree(root string) ([]Module, error) {
	rootMod, err := read(root)
	if err != nil {
		return nil, err
	}
	mods := []Module{{Dir: ".", Path: rootMod.Module.Path}}
	seen := map[string]bool{".": true}
	var nested []Module
	for _, r := range rootMod.Replace {
		dir, ok := inTreeDir(r.New.Path, r.New.Version)
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		m, err := read(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			return nil, fmt.Errorf("go.mod replaces %s with ./%s: %w", r.Old.Path, dir, err)
		}
		if m.Module.Path != r.Old.Path {
			return nil, fmt.Errorf("go.mod replaces %s with ./%s, whose go.mod declares module %s",
				r.Old.Path, dir, m.Module.Path)
		}
		nested = append(nested, Module{Dir: dir, Path: m.Module.Path})
	}
	sort.Slice(nested, func(i, j int) bool { return nested[i].Dir < nested[j].Dir })
	return append(mods, nested...), nil
}

// inTreeDir returns the slash-separated directory a replacement names when it
// is a directory inside the tree. Go reads a replacement as a directory when
// it has no version; a "./" prefix is what keeps it inside the tree.
func inTreeDir(newPath, newVersion string) (string, bool) {
	p := filepath.ToSlash(newPath)
	if newVersion != "" || !strings.HasPrefix(p, "./") {
		return "", false
	}
	dir := path.Clean(p)
	if dir == "." || dir == ".." || strings.HasPrefix(dir, "../") {
		return "", false
	}
	return dir, true
}

func read(dir string) (modFile, error) {
	var m modFile
	file := filepath.Join(dir, "go.mod")
	var stderr bytes.Buffer
	// #nosec G204 -- fixed command and flags; file is a go.mod path, which
	// `go mod edit` reads as its file argument, never as an option.
	cmd := exec.Command("go", "mod", "edit", "-json", file)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return m, fmt.Errorf("go mod edit -json %s: %v: %s", file, err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(out, &m); err != nil {
		return m, fmt.Errorf("decode go mod edit -json %s: %v", file, err)
	}
	if m.Module.Path == "" {
		return m, fmt.Errorf("%s declares no module path", file)
	}
	return m, nil
}

// Owner returns the module a package import path belongs to: the one with
// the longest module path that is the import path or a prefix of it on a
// path boundary. That is how Go resolves it too, since a nested module's
// path extends the root's.
func Owner(mods []Module, importPath string) (Module, bool) {
	best, found := Module{}, false
	for _, m := range mods {
		if (importPath == m.Path || strings.HasPrefix(importPath, m.Path+"/")) &&
			(!found || len(m.Path) > len(best.Path)) {
			best, found = m, true
		}
	}
	return best, found
}

// DirOwner returns the module whose directory holds the repo-relative,
// slash-separated path p: the in-tree module with the longest directory that
// is p or one of p's parents. The root holds everything no nested module does.
func DirOwner(mods []Module, p string) (Module, bool) {
	// The root's "." is not a prefix of anything, so it ranks below every
	// nested directory rather than by its length.
	rank := func(m Module) int {
		if m.Dir == "." {
			return 0
		}
		return len(m.Dir) + 1
	}
	best, found := Module{}, false
	for _, m := range mods {
		if (m.Dir == "." || p == m.Dir || strings.HasPrefix(p, m.Dir+"/")) &&
			(!found || rank(m) > rank(best)) {
			best, found = m, true
		}
	}
	return best, found
}
