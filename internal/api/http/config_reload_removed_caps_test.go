package http

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestReloadConfig_ARemovedConcurrencyKeyIsRejectedNamingItsReplacement — a hot
// reload loads the config the way boot does, so an edit that still sets one of
// the removed caps is refused whole, tells the operator where the cap lives
// now, and applies nothing: the running config stays as it was.
func TestReloadConfig_ARemovedConcurrencyKeyIsRejectedNamingItsReplacement(t *testing.T) {
	const base = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  ollama-local: { driver: ollama, base_url: "http://b:11434" }
models:
  decide: { provider: ollama-local, model: nimble, kind: decision }
`
	for _, c := range []struct{ name, body, want string }{
		{"decision", "decision:\n  default: decide\n  models: [decide]\n  max_concurrent: 2\n",
			"decision.max_concurrent was removed: cap decision calls per provider with providers.<id>.lanes.decision.max_concurrent"},
		{"reranker", "memory:\n  reranker:\n    provider: ollama-local\n    model: qwen3.6:latest\n    max_concurrent: 2\n",
			"memory.reranker.max_concurrent was removed: a listwise rerank is a chat call, capped by providers.<id>.lanes.chat.max_concurrent (or max_concurrent); a decision rerank by providers.<id>.lanes.decision.max_concurrent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Two layers, as a deployment with a CONFIG_DIR and a --config has.
			dir := t.TempDir()
			basePath, editPath := filepath.Join(dir, "base.yaml"), filepath.Join(dir, "edit.yaml")
			if err := os.WriteFile(basePath, []byte(base), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(editPath, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s := &Server{cfgHolder: config.NewHolder(baselineCfg())}
			applied := 0
			s.SetConfigReloader(
				func() (*config.Config, error) { return config.Load(basePath, editPath) },
				SectionReloader{Sections: []string{"providers"}, Reload: func(*config.Config) error { applied++; return nil }},
			)
			_, err := s.ReloadConfig(context.Background(), false)
			if !errors.Is(err, config.ErrReloadInvalid) {
				t.Fatalf("err = %v, want ErrReloadInvalid", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not name the replacement:\n got: %v\nwant: %s", err, c.want)
			}
			if applied != 0 {
				t.Errorf("apply called %d times for a refused reload, want 0", applied)
			}
			if got := s.cfg().Providers["ollama-local"].BaseURL; got != "http://a:11434" {
				t.Errorf("the running config changed to %q under a refused reload", got)
			}
		})
	}
}
