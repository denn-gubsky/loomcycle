package extract

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pdfWith builds a one-page PDF whose catalog's EmbeddedFiles name tree is
// tree (an object number), from objs (object number → body), with a correct
// cross-reference table.
func pdfWith(objs map[int]string, tree int) []byte {
	all := map[int]string{
		1: fmt.Sprintf("<< /Type /Catalog /Pages 2 0 R /Names << /EmbeddedFiles %d 0 R >> >>", tree),
		2: "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		3: "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		4: "<< /Length 0 >>\nstream\n\nendstream",
	}
	for k, v := range objs {
		all[k] = v
	}
	n := 0
	for k := range all {
		n = max(n, k)
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offs := make([]int, n+1)
	for i := 1; i <= n; i++ {
		body, ok := all[i]
		if !ok {
			body = "null"
		}
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", n+1)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offs[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n+1, xref)
	return b.Bytes()
}

type pdfRecSink struct {
	memSink
	record []byte
}

func (s *pdfRecSink) Record(b []byte)       { s.record = b }
func (s *pdfRecSink) HeadingAnchor(string) {}

const recordFile = `<< /Type /Filespec /F (loomcycle.json) /EF << /F 9 0 R >> >>`
const recordStream = "<< /Length 7 /Type /EmbeddedFile >>\nstream\n{\"a\":1}\nendstream"

// The record is found in a kid of the name tree, not only in its root.
func TestPDFRecord_FoundInAKid(t *testing.T) {
	data := pdfWith(map[int]string{
		5: "<< /Kids [6 0 R] >>",
		6: "<< /Names [(other.txt) 8 0 R (loomcycle.json) 8 0 R] >>",
		8: recordFile, 9: recordStream,
	}, 5)
	s := &pdfRecSink{}
	if _, err := (PDF{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{}); err != nil {
		t.Fatal(err)
	}
	if string(s.record) != `{"a":1}` {
		t.Fatalf("record = %q", s.record)
	}
}

// A name tree whose kids point back at themselves, many times over, is
// walked once within a budget: the extraction returns promptly, and a
// record placed past the budget is not found.
func TestPDFRecord_ACyclicOrWideTreeIsBounded(t *testing.T) {
	kids := strings.Repeat("5 0 R ", 200)
	data := pdfWith(map[int]string{
		5: "<< /Kids [" + kids + "6 0 R] >>",
		6: "<< /Names [" + strings.Repeat("(x) 8 0 R ", pdfNameWalkBudget) + "(loomcycle.json) 8 0 R] >>",
		8: recordFile, 9: recordStream,
	}, 5)
	s := &pdfRecSink{}
	start := time.Now()
	if _, err := (PDF{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the walk took %v", time.Since(start))
	}
	if s.record != nil {
		t.Errorf("a record past the walk's budget was read: %q", s.record)
	}
}
