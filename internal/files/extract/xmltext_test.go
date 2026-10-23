package extract

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A part of many small elements separated by whitespace once grew its
// parent's text by a string copy per child: quadratic in the part's size.
func TestParseXML_ManyRunsIsLinear(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<w:document ` + nsW + `><w:body><w:p>`)
	for range 200_000 {
		b.WriteString("\n  <w:r><w:t>а</w:t></w:r>")
	}
	b.WriteString("\n</w:p></w:body></w:document>")
	start := time.Now()
	root, err := parseXML(context.Background(), []byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("parsing 200k runs took %v", d)
	}
	if got := len(root.all("t")); got != 200_000 {
		t.Errorf("runs = %d", got)
	}

}
