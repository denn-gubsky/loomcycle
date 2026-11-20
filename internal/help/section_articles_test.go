package help

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Abacus is a fixture tool with no `op` argument, so its help is split into
// section articles. No bundled tool has that name, so the fixture can never
// collide with the corpus.
const (
	abacusTool = "---\nname: Abacus\ndescription: Evaluates an expression.\n---\nOne call evaluates one expression. Read a section for its functions.\n"

	abacusFinance = "---\nname: Abacus/finance\ndescription: Finance functions.\nkind: section\n---\n" +
		"Loan payments.\n\n## Examples\n\n```json\n{\"expr\": \"pmt(0.05/12, 360, 200000)\"}\n```\n"

	abacusDates = "---\nname: Abacus/dates\ndescription: Date functions.\nkind: section\n---\n" +
		"Day counts.\n\n## Examples\n\n```json\n{\"expr\": \"days(\\\"2026-01-01\\\", \\\"2026-12-31\\\")\"}\n```\n"
)

// loadFixture loads tool articles the way the bundled corpus is loaded: from an
// fs.FS, with every bad file fatal.
func loadFixture(t *testing.T, files map[string]string) (*Set, error) {
	t.Helper()
	m := fstest.MapFS{}
	for name, body := range files {
		m["tools/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	set := &Set{topics: map[string]*Topic{}}
	err := loadToolArticles(bundledDirFS{m, toolsDir}, "", func(tp *Topic) {
		set.topics[tp.Name] = tp
	}, func(path string, err error) error {
		return err
	})
	set.reindex()
	return set, err
}

func abacusFixture() map[string]string {
	return map[string]string{
		"Abacus.md":         abacusTool,
		"Abacus/finance.md": abacusFinance,
		"Abacus/dates.md":   abacusDates,
	}
}

// A file under tools/<Tool>/ that says `kind: section` loads as a section of
// that tool: named <Tool>/<stem>, no op, kept out of the index, listed by its
// tool, and reachable by every spelling an operation article is.
func TestLoadToolArticles_SectionArticleLoadsAsSectionOfItsTool(t *testing.T) {
	set, err := loadFixture(t, abacusFixture())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fin, ok := set.Get("Abacus/finance")
	if !ok {
		t.Fatal("section article Abacus/finance not loaded")
	}
	if fin.Tool != "Abacus" || fin.Section != "finance" || fin.Op != "" {
		t.Errorf("Abacus/finance: Tool=%q Section=%q Op=%q, want Abacus/finance/\"\"", fin.Tool, fin.Section, fin.Op)
	}
	if !fin.IsSectionArticle() || fin.IsOpArticle() {
		t.Errorf("Abacus/finance: IsSectionArticle=%v IsOpArticle=%v, want true/false", fin.IsSectionArticle(), fin.IsOpArticle())
	}

	var secs []string
	for _, s := range set.SectionsOf("Abacus") {
		secs = append(secs, s.Section)
	}
	if got, want := strings.Join(secs, ","), "dates,finance"; got != want {
		t.Errorf("SectionsOf(Abacus) = %s, want %s", got, want)
	}
	if ops := set.OpsOf("Abacus"); len(ops) != 0 {
		t.Errorf("OpsOf(Abacus) = %d articles, want none: a section is not an operation", len(ops))
	}

	if art, ok := set.ToolArticle("Abacus"); !ok || art.IsSectionArticle() {
		t.Errorf("ToolArticle(Abacus) = %v, %v; want the tool article", art, ok)
	}
	if _, ok := set.ToolArticle("Abacus/finance"); ok {
		t.Error("ToolArticle returned a section article")
	}
	if got := strings.Join(set.IndexNames(), ","); got != "Abacus" {
		t.Errorf("IndexNames = %s, want only the tool article", got)
	}

	for _, q := range []string{"Abacus/finance", "abacus/finance", "Abacus.finance", "Abacus finance", "  ABACUS.FINANCE "} {
		if got, ok := set.Get(q); !ok || got.Name != "Abacus/finance" {
			t.Errorf("Get(%q) = %v, %v; want Abacus/finance", q, got, ok)
		}
	}

	// The tool article is an index of its sections, so it needs no example of
	// its own; each section does, and has one.
	for _, tp := range set.All() {
		if probs := set.Lint(tp); len(probs) != 0 {
			t.Errorf("%s: Lint = %q, want clean", tp.Name, probs)
		}
	}
}

// A file under tools/<Tool>/ without `kind:` is still an operation article.
func TestLoadToolArticles_FileWithoutKindStaysAnOperation(t *testing.T) {
	set, err := loadFixture(t, map[string]string{
		"Abacus.md":     abacusTool,
		"Abacus/get.md": "---\nname: Abacus/get\ndescription: x\n---\n## Examples\n\n```json\n{\"op\": \"get\"}\n```\n",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	get, _ := set.Get("Abacus/get")
	if get == nil || get.Op != "get" || get.Section != "" {
		t.Fatalf("Abacus/get = %+v, want an operation article", get)
	}
	if len(set.SectionsOf("Abacus")) != 0 {
		t.Error("an operation article was listed as a section")
	}
}

// Each section rule can fail: a section with no example, and a section whose
// examples only call another tool.
func TestLint_SectionArticleNeedsAnExampleCallingItsTool(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"no example",
			"---\nname: Abacus/finance\ndescription: x\nkind: section\n---\nLoan payments.\n",
			"no call example"},
		{"example calls another tool",
			"---\nname: Abacus/finance\ndescription: x\nkind: section\n---\n## Examples\n\n```json tool=Memory\n{\"op\": \"get\", \"key\": \"k\"}\n```\n",
			"no example calls Abacus"},
	}
	for _, c := range cases {
		set, err := loadFixture(t, map[string]string{"Abacus.md": abacusTool, "Abacus/finance.md": c.body})
		if err != nil {
			t.Fatalf("%s: load: %v", c.name, err)
		}
		fin, _ := set.Get("Abacus/finance")
		if probs := set.Lint(fin); !anyContains(probs, c.want) {
			t.Errorf("%s: Lint = %q, want a problem containing %q", c.name, probs, c.want)
		}
	}
}

// An unknown `kind:` is refused at parse, and `kind:` is refused anywhere but
// under tools/<Tool>/, on the bundled path where a bad file is fatal.
func TestLoadToolArticles_RefusesUnknownOrMisplacedKind(t *testing.T) {
	if _, err := parseTopic([]byte("---\nname: x\ndescription: x\nkind: chapter\n---\nbody\n"), "x", ""); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("parseTopic with kind: chapter = %v, want an unknown-kind error", err)
	}
	files := abacusFixture()
	files["Abacus/finance.md"] = strings.Replace(abacusFinance, "kind: section", "kind: sections", 1)
	if _, err := loadFixture(t, files); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("load with kind: sections = %v, want an unknown-kind error", err)
	}
	files = abacusFixture()
	files["Abacus.md"] = strings.Replace(abacusTool, "description:", "kind: section\ndescription:", 1)
	if _, err := loadFixture(t, files); err == nil || !strings.Contains(err.Error(), "only to an article under tools/") {
		t.Errorf("load with kind on the tool article = %v, want a misplaced-kind error", err)
	}
}

