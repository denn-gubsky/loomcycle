package recipes

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// bundledFS embeds the curated recipes shipped with the binary. The
// `docs/mcp-recipes/` path is relative to the package root (Go's
// `embed` directive walks up to find the named directory).
//
//go:embed all:bundled
var bundledFS embed.FS

// disabledFilename is the operator's per-name suppression list. One
// recipe name per line under `$LOOMCYCLE_MCP_RECIPES_ROOT/.disabled`.
// Lines starting with `#` and blank lines are ignored. Suppression
// applies to BOTH bundled and overlay recipes — the CLI's `disable
// <name>` writes here regardless of source.
const disabledFilename = ".disabled"

// Library is a name→Recipe registry with bundled + overlay merging.
// The bundled set is read-only (embed.FS); the overlay is filesystem-
// resident at `$LOOMCYCLE_MCP_RECIPES_ROOT` if set. CLI verbs that
// mutate (`add` / `remove` / `enable` / `disable`) touch only the
// overlay — bundled entries are never modified at runtime.
//
// Library is safe for concurrent use. The mutex guards both the
// recipes / disabled maps AND the filesystem writes (the .disabled
// file + overlay JSON files), so concurrent callers can't race two
// `disable` invocations into a corrupt .disabled file. CLI usage is
// single-shot per process so this is defensive; programmatic
// consumers (e.g. a future Library-backed admin HTTP endpoint) get
// safety for free.
type Library struct {
	mu sync.RWMutex

	// overlayRoot is the resolved overlay directory or "" when no
	// overlay is configured. Held so mutation verbs can write back
	// without re-parsing the env var.
	overlayRoot string

	// recipes maps name → Recipe with overlay-wins semantics. Empty
	// when no recipes parsed at all (zero-value Library still works
	// — `Get` returns (nil,false), `Names()` returns nil).
	recipes map[string]*Recipe

	// disabled is the set of names suppressed via `.disabled`. Names
	// still appear in `recipes` (so `--include-disabled` can surface
	// them) but `Enabled()` filters them out.
	disabled map[string]bool
}

// Get returns the named recipe + whether it's enabled. Safe on nil
// receiver. The returned Recipe is the overlay version if one exists,
// otherwise the bundled version.
func (l *Library) Get(name string) (rec *Recipe, enabled bool, ok bool) {
	if l == nil {
		return nil, false, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.recipes[name]
	if !ok {
		return nil, false, false
	}
	return r, !l.disabled[name], true
}

// Enabled returns the names of all enabled recipes, sorted.
func (l *Library) Enabled() []string {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.recipes))
	for n := range l.recipes {
		if !l.disabled[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// All returns the names of all known recipes (enabled + disabled),
// sorted. Used by `mcp-registry list --include-disabled`.
func (l *Library) All() []string {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.recipes))
	for n := range l.recipes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsDisabled reports whether the named recipe is in the disabled set.
// Safe on nil receiver + unknown names (returns false).
func (l *Library) IsDisabled(name string) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.disabled[name]
}

// OverlayRoot returns the resolved overlay directory, empty when no
// overlay is configured. Used by mutation CLI verbs to refuse without
// a clear error message.
func (l *Library) OverlayRoot() string {
	if l == nil {
		return ""
	}
	return l.overlayRoot
}

