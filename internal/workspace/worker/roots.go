package worker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// Roots on a personal worker (RFC GW §4.5, P13). A user starts the worker on
// their own machine with --personal and names directories as roots; a
// workspace opened with a root IS that directory, never a copy. The worker
// keeps which workspace id is bound to which root in its state folder, so the
// ops on that id reach the directory, and a remove only unbinds it: nothing
// the core asks ever deletes a root.

// Root is one directory a personal worker offers, by the name the model uses.
type Root struct {
	Name string
	// Path is absolute; after checkRoots it has no link in it.
	Path string
}

// rootsDir, in the worker's state folder, holds one file per bound
// workspace id, naming its root. A dot keeps held() from taking it for a
// workspace.
const rootsDir = ".loomcycle-roots"

// maxRootName bounds a root's name, as a workspace name is bounded.
const maxRootName = 64

// validRootName is a workspace-name-like label: 1 to 64 letters, digits,
// '.', '-' and '_', starting with a letter or a digit. It is printed to the
// model and sent as a label, and never reaches a path.
func validRootName(n string) bool {
	if n == "" || len(n) > maxRootName {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case (c == '.' || c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// checkRoots checks the roots a personal worker is started with (D-P13-7)
// and returns them with their real paths: each name valid and used once,
// each path absolute, an existing folder, never the worker's state folder,
// inside it or holding it, and no root inside another. A bad declaration
// stops the start: a root the user did not mean would be a directory handed
// to an agent.
func checkRoots(state string, roots []Root) ([]Root, error) {
	stateReal, err := filepath.EvalSymlinks(state)
	if err != nil {
		return nil, fmt.Errorf("the worker's state folder: %w", err)
	}
	out := make([]Root, 0, len(roots))
	names := map[string]bool{}
	for _, r := range roots {
		switch {
		case !validRootName(r.Name):
			return nil, fmt.Errorf("root %q: a name is 1 to %d letters, digits, '.', '-' or '_', starting with a letter or digit", r.Name, maxRootName)
		case names[r.Name]:
			return nil, fmt.Errorf("root %q is named twice", r.Name)
		case !filepath.IsAbs(r.Path):
			return nil, fmt.Errorf("root %q: the path must be absolute", r.Name)
		}
		names[r.Name] = true
		real, err := filepath.EvalSymlinks(r.Path)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", r.Name, err)
		}
		st, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", r.Name, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("root %q is not a folder", r.Name)
		}
		if pathWithin(real, stateReal) || pathWithin(stateReal, real) {
			return nil, fmt.Errorf("root %q is, holds or lies inside the worker's own state folder (LOOMCYCLE_WORKSPACE_ROOT)", r.Name)
		}
		for _, o := range out {
			if pathWithin(real, o.Path) || pathWithin(o.Path, real) {
				return nil, fmt.Errorf("roots %q and %q nest: one lies inside the other", o.Name, r.Name)
			}
		}
		out = append(out, Root{Name: r.Name, Path: real})
	}
	return out, nil
}

// pathWithin reports whether path is dir or lies under it.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// rootLabels are the labels naming the roots, by name only.
func (w *worker) rootLabels() []string {
	out := make([]string, 0, len(w.opt.Roots))
	for _, r := range w.opt.Roots {
		out = append(out, workspace.RootLabelPrefix+r.Name)
	}
	return out
}

func (w *worker) rootPath(name string) (string, bool) {
	for _, r := range w.opt.Roots {
		if r.Name == name {
			return r.Path, true
		}
	}
	return "", false
}

func (w *worker) bindingsDir() string { return filepath.Join(w.opt.Root, rootsDir) }

// loadBindings reads the bindings a previous run of the worker left. One
// whose root is no longer declared is dropped: its id is then absent, and
// the core's reconciliation treats it as gone.
func (w *worker) loadBindings() error {
	w.rootMu.Lock()
	defer w.rootMu.Unlock()
	w.bound = map[string]string{}
	ents, err := os.ReadDir(w.bindingsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		id := e.Name()
		if e.IsDir() || !workspace.ValidID(id) {
			_ = os.RemoveAll(filepath.Join(w.bindingsDir(), id))
			continue
		}
		// #nosec G304 -- id is a ValidID (no separator, no dot), so the path stays in the bindings folder
		b, err := os.ReadFile(filepath.Join(w.bindingsDir(), id))
		name := strings.TrimSpace(string(b))
		if _, ok := w.rootPath(name); err != nil || !ok {
			_ = os.Remove(filepath.Join(w.bindingsDir(), id))
			continue
		}
		w.bound[id] = name
	}
	return nil
}

// boundRoot is the root workspace id is bound to.
func (w *worker) boundRoot(id string) (string, bool) {
	w.rootMu.Lock()
	defer w.rootMu.Unlock()
	name, ok := w.bound[id]
	return name, ok
}

// boundIDs are the ids bound to a root, for held().
func (w *worker) boundIDs() []string {
	w.rootMu.Lock()
	defer w.rootMu.Unlock()
	out := make([]string, 0, len(w.bound))
	for id := range w.bound {
		out = append(out, id)
	}
	return out
}

// wsDir is the directory of workspace id: its root's path when it is bound
// to one, else <state>/<id>. id is a ValidID.
func (w *worker) wsDir(id string) string {
	if name, ok := w.boundRoot(id); ok {
		if p, ok := w.rootPath(name); ok {
			return p
		}
	}
	return filepath.Join(w.opt.Root, id)
}

// bindRoot binds workspace id to root name: one live binding per root
// (D-P13-6); a repeated bind of the same id is harmless. The binding is
// written by a rename, so a worker killed part way leaves none or all of it.
func (w *worker) bindRoot(id, name string) answer {
	if !w.opt.Personal || len(w.opt.Roots) == 0 {
		return refuse(workspace.ClassInvalid, "this worker offers no roots")
	}
	if _, ok := w.rootPath(name); !ok {
		return refuse(workspace.ClassInvalid, "this worker offers no root named %q", name)
	}
	w.rootMu.Lock()
	defer w.rootMu.Unlock()
	if cur, ok := w.bound[id]; ok {
		if cur == name {
			return answer{}
		}
		return refuse(workspace.ClassExists, "the workspace is bound to another root")
	}
	for other, n := range w.bound {
		if n == name && other != id {
			return refuse(workspace.ClassRootBusy, "another workspace is open on root %q; release it first", name)
		}
	}
	if err := os.MkdirAll(w.bindingsDir(), 0o700); err != nil {
		return refuse(workspace.ClassFailed, "recording the binding: %v", err)
	}
	f := filepath.Join(w.bindingsDir(), id)
	if err := os.WriteFile(f+".tmp", []byte(name), 0o600); err != nil {
		return refuse(workspace.ClassFailed, "recording the binding: %v", err)
	}
	if err := os.Rename(f+".tmp", f); err != nil {
		_ = os.Remove(f + ".tmp")
		return refuse(workspace.ClassFailed, "recording the binding: %v", err)
	}
	w.bound[id] = name
	return answer{}
}

// unbind forgets id's binding. The directory is never touched.
func (w *worker) unbind(id string) error {
	w.rootMu.Lock()
	defer w.rootMu.Unlock()
	if _, ok := w.bound[id]; !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(w.bindingsDir(), id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(w.bound, id)
	return nil
}

// onRoot refuses an op the root of a bound workspace does not take.
func (w *worker) onRoot(id, op string) *answer {
	if name, ok := w.boundRoot(id); ok {
		return refusal(workspace.ClassNotOnRoot, "%s is refused on root %q: the directory is the user's own", op, name)
	}
	return nil
}
