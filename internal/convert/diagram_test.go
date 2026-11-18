package convert

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/render"
)

// The diagram helper reads {in} and writes {out}, in a scratch folder that
// is gone afterwards; what it writes comes back within the format's cap.
func TestInProcess_DrawDiagramRunsTheHelper(t *testing.T) {
	root := t.TempDir()
	p := &InProcess{Scratch: scratchIn(root), Draw: &Helper{Key: DiagramHelperKey, Command: "cp {in} {out}", Timeout: 10 * time.Second}}
	src := []byte("flowchart LR\n  A --> B\n")
	if got, err := p.DrawDiagram(context.Background(), src); err != nil || !bytes.Equal(got, src) {
		t.Errorf("%q %v", got, err)
	}
	assertEmpty(t, root)

	for name, in := range map[string][]byte{"empty": nil, "too long": bytes.Repeat([]byte("x"), render.MaxDiagramSource+1)} {
		if _, err := p.DrawDiagram(context.Background(), in); !errors.Is(err, render.ErrTooLarge) {
			t.Errorf("%s: %v, want ErrTooLarge", name, err)
		}
	}
	big := &InProcess{Scratch: scratchIn(root), Draw: &Helper{Key: DiagramHelperKey, Command: "cp {in} {out}", Timeout: 10 * time.Second}}
	if _, err := big.DrawDiagram(context.Background(), bytes.Repeat([]byte("x"), render.MaxDiagramSource)); err != nil {
		t.Errorf("a source at the cap: %v", err)
	}
	if _, err := (&InProcess{}).DrawDiagram(context.Background(), src); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no helper: %v, want ErrNotConfigured", err)
	}
	missing := &InProcess{Scratch: scratchIn(root), Draw: &Helper{Key: DiagramHelperKey, Command: "loomcycle-no-such-renderer {in} {out}", Timeout: 10 * time.Second}}
	if _, err := missing.DrawDiagram(context.Background(), src); !errors.Is(err, ErrNoHelper) {
		t.Errorf("a renderer that is not installed: %v, want ErrNoHelper", err)
	}
	assertEmpty(t, root)
}

// The pinned renderer draws every diagram type loomcycle documents use, as
// a PNG within D25, and refuses a malformed diagram, which then stays code. Skipped where merman-cli is not
// installed; CI's render-office job requires it.
func TestInProcess_MermanDrawsTheDiagrams(t *testing.T) {
	if _, err := exec.LookPath(DiagramProgram); err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_MERMAN") != "" {
			t.Fatal("merman-cli is required here and not on PATH")
		}
		t.Skip("merman-cli not installed")
	}
	p := &InProcess{Scratch: scratchIn(t.TempDir()), Draw: &Helper{Key: DiagramHelperKey, Command: DefaultDiagramCommand, Timeout: render.DiagramTimeout}}
	diagrams := []string{
		"flowchart TD\n  A[Start] --> B{Is it?}\n  B -->|Yes| C[OK]\n  B -->|No| D[Протокол]\n",
		"sequenceDiagram\n  App->>Loom: POST /v1/runs\n  Loom-->>App: SSE\n  loop each tool\n    Loom->>Loom: tool_use\n  end\n",
		"classDiagram\n  class Doc {\n    +Title string\n  }\n  Doc <|-- Section\n",
		"stateDiagram-v2\n  [*] --> draft\n  draft --> confirmed\n  confirmed --> [*]\n",
		"erDiagram\n  DOCUMENT ||--o{ CHUNK : has\n",
		"gantt\n  title Plan\n  dateFormat YYYY-MM-DD\n  section P4\n  P4d :a1, 2026-11-18, 2d\n",
		"pie title Formats\n  \"pdf\" : 40\n  \"md\" : 60\n",
		"mindmap\n  root((render))\n    pdf\n    docx\n",
		"timeline\n  title GR\n  2026-11-16 : P0\n  2026-11-18 : P4d\n",
		// A click: a PNG holds no link, so it is only a picture.
		"flowchart LR\n  A[\"<script>alert(1)</script>\"] --> B\n  click A \"javascript:alert(1)\"\n",
		// Malformed: refused.
		"flowchart TD\n  A[[[ -->\n",
	}
	var md strings.Builder
	for _, d := range diagrams {
		md.WriteString("```mermaid\n" + d + "```\n\n")
	}
	{
		b := render.NewBuilder("Diagrams")
		b.Doc.Blocks = b.Body(md.String())
		doc, rep := b.Finish()
		draw := func(ctx context.Context, src string) ([]byte, error) { return p.DrawDiagram(ctx, []byte(src)) }
		if err := render.DrawDiagrams(context.Background(), doc, render.FormatPDF, draw, &rep); err != nil {
			t.Fatal(err)
		}
		if rep.DiagramsDrawn != len(diagrams)-1 || rep.DiagramsLeftAsCode != 1 {
			t.Errorf("drawn %d, left %d; want %d and 1 (notes %q)", rep.DiagramsDrawn, rep.DiagramsLeftAsCode, len(diagrams)-1, rep.Notes)
		}
		var out bytes.Buffer
		if err := render.Write(context.Background(), &out, doc, render.FormatDOCX, render.Options{}); err != nil {
			t.Errorf("writing the drawn diagrams: %v", err)
		}
	}
}
