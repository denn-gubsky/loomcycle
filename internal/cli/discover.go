package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// discover.go — RFC CK section-per-file config split.
//
// A loomcycle.yaml base "brings" its loomcycle.<section>.yaml siblings: they
// deep-merge (RFC AN) after the base. This lives in internal/cli — the shared
// config-assembly home — so BOTH the server boot path (cmd/loomcycle/main.go)
// and loadLayeredConfig (validate / doctor / agents list) discover the same
// section files. Server-only discovery was the #602/v1.8.1 lockstep-drift bug
// class; keeping one helper is what prevents it here.

// SectionSiblings returns the loomcycle.<section>.yaml (/.yml) section files that
// sit beside basePath, sorted lexically. It excludes:
//   - the base itself;
//   - any *.example.yaml / *.example.yml — the repo ships loomcycle.example.yaml
//     and the cp-and-edit workflow leaves it beside a live config; a template
//     must never silently deep-merge OVER the operator's values (last wins);
//   - any non-regular file (a directory, broken symlink, or fifo matching the
//     glob), which would otherwise reach config.LoadLayers → os.ReadFile and
//     abort boot — mirroring configDirLayers' IsDir guard.
//
// filepath.Glob's only error is ErrBadPattern, impossible for these constant
// patterns, so it is discarded deliberately.
func SectionSiblings(basePath string) []string {
	dir := filepath.Dir(basePath)
	var files []string
	for _, pat := range []string{"loomcycle.*.yaml", "loomcycle.*.yml"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, m := range matches {
			if m == basePath ||
				strings.HasSuffix(m, ".example.yaml") || strings.HasSuffix(m, ".example.yml") {
				continue
			}
			// os.Stat follows symlinks: a symlink to a regular file loads; a
			// directory, broken symlink, or fifo is skipped.
			if fi, err := os.Stat(m); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			files = append(files, m)
		}
	}
	// A file matches at most one of the two suffix patterns, so no dedup is needed
	// within this call; ordering across the two globs is restored by the sort.
	sort.Strings(files)
	return files
}

// WithSectionSiblings expands a list of operator config files so each
// loomcycle.yaml is immediately followed by its SectionSiblings (deep-merged
// after it). Non-base files pass through unchanged. The result preserves order
// and is deduped, so a base listed twice — or two bases sharing a directory —
// never double-layers a sibling. Applied wherever loomcycle.yaml files are
// layered explicitly (LOOMCYCLE_CONFIG_FILES / --config) or by auto-discovery;
// NOT to LOOMCYCLE_CONFIG_DIR, which already globs every *.yaml in the dir.
func WithSectionSiblings(files []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range files {
		add(f)
		if filepath.Base(f) == "loomcycle.yaml" {
			for _, s := range SectionSiblings(f) {
				add(s)
			}
		}
	}
	return out
}
