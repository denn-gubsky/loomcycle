package claudeimport

import (
	"fmt"
	"os"
)

// WriteEmittedRecipes flushes the report's EmitRecipePath entries
// (planned by walkMCP under opts.EmitRecipes) to disk. Caller invokes
// this only under --write; the dry-run reports describe what WOULD
// be written via MCPEntry.EmitRecipePath + EmitRecipeJSON.
//
// Refuses to clobber an existing overlay file without `force`. Each
// successful write contributes a one-line status string to the
// returned slice so the CLI can render a per-file summary; failures
// return at the first error (the operator should fix and re-run).
func WriteEmittedRecipes(report *ImportReport, force bool) ([]string, error) {
	if report == nil {
		return nil, nil
	}
	var written []string
	for _, m := range report.MCPServers {
		if m.EmitRecipePath == "" || m.EmitRecipeJSON == "" {
			continue
		}
		if _, err := os.Stat(m.EmitRecipePath); err == nil && !force {
			return written, fmt.Errorf("emit-recipes: %s already exists (use --force to overwrite)",
				m.EmitRecipePath)
		} else if err != nil && !os.IsNotExist(err) {
			return written, fmt.Errorf("emit-recipes: stat %s: %w", m.EmitRecipePath, err)
		}
		if err := os.WriteFile(m.EmitRecipePath, []byte(m.EmitRecipeJSON), 0o644); err != nil {
			return written, fmt.Errorf("emit-recipes: write %s: %w", m.EmitRecipePath, err)
		}
		written = append(written, fmt.Sprintf("wrote %s", m.EmitRecipePath))
	}
	return written, nil
}

// WriteSkillCopies copies SKILL.md files from the report's SkillEntry
// SourcePath → DestinationPath. Mirrors WriteEmittedRecipes's
// refuse-on-collision semantics. Called by the CLI under --write.
//
// Supplementary files in multi-file skills are NOT copied per the
// RFC sharp edge — operators decide what to bring over manually.
func WriteSkillCopies(report *ImportReport, force bool) ([]string, error) {
	if report == nil {
		return nil, nil
	}
	var written []string
	for _, s := range report.Skills {
		if s.SourcePath == "" || s.DestinationPath == "" {
			continue
		}
		if _, err := os.Stat(s.DestinationPath); err == nil && !force {
			return written, fmt.Errorf("skill copy: %s already exists (use --force to overwrite)",
				s.DestinationPath)
		} else if err != nil && !os.IsNotExist(err) {
			return written, fmt.Errorf("skill copy: stat %s: %w", s.DestinationPath, err)
		}
		if err := os.MkdirAll(parentDir(s.DestinationPath), 0o755); err != nil {
			return written, fmt.Errorf("skill copy: mkdir for %s: %w", s.DestinationPath, err)
		}
		data, err := os.ReadFile(s.SourcePath)
		if err != nil {
			return written, fmt.Errorf("skill copy: read %s: %w", s.SourcePath, err)
		}
		if err := os.WriteFile(s.DestinationPath, data, 0o644); err != nil {
			return written, fmt.Errorf("skill copy: write %s: %w", s.DestinationPath, err)
		}
		written = append(written, fmt.Sprintf("copied %s → %s", s.SourcePath, s.DestinationPath))
	}
	return written, nil
}

// parentDir returns the directory portion of a filesystem path. Stdlib
// has filepath.Dir but we keep a tiny wrapper so the call sites read
// linearly without an import shuffle.
func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			if i == 0 {
				return p[:1]
			}
			return p[:i]
		}
	}
	return "."
}
