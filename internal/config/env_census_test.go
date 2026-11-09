package config

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envCensusExempt names LOOMCYCLE_* string literals in the Go sources that are
// NOT knobs an operator sets on the runtime, each with the reason. Everything
// else the census finds must be documented in docs/CONFIGURATION.md. Keep this
// list short: a name the runtime reads belongs in the docs, not here.
var envCensusExempt = map[string]string{
	// `loomcycle import claude-code` pre-allows these names in its advisory
	// ${VAR} rewrite check; they are suggestions for the operator's own
	// env_allowlist, never read by loomcycle itself.
	"LOOMCYCLE_ANTHROPIC_API_KEY":  "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_OPENAI_API_KEY":     "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_GEMINI_API_KEY":     "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_DEEPSEEK_API_KEY":   "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_GITHUB_TOKEN":       "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_GITLAB_TOKEN":       "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_SLACK_BOT_TOKEN":    "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_TELEGRAM_BOT_TOKEN": "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_DISCORD_BOT_TOKEN":  "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_NOTION_API_KEY":     "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_TAVILY_API_KEY":     "importer's advisory ${} allowlist, not read",
	"LOOMCYCLE_BRAVE_API_KEY":      "importer's advisory ${} allowlist, not read",
}

// envNameLiteral is a whole string literal that is a LOOMCYCLE_* name. A name
// built at run time ("LOOMCYCLE_" + x) is invisible to this census; none of
// the runtime's reads do that today, and a prefix literal ends in "_" and so
// does not match.
var envNameLiteral = regexp.MustCompile(`^LOOMCYCLE_[A-Z0-9_]*[A-Z0-9]$`)

// envNamesInSources tokenises every non-test .go file under the given roots
// and returns each LOOMCYCLE_* string literal with one file it appears in.
// Tokenising (rather than grepping) keeps names that appear only in comments
// out of the census.
func envNamesInSources(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			var s scanner.Scanner
			s.Init(fset.AddFile(path, -1, len(src)), src, nil, 0)
			for {
				_, tok, lit := s.Scan()
				if tok == token.EOF {
					break
				}
				if tok != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit)
				if err != nil || !envNameLiteral.MatchString(v) {
					continue
				}
				if _, ok := found[v]; !ok {
					found[v] = path
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return found
}

// TestEnvCensus_EveryReadNameIsDocumented holds docs/CONFIGURATION.md to the
// code: an environment variable the runtime reads that the configuration guide
// never names is a knob an operator cannot find.
func TestEnvCensus_EveryReadNameIsDocumented(t *testing.T) {
	repo := filepath.Join("..", "..")
	doc, err := os.ReadFile(filepath.Join(repo, "docs", "CONFIGURATION.md"))
	if err != nil {
		t.Fatalf("read CONFIGURATION.md: %v", err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`LOOMCYCLE_[A-Z0-9_]*[A-Z0-9]`).FindAllString(string(doc), -1) {
		documented[m] = true
	}

	found := envNamesInSources(t, filepath.Join(repo, "cmd"), filepath.Join(repo, "internal"))
	if len(found) < 100 {
		// The runtime reads a few hundred names; a handful means the walk
		// looked in the wrong place and the check below proves nothing.
		t.Fatalf("census found only %d LOOMCYCLE_* names; the source walk is broken", len(found))
	}

	var missing []string
	for name, file := range found {
		if documented[name] {
			continue
		}
		if _, ok := envCensusExempt[name]; ok {
			continue
		}
		missing = append(missing, name+" ("+file+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d LOOMCYCLE_* names read in code are not documented in docs/CONFIGURATION.md "+
			"(add a row to the environment variable reference, or an exemption with its reason):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	// An exemption for a name the code no longer contains is dead weight that
	// would quietly cover a future knob of the same name.
	for name := range envCensusExempt {
		if _, ok := found[name]; !ok {
			t.Errorf("envCensusExempt lists %s, which no longer appears in the sources; remove it", name)
		}
	}
}
