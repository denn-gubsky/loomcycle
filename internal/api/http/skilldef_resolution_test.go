package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC BA: resolveSkillBodiesForRun no longer bakes SkillDef bodies into the
// prompt (skills are on-demand via the Skill tool). It now only injects the
// ephemeral (per-run, never persisted) "skills available" NOTE, and ONLY when
// `skills:` is a WHITELIST (names positive patterns). For no-skills, an
// empty/absent allowlist, or a blacklist-only allowlist it is a no-op — the
// Skill tool is in the agent's tool list and the model discovers via
// Skill(op=list). These tests replace the old DB-active-body resolution suite.

// No `skills:` list at all → no note, prompt unchanged.
func TestResolveSkillBodiesForRun_NoSkillsIsNoop(t *testing.T) {
	srv := &Server{}
	def := config.AgentDef{SystemPrompt: "base prompt"}
	got, prov := srv.resolveSkillBodiesForRun(context.Background(), "", def)
	if got.SystemPrompt != "base prompt" {
		t.Errorf("got %q, want unchanged base prompt", got.SystemPrompt)
	}
	if len(prov.SkillDefIDs) != 0 {
		t.Errorf("provenance should be empty (no substrate baking); got %v", prov.SkillDefIDs)
	}
}

// A blacklist-only allowlist (`skills: [-secret/*]`) has no positive pattern →
// no note. The agent may use every non-secret skill on demand via the tool.
func TestResolveSkillBodiesForRun_BlacklistIsNoop(t *testing.T) {
	srv := &Server{}
	def := config.AgentDef{Skills: []string{"-secret/*"}, SystemPrompt: "base prompt"}
	got, _ := srv.resolveSkillBodiesForRun(context.Background(), "", def)
	if got.SystemPrompt != "base prompt" {
		t.Errorf("a blacklist should not inject a note; got %q", got.SystemPrompt)
	}
}

// A whitelist (`skills: [doc/*]`) appends a per-run note naming the permitted
// patterns + how to use the Skill tool. The note lands AFTER the agent's own
// prompt, separated by a rule.
func TestResolveSkillBodiesForRun_WhitelistAppendsNote(t *testing.T) {
	srv := &Server{}
	def := config.AgentDef{Skills: []string{"doc/*", "-doc/secret"}, SystemPrompt: "base prompt"}
	got, _ := srv.resolveSkillBodiesForRun(context.Background(), "", def)
	if !strings.HasPrefix(got.SystemPrompt, "base prompt") {
		t.Errorf("note must append after the agent's own prompt; got %q", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "doc/*") {
		t.Errorf("note should name the permitted patterns; got %q", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "Skill") {
		t.Errorf("note should point at the Skill tool; got %q", got.SystemPrompt)
	}
	// The negative pattern is not a permitted target, so it must not be listed.
	if strings.Contains(got.SystemPrompt, "-doc/secret") {
		t.Errorf("note should list only positive patterns; got %q", got.SystemPrompt)
	}
}

// A whitelist agent with no base prompt still gets the note (no leading rule).
func TestResolveSkillBodiesForRun_WhitelistNoteWithEmptyPrompt(t *testing.T) {
	srv := &Server{}
	def := config.AgentDef{Skills: []string{"marketing/**"}}
	got, _ := srv.resolveSkillBodiesForRun(context.Background(), "", def)
	if !strings.Contains(got.SystemPrompt, "marketing/**") {
		t.Errorf("note should name the whitelist pattern even with no base prompt; got %q", got.SystemPrompt)
	}
	if strings.HasPrefix(got.SystemPrompt, "\n") || strings.HasPrefix(got.SystemPrompt, "---") {
		t.Errorf("note with no base prompt should not carry a leading separator; got %q", got.SystemPrompt)
	}
}
