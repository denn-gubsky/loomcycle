package providers

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The spawn ledger rows reach a TS client as AgentEvent.spawn_child, typed by
// the hand-written SpawnChildInfo. A field added here and not there is on the
// wire but nameless to a typed consumer, so every json key of the struct must
// be declared in the interface. The Go side is read off the struct itself.
func TestSpawnChildEventInfo_TSMirrorDeclaresEveryField(t *testing.T) {
	var goFields []string
	st := reflect.TypeOf(SpawnChildEventInfo{})
	for i := range st.NumField() {
		if name, _, _ := strings.Cut(st.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			goFields = append(goFields, name)
		}
	}
	if len(goFields) < 10 {
		t.Fatalf("only %d json fields read off SpawnChildEventInfo — this guard is reading nothing", len(goFields))
	}
	ts, err := os.ReadFile("../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("adapters/ts/src/types.ts not readable from here: %v", err)
	}
	m := regexp.MustCompile(`(?s)export interface SpawnChildInfo \{(.*?)\n\}`).FindSubmatch(ts)
	if m == nil {
		t.Fatal("could not find SpawnChildInfo in types.ts")
	}
	declared := map[string]bool{}
	for _, f := range regexp.MustCompile(`(?m)^\s*([a-z0-9_]+)\??:`).FindAllSubmatch(m[1], -1) {
		declared[string(f[1])] = true
	}
	for _, f := range goFields {
		if !declared[f] {
			t.Errorf("SpawnChildEventInfo carries %q but TS SpawnChildInfo does not declare it", f)
		}
	}
}