// LoadLibrary builds the recipe registry. Bundled recipes load first;
// overlay recipes (when root != "") override matching names.
//
//   - Bundled parse errors are fatal — operator can't fix them without
//     a rebuild; failing loudly catches build-time mistakes (e.g. an
//     edited bundled JSON shipped malformed).
//   - Overlay dir-resolution errors (missing root, root is a file)
//     are fatal — these signal clear operator misconfiguration.
//   - Per-file overlay errors (parse, read, symlink) are SKIPPED with
//     a log line; one bad operator-supplied recipe must not kill the
//     CLI. Bundled defaults remain intact for the unaffected names.
//
// Symlinks under the overlay root are refused — operator-supplied
// directories are a trust boundary; same posture as Context.help.
func LoadLibrary(overlayRoot string) (*Library, error) {
	lib := &Library{
		overlayRoot: overlayRoot,
		recipes:     map[string]*Recipe{},
		disabled:    map[string]bool{},
	}

	// Load bundled recipes first.
	entries, err := bundledFS.ReadDir("bundled")
	if err != nil {
		return nil, fmt.Errorf("read embedded recipes/bundled: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := bundledFS.ReadFile("bundled/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read bundled recipe %s: %w", e.Name(), err)
		}
		nameFromFile := strings.TrimSuffix(e.Name(), ".json")
		rec, err := parseRecipe(data, nameFromFile, "")
		if err != nil {
			return nil, fmt.Errorf("parse bundled recipe %s: %w", e.Name(), err)
		}
		rec.Source = "bundled"
		lib.recipes[rec.Name] = rec
	}

	// Overlay loading is opt-in.
	if overlayRoot == "" {
		return lib, nil
	}
	st, err := os.Stat(overlayRoot)
	if err != nil {
		return nil, fmt.Errorf("mcp-recipes overlay root %s: %w", overlayRoot, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("mcp-recipes overlay root %s: not a directory", overlayRoot)
	}
	fsEntries, err := os.ReadDir(overlayRoot)
	if err != nil {
		return nil, fmt.Errorf("read mcp-recipes overlay root %s: %w", overlayRoot, err)
	}
	for _, e := range fsEntries {
		if e.IsDir() {
			continue
		}
		// `.disabled` is a special-cased text file — read separately.
		if e.Name() == disabledFilename {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(overlayRoot, e.Name())
		// Refuse to follow symlinks (same posture as Context.help).
		fi, err := os.Lstat(path)
		if err != nil {
			log.Printf("mcp-recipes: skipping %s: lstat: %v", path, err)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			log.Printf("mcp-recipes: skipping %s: symlink (overlay recipes must be regular files)", path)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("mcp-recipes: skipping %s: %v", path, err)
			continue
		}
		nameFromFile := strings.TrimSuffix(e.Name(), ".json")
		rec, err := parseRecipe(data, nameFromFile, path)
		if err != nil {
			// Soft-skip: one malformed overlay recipe must not kill
			// the CLI. Bundled defaults loaded above remain intact.
			log.Printf("mcp-recipes: skipping %s: %v", path, err)
			continue
		}
		rec.Source = "overlay"
		lib.recipes[rec.Name] = rec
	}

	// Load the .disabled file (after recipe parsing so we can warn
	// about names that don't exist in the library).
	disabledPath := filepath.Join(overlayRoot, disabledFilename)
	if data, err := os.ReadFile(disabledPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			lib.disabled[line] = true
			if _, known := lib.recipes[line]; !known {
				log.Printf("mcp-recipes: .disabled lists %q which doesn't exist in the library — ignoring", line)
			}
		}
	} else if !os.IsNotExist(err) {
		log.Printf("mcp-recipes: cannot read %s: %v (no recipes disabled)", disabledPath, err)
	}

	return lib, nil
}

// parseRecipe decodes one JSON body. nameFromFile is the filename
// stem; the parsed Recipe gets that name (recipes have no "name"
// field inside the JSON — filename IS the canonical identifier,
// matching Claude Code's `.claude/mcp.json` shape where the top-level
// key is the server name).
func parseRecipe(data []byte, nameFromFile, path string) (*Recipe, error) {
	rec := &Recipe{Name: nameFromFile, Path: path}
	if err := json.Unmarshal(data, rec); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return rec, nil
}

// AddOverlay copies the given recipe JSON into the overlay root as
// `<name>.json`. Refuses on overlay-root-unset, on collision unless
// force=true, and on malformed input JSON. The recipe is parsed +
// validated before write so we don't land broken JSON in the overlay.
//
// On success the Library's in-memory map is updated so subsequent
// `Get` / `All` calls reflect the addition without a re-read. The
// returned *Recipe carries Source="overlay" + Path=overlay/<name>.json.
func (l *Library) AddOverlay(name string, jsonBody []byte, force bool) (*Recipe, error) {
	if l == nil {
		return nil, fmt.Errorf("library not loaded")
	}
	if l.overlayRoot == "" {
		return nil, fmt.Errorf("LOOMCYCLE_MCP_RECIPES_ROOT not set: cannot add recipes (overlay root required)")
	}
	if !validRecipeName(name) {
		return nil, fmt.Errorf("invalid recipe name %q: must match [A-Za-z0-9_-]+", name)
	}
	rec, err := parseRecipe(jsonBody, name, "")
	if err != nil {
		return nil, fmt.Errorf("input JSON: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	dst := filepath.Join(l.overlayRoot, name+".json")
	if !force {
		if _, err := os.Stat(dst); err == nil {
			return nil, fmt.Errorf("overlay file %s already exists (use --force to clobber)", dst)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", dst, err)
		}
	}
	if err := os.WriteFile(dst, jsonBody, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", dst, err)
	}
	rec.Source = "overlay"
	rec.Path = dst
	l.recipes[name] = rec
	return rec, nil
}

// RemoveOverlay deletes `<name>.json` from the overlay root. Refuses
// when the name only exists in bundled (operator must `disable`
// instead — `remove` is for the OVERLAY surface only).
func (l *Library) RemoveOverlay(name string) error {
	if l == nil {
		return fmt.Errorf("library not loaded")
	}
	if l.overlayRoot == "" {
		return fmt.Errorf("LOOMCYCLE_MCP_RECIPES_ROOT not set: cannot remove (no overlay configured)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.recipes[name]
	if !ok {
		return fmt.Errorf("recipe %q not found", name)
	}
	if rec.Source != "overlay" {
		return fmt.Errorf("recipe %q is bundled, not overlay — use `mcp-registry disable %s` to suppress it instead", name, name)
	}
	if rec.Path == "" {
		return fmt.Errorf("internal: overlay recipe %q has empty path", name)
	}
	if err := os.Remove(rec.Path); err != nil {
		return fmt.Errorf("remove %s: %w", rec.Path, err)
	}
	delete(l.recipes, name)
	// Re-check bundled — if the overlay shadowed a bundled entry,
	// `remove` reveals the bundled fallback. Load it back into the map.
	if data, err := bundledFS.ReadFile("bundled/" + name + ".json"); err == nil {
		if bundled, err := parseRecipe(data, name, ""); err == nil {
			bundled.Source = "bundled"
			l.recipes[name] = bundled
		}
	}
	return nil
}

// Disable adds the name to the `.disabled` file, creating it if
// absent. Idempotent — calling Disable on an already-disabled name is
// a no-op + returns nil. Refuses on overlay-root-unset (the disabled
// list lives in the overlay too).
func (l *Library) Disable(name string) error {
	if l == nil {
		return fmt.Errorf("library not loaded")
	}
	if l.overlayRoot == "" {
		return fmt.Errorf("LOOMCYCLE_MCP_RECIPES_ROOT not set: cannot disable (overlay root required)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.recipes[name]; !ok {
		return fmt.Errorf("recipe %q not found in library", name)
	}
	if l.disabled[name] {
		return nil
	}
	l.disabled[name] = true
	return writeDisabledFile(l.overlayRoot, l.disabled)
}

// Enable removes the name from the `.disabled` file. Idempotent.
// Refuses on overlay-root-unset.
func (l *Library) Enable(name string) error {
	if l == nil {
		return fmt.Errorf("library not loaded")
	}
	if l.overlayRoot == "" {
		return fmt.Errorf("LOOMCYCLE_MCP_RECIPES_ROOT not set: cannot enable (overlay root required)")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.disabled[name] {
		return nil
	}
	delete(l.disabled, name)
	return writeDisabledFile(l.overlayRoot, l.disabled)
}

// writeDisabledFile serialises the disabled set to the overlay root.
// One name per line, sorted for stability; preserves a leading help
// comment so operators reading the file by hand know what it's for.
func writeDisabledFile(overlayRoot string, disabled map[string]bool) error {
	names := make([]string, 0, len(disabled))
	for n := range disabled {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# loomcycle mcp-registry disable list — one recipe name per line.\n")
	b.WriteString("# Edit via `loomcycle mcp-registry enable <name>` / `disable <name>`.\n")
	for _, n := range names {
		b.WriteString(n)
		b.WriteString("\n")
	}
	path := filepath.Join(overlayRoot, disabledFilename)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// validRecipeName enforces the operator-input contract on names
// supplied to `add` (the filename is the canonical identifier).
// Mirrors Claude Code's MCP-name restriction shape.
func validRecipeName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
