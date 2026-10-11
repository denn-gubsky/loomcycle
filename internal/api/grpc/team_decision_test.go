package grpc

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A team walk's decision reaches gRPC clients with its payload, and only on
// its own event type.
func TestEventToProto_CarriesTheTeamDecision(t *testing.T) {
	answer := `{"model":"decide","answers":{"route":{"type":"choice","choice":"billing"}}}`
	out := eventToProto(providers.Event{Type: providers.EventTeamDecision, TeamDecision: &providers.TeamDecisionInfo{
		State: "triage", Visit: 2, Edge: "conditional:billing", Next: "billing-desk", Answer: json.RawMessage(answer),
	}})
	td := out.GetTeamDecision()
	if out.GetType() != "team_decision" || td.GetState() != "triage" || td.GetVisit() != 2 ||
		td.GetEdge() != "conditional:billing" || td.GetNext() != "billing-desk" || string(td.GetAnswer()) != answer {
		t.Errorf("proto = %+v", out)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "x"}).GetTeamDecision() != nil {
		t.Error("a text frame carries a team decision")
	}
}

// Every field of the team_decision payload is on every transport: the proto
// message, the TS adapter's interface, the Python adapter's dataclass and its
// decode. The fields are read from the Go struct, so one added there fails
// here until each transport carries it.
func TestTeamDecision_FourWayDrift(t *testing.T) {
	var fields []string
	rt := reflect.TypeOf(providers.TeamDecisionInfo{})
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		fields = append(fields, name)
	}
	if len(fields) < 5 {
		t.Fatalf("read %d fields from TeamDecisionInfo", len(fields))
	}
	for _, tc := range []struct {
		what, path, start, end string
		spell                  func(string) []string
	}{
		{"the proto", "../../../proto/loomcycle.proto", "message TeamDecision {", "\n}",
			func(f string) []string { return []string{" " + f + " = "} }},
		{"the TS adapter", "../../../adapters/ts/src/types.ts", "export interface TeamDecisionInfo {", "\n}",
			func(f string) []string { return []string{" " + f + ":", " " + f + "?:"} }},
		{"the Python dataclass", "../../../adapters/python/loomcycle/events.py", "class TeamDecision:", "\n\n\n",
			func(f string) []string { return []string{" " + f + ":"} }},
		{"the Python decode", "../../../adapters/python/loomcycle/events.py", "td = TeamDecision(", ")",
			func(f string) []string { return []string{f + "=d." + f} }},
	} {
		b, err := os.ReadFile(tc.path)
		if err != nil {
			t.Skipf("%s not readable from here: %v", tc.path, err)
		}
		src := string(b)
		i := strings.Index(src, tc.start)
		if i < 0 {
			t.Fatalf("%s: %q not found", tc.what, tc.start)
		}
		blk := src[i:]
		if j := strings.Index(blk, tc.end); j >= 0 {
			blk = blk[:j]
		}
		for _, f := range fields {
			found := false
			for _, s := range tc.spell(f) {
				found = found || strings.Contains(blk, s)
			}
			if !found {
				t.Errorf("%s does not carry team_decision.%s", tc.what, f)
			}
		}
	}
}
