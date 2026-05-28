package claudeimport

// walkSkills enumerates .claude/skills/<name>/SKILL.md and records
// each as a SkillEntry with copy intent. Multi-file skills are
// flagged (loomcycle's Approach A bundling reads only SKILL.md;
// supplementary files are NOT auto-copied).
//
// This stub is filled in by the skills-mapper commit.
func walkSkills(dir, dest string, report *ImportReport) error {
	_ = dir
	_ = dest
	_ = report
	return nil
}
