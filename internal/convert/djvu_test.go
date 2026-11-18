//go:build unix

package convert

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/convert/djvutest"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

var updateDjVu = flag.Bool("update-djvu", false, "rewrite the DjVu golden Markdown under internal/files/extract/testdata/djvu (needs DjVuLibre)")

func djvuFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "files", "extract", "testdata", "djvu", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pagedSink is a PageSink that keeps the Markdown.
type pagedSink struct {
	b      strings.Builder
	starts []int
}

func (s *pagedSink) Text(md string) error                { s.b.WriteString(md); return nil }
func (s *pagedSink) Image(extract.Image) (string, error) { return "", nil }
func (s *pagedSink) StartPage(n int)                     { s.starts = append(s.starts, n) }

func djvuInProcess(root string, inspect bool) *InProcess {
	p := &InProcess{Scratch: scratchIn(root), DjVuText: &Helper{Key: DjVuTextHelperKey, Command: DefaultDjVuTextCommand}}
	if inspect {
		p.DjVuInspect = &Helper{Key: DjVuInspectHelperKey, Command: DefaultDjVuInspectCommand}
	}
	return p
}

// TestDjVuHelper_ScriptIsOneOfTheFixedTwo: the inspect helper runs only
// print-outline or print-meta, on the copy in the scratch folder; any other
// script is refused before anything runs, and so is a page list that is not
// one.
func TestDjVuHelper_ScriptIsOneOfTheFixedTwo(t *testing.T) {
	fake := djvutest.Install(t)
	root := t.TempDir()
	p := djvuInProcess(root, true)
	data := djvuFixture(t, "book.djvu")
	s, err := djvuHelpers{p}.Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []extract.DjVuScript{"set-txt /etc/passwd; save", "print-txt", "", "print-meta;save"} {
		if _, _, err := s.Inspect(context.Background(), bad, 1<<10); err == nil {
			t.Errorf("script %q ran", bad)
		}
	}
	for _, bad := range []string{"", "-1", "1;id", "1 2", "1,", "x", strings.Repeat("1,", 300) + "1", "1--2", "1-2-3"} {
		if _, _, err := s.Text(context.Background(), bad, 1<<10); err == nil {
			t.Errorf("page list %q ran", bad)
		}
	}
	if calls := fake.Calls(t); len(calls) != 0 {
		t.Fatalf("refused calls reached the helper: %q", calls)
	}
	out, _, err := s.Inspect(context.Background(), extract.DjVuMeta, 1<<10)
	if err != nil || !strings.Contains(string(out), "A Stand-in Title") {
		t.Fatalf("print-meta: %q, %v", out, err)
	}
	in := filepath.Join(root, "")
	calls := fake.Calls(t)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "djvused -e print-meta "+in) || !strings.HasSuffix(calls[0], "/book.djvu") {
		t.Errorf("calls = %q, want djvused -e print-meta on the scratch copy", calls)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, root)
}

