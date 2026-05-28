package claudeimport

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// walkSkills enumerates .claude/skills/<name>/SKILL.md and records
// each as a SkillEntry with copy intent. The walker does NOT copy
// files — the CLI's --write path applies the report's plan.
//
// Multi-file skills (skill directories with files beyond SKILL.md)
// are flagged. Loomcycle's Approach A bundling reads only SKILL.md;
// auto-copying supplementary files would create dead files in the
// loomcycle repo. The flag lets operators decide what to bring over.
//
// dest is the absolute path under which destination paths get
// computed. Empty dest is fine for dry-run reports — the
// DestinationPath field stays empty and the CLI fills it in if it
// knows the operator's skills root.
func walkSkills(dir, dest string, report *ImportReport) error {
	st, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !st.IsDir() {
		return nil
	}
	subdirs, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, sub := range subdirs {
		if !sub.IsDir() {
			continue
		}
		name := sub.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		skillRoot := filepath.Join(dir, name)
		skillFile := filepath.Join(skillRoot, "SKILL.md")
		if _, err := os.Stat(skillFile); err != nil {
			if os.IsNotExist(err) {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("skill %s: skipped (no SKILL.md found under %s)", name, skillRoot))
				continue
			}
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("skill %s: stat SKILL.md: %v", name, err))
			continue
		}

		entry := &SkillEntry{
			Name:       name,
			SourcePath: skillFile,
		}
		if dest != "" {
			entry.DestinationPath = filepath.Join(dest, name, "SKILL.md")
		}

		// Detect multi-file: anything in the skill dir besides
		// SKILL.md surfaces as supplementary.
		siblings, err := os.ReadDir(skillRoot)
		if err == nil {
			supps := []string{}
			for _, f := range siblings {
				if f.IsDir() {
					supps = append(supps, f.Name()+"/")
					continue
				}
				if f.Name() == "SKILL.md" {
					continue
				}
				supps = append(supps, f.Name())
			}
			sort.Strings(supps)
			if len(supps) > 0 {
				entry.MultiFile = true
				entry.SupplementaryAny = supps
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("skill %s: multi-file (SKILL.md + %d supplementary); "+
						"loomcycle's Approach A bundling reads only SKILL.md — "+
						"supplementary files not auto-copied", name, len(supps)))
			}
		}

		report.Skills = append(report.Skills, entry)
	}
	return nil
}
