package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestResolveSkillBodiesForRun_NoSkillsIsNoop verifies the helper's
// fast path: an agent with no `skills` list returns unchanged.
func TestResolveSkillBodiesForRun_NoSkillsIsNoop(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	srv := &Server{store: s}
	def := config.AgentDef{SystemPrompt: "base prompt", SystemPromptBase: "base prompt"}
	got, _ := srv.resolveSkillBodiesForRun(context.Background(), def)
	if got.SystemPrompt != "base prompt" {
		t.Errorf("got %q, want unchanged base prompt", got.SystemPrompt)
	}
}

// TestResolveSkillBodiesForRun_NoActiveRowsIsNoop verifies the
// fast path: an agent with skills but no DB-active rows returns
// the already-baked SystemPrompt unchanged.
func TestResolveSkillBodiesForRun_NoActiveRowsIsNoop(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	srv := &Server{store: s}
	const baked = "base prompt\n\n---\n\nSTATIC SKILL BODY"
	def := config.AgentDef{
		Skills:           []string{"karpathy-guidelines"},
		SystemPrompt:     baked,
		SystemPromptBase: "base prompt",
	}
	got, _ := srv.resolveSkillBodiesForRun(context.Background(), def)
	if got.SystemPrompt != baked {
		t.Errorf("no DB-active row should leave baked prompt unchanged; got %q", got.SystemPrompt)
	}
}

// TestResolveSkillBodiesForRun_DBActiveOverrides verifies the slow
// path: a DB-active SkillDef row rebuilds SystemPrompt from base +
// the DB body.
func TestResolveSkillBodiesForRun_DBActiveOverrides(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Seed a v1 SkillDef row + active pointer for "karpathy-guidelines".
	defJSON, _ := json.Marshal(map[string]string{"body": "DB BODY"})
	row, err := s.SkillDefCreate(ctx, store.SkillDefRow{
		DefID:      "sdf_test1",
		Name:       "karpathy-guidelines",
		Definition: defJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "", "karpathy-guidelines", row.DefID, ""); err != nil {
		t.Fatal(err)
	}

	srv := &Server{store: s}
	def := config.AgentDef{
		Skills: []string{"karpathy-guidelines"},
		// Baked SystemPrompt from config-load contains the STATIC body.
		// The helper should replace it with the DB body.
		SystemPrompt:     "base prompt\n\n---\n\nSTATIC BODY",
		SystemPromptBase: "base prompt",
	}
	got, _ := srv.resolveSkillBodiesForRun(ctx, def)
	if !strings.Contains(got.SystemPrompt, "DB BODY") {
		t.Errorf("DB body should be substituted into SystemPrompt; got %q", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "STATIC BODY") {
		t.Errorf("STATIC BODY should be replaced; got %q", got.SystemPrompt)
	}
	if !strings.HasPrefix(got.SystemPrompt, "base prompt") {
		t.Errorf("rebuild should start from base; got %q", got.SystemPrompt)
	}
}

// TestResolveSkillBodiesForRun_StaleRowIsIgnored verifies that a
// SkillDef row with an empty body is ignored (defends against
// hand-mucked DB rows).
func TestResolveSkillBodiesForRun_StaleRowIsIgnored(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Seed with an empty body — should be treated as "no DB-active body."
	defJSON, _ := json.Marshal(map[string]string{"body": ""})
	row, err := s.SkillDefCreate(ctx, store.SkillDefRow{
		DefID:      "sdf_empty",
		Name:       "empty-skill",
		Definition: defJSON,
		CreatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "", "empty-skill", row.DefID, ""); err != nil {
		t.Fatal(err)
	}

	srv := &Server{store: s}
	const baked = "base prompt\n\n---\n\nSTATIC BODY"
	def := config.AgentDef{
		Skills:           []string{"empty-skill"},
		SystemPrompt:     baked,
		SystemPromptBase: "base prompt",
	}
	got, _ := srv.resolveSkillBodiesForRun(ctx, def)
	if got.SystemPrompt != baked {
		t.Errorf("empty-body DB row should NOT trigger rebuild; got %q", got.SystemPrompt)
	}
}

// TestResolveSkillBodiesForRun_OneSkillResolvesEvenIfAnotherIsMissing
// pins the per-skill-error fallback. With skill-a active in DB and
// skill-b having no DB row, the rebuild includes the DB body for
// skill-a AND the static fallback chain (or no body) for skill-b —
// it does NOT abort the whole agent.
func TestResolveSkillBodiesForRun_OneSkillResolvesEvenIfAnotherIsMissing(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// skill-a has a DB-active row; skill-b does not.
	defJSON, _ := json.Marshal(map[string]string{"body": "DB BODY A"})
	row, err := s.SkillDefCreate(ctx, store.SkillDefRow{
		DefID:      "sdf_a",
		Name:       "skill-a",
		Definition: defJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SkillDefSetActive(ctx, "", "skill-a", row.DefID, ""); err != nil {
		t.Fatal(err)
	}

	srv := &Server{store: s}
	def := config.AgentDef{
		Skills:           []string{"skill-a", "skill-b"},
		SystemPrompt:     "base prompt\n\n---\n\nstatic A\n\n---\n\nstatic B",
		SystemPromptBase: "base prompt",
	}
	got, _ := srv.resolveSkillBodiesForRun(ctx, def)
	if !strings.Contains(got.SystemPrompt, "DB BODY A") {
		t.Errorf("DB body for skill-a should be substituted; got %q", got.SystemPrompt)
	}
}