// The extractor reads a DjVu through the helpers of an InProcess that has
// them, and only then: with no text helper a DjVu is not a type it reads.
func TestInProcess_DjVuIsReadOnlyWithTheTextHelper(t *testing.T) {
	fake := djvutest.Install(t)
	data := djvuFixture(t, "book.djvu")
	if _, err := (&InProcess{}).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &pagedSink{}, extract.Options{Name: "b.djvu"}); !errors.Is(err, extract.ErrUnsupported) {
		t.Fatalf("no helper: %v, want ErrUnsupported", err)
	}
	root := t.TempDir()
	var sink pagedSink
	rep, err := djvuInProcess(root, true).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &sink, extract.Options{Name: "b.djvu"})
	if err != nil {
		t.Fatal(err)
	}
	md := sink.b.String()
	for _, want := range []string{"# A Stand-in Title", "## Chapter From The Stand-in\n\n## Page 1\n\nText of page 1", "## Page 2\n\n## Page 3\n\nText of page 3"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if rep.Format != "djvu" || rep.Pages != 3 || rep.Metadata == nil || rep.Metadata.Authors[0] != "Ada Example" {
		t.Errorf("report = %+v", rep)
	}
	if !slices.Equal(sink.starts, []int{1, 2, 3}) {
		t.Errorf("page starts = %v", sink.starts)
	}
	calls := fake.Calls(t)
	if len(calls) != 3 || !strings.HasPrefix(calls[2], "djvutxt --detail=page --page=1,3 ") {
		t.Errorf("calls = %q", calls)
	}
	assertEmpty(t, root)

	// The text helper missing where the extraction runs fails it at once,
	// as a helper to install.
	p := &InProcess{Scratch: scratchIn(t.TempDir()), DjVuText: &Helper{Key: DjVuTextHelperKey, Command: "loomcycle-no-such-djvutxt --page={pages} {in}"}}
	if _, err := p.Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &pagedSink{}, extract.Options{}); !errors.Is(err, ErrNoHelper) {
		t.Errorf("missing program: %v, want ErrNoHelper", err)
	}
}

// requireDjVuLibre skips where DjVuLibre is not installed, unless
// LOOMCYCLE_REQUIRE_DJVULIBRE says it must be (CI).
func requireDjVuLibre(t *testing.T) {
	t.Helper()
	for _, prog := range []string{DjVuTextProgram, DjVuInspectProgram} {
		if _, err := exec.LookPath(prog); err != nil {
			if os.Getenv("LOOMCYCLE_REQUIRE_DJVULIBRE") != "" {
				t.Fatalf("%s is required here and not on PATH", prog)
			}
			t.Skipf("%s not installed", prog)
		}
	}
}

// TestInProcess_DjVuLibreReadsTheFixtures runs DjVuLibre itself over the
// fixtures with the default commands: the text layer, the outline and the
// metadata come out as the golden Markdown says, pages asks only for those
// pages, and an indirect document is refused before a helper runs.
func TestInProcess_DjVuLibreReadsTheFixtures(t *testing.T) {
	requireDjVuLibre(t)
	for _, name := range []string{"single.djvu", "book.djvu", "scan.djvu"} {
		data := djvuFixture(t, name)
		var sink pagedSink
		rep, err := djvuInProcess(t.TempDir(), true).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &sink, extract.Options{Name: name})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		golden := filepath.Join("..", "files", "extract", "testdata", "djvu", name+".md")
		if *updateDjVu {
			if err := os.WriteFile(golden, []byte(sink.b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if sink.b.String() != string(want) {
			t.Errorf("%s:\n%s\nwant\n%s", name, sink.b.String(), want)
		}
		if name == "book.djvu" {
			m := rep.Metadata
			if m == nil || m.Title != "A Synthetic Book" || m.Year != 2019 || m.Publisher != "Loomcycle Test Press" || len(m.Authors) != 1 {
				t.Errorf("metadata = %+v", m)
			}
			if len(rep.PageYields) != 3 || rep.PageYields[1].Chars != 0 || rep.PageYields[2].Chars == 0 {
				t.Errorf("yields = %+v", rep.PageYields)
			}
		}
	}

	data := djvuFixture(t, "book.djvu")
	var sink pagedSink
	rep, err := djvuInProcess(t.TempDir(), false).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &sink, extract.Options{Name: "book.djvu", Pages: []int{3}})
	if err != nil {
		t.Fatal(err)
	}
	if md := sink.b.String(); !strings.Contains(md, "Третья страница") || strings.Contains(md, "First page") || rep.Metadata != nil {
		t.Errorf("pages [3] without inspect:\n%s\nmetadata %+v", md, rep.Metadata)
	}

	data = djvuFixture(t, "indirect.djvu")
	if _, err := djvuInProcess(t.TempDir(), true).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &pagedSink{}, extract.Options{}); !extract.IsRefused(err) {
		t.Errorf("indirect: %v, want refused", err)
	}
}