// A section belongs only on a tool without operations; SectionsOnOpTool is the
// check the builtin tests run against each tool's live schema.
func TestSectionsOnOpTool_RefusesSectionsOfAToolWithOperations(t *testing.T) {
	set, err := loadFixture(t, abacusFixture())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	probs := set.SectionsOnOpTool("Abacus", []string{"get", "set"})
	if len(probs) != 2 || !anyContains(probs, "Abacus/finance is a section article") {
		t.Errorf("SectionsOnOpTool(with ops) = %q, want one problem per section", probs)
	}
	if probs := set.SectionsOnOpTool("Abacus", nil); probs != nil {
		t.Errorf("SectionsOnOpTool(no ops) = %q, want none", probs)
	}
}

// The operator overlay (LOOMCYCLE_HELP_ROOT) loads section articles the same
// way, skips one with an unknown kind, and logs the lint problem of one whose
// examples do not call its tool.
func TestLoadSet_OperatorSectionArticles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tools", "Abacus"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "tools", rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Abacus.md", abacusTool)
	write("Abacus/finance.md", abacusFinance)
	write("Abacus/dates.md", strings.Replace(abacusDates, "```json\n", "```json tool=Memory\n", 1))
	write("Abacus/trig.md", "---\nname: Abacus/trig\ndescription: x\nkind: chapter\n---\nbody\n")

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	set, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	fin, ok := set.Get("Abacus.finance")
	if !ok || !fin.IsSectionArticle() || fin.Source != "filesystem" {
		t.Fatalf("operator section article = %+v, %v; want a filesystem section", fin, ok)
	}
	if _, ok := set.Get("Abacus/trig"); ok {
		t.Error("article with an unknown kind was loaded")
	}
	if len(set.SectionsOf("Abacus")) != 2 {
		t.Errorf("SectionsOf(Abacus) = %d, want 2", len(set.SectionsOf("Abacus")))
	}
	if contains(set.IndexNames(), "Abacus/finance") {
		t.Error("index lists a section article")
	}
	out := logs.String()
	if !strings.Contains(out, "no example calls Abacus") {
		t.Errorf("no lint warning for the section calling another tool; log:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("no skip logged for the unknown kind; log:\n%s", out)
	}
	if strings.Contains(out, "Abacus/finance.md") {
		t.Errorf("a clean section was warned about; log:\n%s", out)
	}
}
