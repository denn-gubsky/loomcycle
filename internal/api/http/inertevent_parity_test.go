package http

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// An EVENT enumerates across transports exactly the way an override input does,
// and this line has already shipped three separate gaps of that shape: the MCP
// schema that hid the overrides, the Python stubs that dropped them, and the
// steer path that carried none.
//
// So the moment a new server-generated event exists, its surfaces get a guard —
// before the gap rather than after it. Each entry is a SURFACE, not a file: what
// is checked is that the surface names the event and its payload, because a
// transport that knows the type and not the payload delivers an empty frame.
func TestCapabilityInertEvent_EveryTransportCarriesIt(t *testing.T) {
	for _, tc := range []struct {
		what  string
		path  string
		wants []string
	}{
		{
			what: "the gRPC proto",
			path: "../../../proto/loomcycle.proto",
			// The message AND the field on Event: declaring the payload type and
			// not wiring it to the frame is a silent no-op.
			wants: []string{"message CapabilityInertInfo", "capability_inert = "},
		},
		{
			what:  "the gRPC event mapper",
			path:  "../grpc/server.go",
			wants: []string{"ev.CapabilityInert != nil", "loomcyclepb.CapabilityInertInfo{"},
		},
		{
			what: "the TS adapter",
			path: "../../../adapters/ts/src/types.ts",
			// The union member, the payload interface, and the field on the event
			// — all three, because a consumer needs to narrow on the type AND
			// reach the payload.
			wants: []string{`"capability_inert"`, "interface CapabilityInertInfo", "capability_inert?:"},
		},
		{
			what: "the Python adapter",
			path: "../../../adapters/python/loomcycle/events.py",
			// The DECODE and the ATTACH are separate steps, and forgetting the
			// second is a mistake this project has actually made: the payload is
			// built and then never put on the event.
			wants: []string{"class CapabilityInertInfo", `HasField("capability_inert")`, "capability_inert=ci"},
		},
		{
			what:  "the runtime's own event vocabulary",
			path:  "../../providers/provider.go",
			wants: []string{"EventCapabilityInert EventType", "CapabilityInert *CapabilityInertInfo"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Skipf("%s not readable from here: %v", tc.path, err)
			}
			src := string(b)
			var missing []string
			for _, w := range tc.wants {
				if !strings.Contains(src, w) {
					missing = append(missing, w)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s does not carry: %s\n\nAn event present on one transport and absent "+
					"from another is DROPPED there silently — the consumer sees nothing and has no "+
					"way to know there was something to see.", tc.what, strings.Join(missing, ", "))
			}
		})
	}
}

// The crossing: a definition with an inert grant must put the event on the run's
// own stream, not merely be computable from the config.
func TestCapabilityInert_ReachesTheRunsEventStream(t *testing.T) {
	var got []providers.Event
	emit := func(ev providers.Event) { got = append(got, ev) }

	emitInertCapabilityWarnings(config.AgentDef{
		Tools:          []string{"AgentDef", "Memory"},
		AgentDefScopes: nil, // inert
		MemoryScopes:   nil, // defaults now — must NOT be reported
	}, emit)

	if len(got) != 1 {
		t.Fatalf("emitted %d events, want 1 (AgentDef only — Memory defaults and is not inert)", len(got))
	}
	ev := got[0]
	if ev.Type != providers.EventCapabilityInert {
		t.Errorf("type = %q", ev.Type)
	}
	if ev.CapabilityInert == nil {
		t.Fatal("the event carried no payload — a reader sees that something is wrong and not what")
	}
	if ev.CapabilityInert.Tool != "AgentDef" || ev.CapabilityInert.Gate != "agent_def_scopes" {
		t.Errorf("payload = %+v, want AgentDef/agent_def_scopes", ev.CapabilityInert)
	}
	// Text carries the same line, so a consumer that renders only Text is not
	// left with an empty frame.
	if !strings.Contains(ev.Text, "agent_def_scopes") {
		t.Errorf("Text does not name the gate: %q", ev.Text)
	}
}

// A fully-gated agent emits nothing. An advisory that fires on healthy configs
// is one operators filter out, taking the real ones with it.
func TestCapabilityInert_SaysNothingAboutAFullyGatedAgent(t *testing.T) {
	var n int
	emitInertCapabilityWarnings(config.AgentDef{
		Tools:          []string{"AgentDef", "Memory", "Channel"},
		AgentDefScopes: []string{"self"},
		Channels:       config.AgentChannelACL{Publish: []string{"x"}},
	}, func(providers.Event) { n++ })
	if n != 0 {
		t.Errorf("emitted %d events for a fully-gated agent", n)
	}
}
