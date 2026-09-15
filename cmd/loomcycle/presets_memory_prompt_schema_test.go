package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestExtractor_TemplateCarriesEveryFieldTheProseAsksFor.
//
// The extractor's reply is copied from the JSON template at the end of its
// prompt — the prompt's own design note puts the required output shape there
// deliberately, as the last thing read before the transcript. A field the prose
// asks for but the template omits is therefore a field the model mostly does not
// emit, and nothing anywhere fails: the pass succeeds, the facts are written,
// and only the missing structure is gone.
//
// That happened, and it cost the graph its edges. `also_about` was named in the
// prose and absent from the template, so a fact naming two entities was stored
// with one `about` edge to its own subject and nothing joining it to the other.
// Measured on a corpus where every fact names two entities, one fact in four
// carried the second subject; with the field in the template, all of them did
// (17/72 → 72/72 over the same transcripts, two-sided exact p = 0.00098). A
// multi-hop question needs A→B and B→C, and the store held only spokes.
//
// So: every field the prose names in backticks must appear in the template.
func TestExtractor_TemplateCarriesEveryFieldTheProseAsksFor(t *testing.T) {
	cfg := memoryBundleConfig(t)
	prompt := cfg.Agents["memory/extractor"].SystemPrompt

	// The template lines are the ones showing the JSON array shape. There may be
	// more than one (the minimal form and the full form), and a field only has to
	// appear in one of them — the minimal form exists precisely to show the shape
	// WITHOUT the entity fields.
	var template strings.Builder
	for _, line := range strings.Split(prompt, "\n") {
		if strings.Contains(line, `{"text"`) {
			template.WriteString(line)
			template.WriteString("\n")
		}
	}
	if template.Len() == 0 {
		t.Fatal("the extractor prompt no longer shows a JSON template — the model has nothing to copy, " +
			"and the reply shape is the one thing this prompt cannot leave to inference")
	}

	// Backticked lowercase identifiers are how this prompt names an emittable
	// field (`type`, `subject`, `also_about`). Prose prefers plain words for
	// everything else, so the convention is a usable signal rather than a guess.
	fieldRe := regexp.MustCompile("`([a-z][a-z_]*)`")
	seen := map[string]bool{}
	var missing []string
	for _, m := range fieldRe.FindAllStringSubmatch(prompt, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(template.String(), `"`+name+`"`) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the prose asks for %v but the JSON template does not show %v.\n"+
			"The template is the last thing the model reads before the transcript and it copies it, "+
			"so a field only named in prose is a field it mostly will not emit — silently, because "+
			"the pass still succeeds and the facts are still written.\ntemplate:\n%s",
			keys(seen), missing, template.String())
	}

	// And the fields that carry the entity graph must be there by name: a future
	// edit that drops one would otherwise pass the check above by also dropping
	// the prose that asks for it, which is the same outage with no test to show it.
	for _, want := range []string{"subject", "also_about"} {
		if !strings.Contains(template.String(), `"`+want+`"`) {
			t.Errorf("the JSON template no longer shows %q — without it a fact naming two entities "+
				"is stored as a spoke instead of a link, and multi-hop recall has no path to walk", want)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
