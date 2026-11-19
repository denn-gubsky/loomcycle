package memory

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseClassBody_AltNamesTheClassOtherNames: a class takes @alt like a concept,
// one name per line, the whole value kept (a name may hold spaces), in either
// spelling. An empty @alt is reported, and a class without one has none.
func TestParseClassBody_AltNamesTheClassOtherNames(t *testing.T) {
	d := ParseClassBody("@alt playbook\n- `@alt` operations manual\n@alt\n")
	if !reflect.DeepEqual(d.Alt, []string{"playbook", "operations manual"}) {
		t.Errorf("alt = %q", d.Alt)
	}
	if len(d.Issues) != 1 || !strings.Contains(d.Issues[0], "@alt names nothing") {
		t.Errorf("issues = %q, want the empty @alt reported", d.Issues)
	}
	if none := ParseClassBody("@identity\n"); none.Alt != nil {
		t.Errorf("a class without @alt has alt %q", none.Alt)
	}
}
