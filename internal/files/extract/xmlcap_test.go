package extract

import (
	"context"
	"strings"
	"testing"
)

// The character data one XML part may hold is capped.
func TestParseXML_TextIsCapped(t *testing.T) {
	big := `<w:document ` + nsW + `><w:body><w:p><w:r><w:t>` + strings.Repeat("я", 600) + `</w:t></w:r></w:p></w:body></w:document>`
	if _, err := parseXMLCapped(context.Background(), []byte(big), 1000); !IsRefused(err) {
		t.Errorf("err = %v, want a text-cap refusal", err)
	}
	if _, err := parseXMLCapped(context.Background(), []byte(big), 2000); err != nil {
		t.Errorf("under the cap: %v", err)
	}
}
