package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// A runtime-authored cadence tick: no agent, no prompt, just a channel. This
// is the shape a canvas writes when it wires a clock into a workflow, so it
// has to survive create → definition JSON → the sweeper's read shape.
func TestScheduleDefTool_CreateChannelDelivery(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"create","name":"wave-tick","overlay":{"delivery":"channel","channel":"wave-in","schedule":"0 * * * *","metadata":{"batch":"nightly"}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}

	created := decodeResult(t, res.Text)
	defID, _ := created["def_id"].(string)
	if defID == "" {
		t.Fatalf("create returned no def_id: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	def, ok := out["definition"].(map[string]any)
	if !ok {
		t.Fatalf("no definition in %s", res.Text)
	}
	if def["delivery"] != "channel" || def["channel"] != "wave-in" {
		t.Errorf("delivery/channel did not round-trip: %v", def)
	}
	if _, present := def["agent"]; present {
		t.Errorf("a channel tick stored an agent: %v", def)
	}
}

// The substrate refuses the same mixes the yaml validator does — an agent
// authoring a def must not be able to create the shape the operator can't.
func TestScheduleDefTool_ChannelDeliveryRefusesRunShapedFields(t *testing.T) {
	cases := []struct {
		name    string
		overlay string
		want    string
	}{
		{"agent", `{"delivery":"channel","channel":"c","schedule":"0 * * * *","agent":"job-search-batch"}`, "forbids agent"},
		{"prompt", `{"delivery":"channel","channel":"c","schedule":"0 * * * *","prompt":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`, "forbids prompt"},
		{"on_complete", `{"delivery":"channel","channel":"c","schedule":"0 * * * *","on_complete":[{"kind":"memory.set","scope":"agent","key":"k"}]}`, "forbids on_complete"},
		{"credentials", `{"delivery":"channel","channel":"c","schedule":"0 * * * *","user_credentials":{"jobs":"t"}}`, "forbids credentials"},
		{"no channel", `{"delivery":"channel","schedule":"0 * * * *"}`, "requires channel"},
		{"unknown delivery", `{"delivery":"smoke-signal","schedule":"0 * * * *"}`, "unknown delivery"},
		{"channel on a run", `{"agent":"job-search-batch","channel":"c","schedule":"0 * * * *"}`, "forbids channel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := scheduleDefFixture(t)
			defer cleanup()
			res, _ := tool.Execute(ctx, json.RawMessage(
				`{"op":"create","name":"bad-tick","overlay":`+c.overlay+`}`))
			if !res.IsError {
				t.Fatalf("create with %s succeeded; want a refusal", c.name)
			}
			if !strings.Contains(res.Text, c.want) {
				t.Errorf("refusal = %q, want it to mention %q", res.Text, c.want)
			}
		})
	}
}
