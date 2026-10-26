package http

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// protoAnyFieldRe also reads `optional` fields, which protoFieldRe does not:
// TranscriptionInfo.cost is optional so an unpriced pass stays apart from a
// free one, and a parity check that skipped it would pass with it missing.
var protoAnyFieldRe = regexp.MustCompile(`(?m)^\s*(?:repeated\s+|optional\s+)?[\w.]+\s+([a-z0-9_]+)\s*=\s*\d+;`)

// The transcription record has four hand-written mirrors: the Go event payload
// every transport starts from, the proto message, the TS interface and the
// Python dataclass with its proto unwrap. Each pair is checked, derived from
// the Go struct's tags rather than a list here, so a field added to the record
// is a field every mirror must declare.
func TestTranscriptionInfo_EveryMirrorCarriesEveryField(t *testing.T) {
	want := jsonFields(t, providers.TranscriptionInfo{})
	if len(want) < 10 {
		t.Fatalf("only %d fields read from providers.TranscriptionInfo — the reflection has stopped matching", len(want))
	}

	proto := declaredNames(protoAnyFieldRe, sourceBlock(t, "../../../proto/loomcycle.proto", "message TranscriptionInfo {", "\n}"))
	ts := declaredNames(tsFieldRe, sourceBlock(t, "../../../adapters/ts/src/types.ts", "export interface TranscriptionInfo {", "\n}"))
	py := declaredNames(regexp.MustCompile(`(?m)^    ([a-z0-9_]+):`),
		sourceBlock(t, "../../../adapters/python/loomcycle/events.py", "class TranscriptionInfo:", "\n\n\n"))
	unwrap := sourceBlock(t, "../../../adapters/python/loomcycle/events.py", `if ev.HasField("transcription"):`, "\n        )\n")

	for _, mirror := range []struct {
		name string
		has  map[string]bool
	}{{"proto TranscriptionInfo", proto}, {"TS TranscriptionInfo", ts}, {"Python TranscriptionInfo", py}} {
		for _, f := range want {
			if !mirror.has[f] {
				t.Errorf("%s does not declare %q", mirror.name, f)
			}
		}
		for f := range mirror.has {
			if !slices.Contains(want, f) {
				t.Errorf("%s declares %q that providers.TranscriptionInfo does not carry", mirror.name, f)
			}
		}
	}
	for _, f := range want {
		if !strings.Contains(unwrap, "\n                "+f+"=") {
			t.Errorf("the Python _from_proto unwrap does not map %q", f)
		}
	}
}
