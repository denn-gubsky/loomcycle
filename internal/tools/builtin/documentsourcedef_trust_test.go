package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// trust: asserted makes a peer's statements hold here without review — the
// operator's decision, like accepting them. Only the operator plane declares it;
// a run (or a member) may author a source only as proposed. A fork inherits its
// parent's trust, so a run's fork of a trusted source must say proposed.
func TestDocumentSourceDef_OnlyTheOperatorPlaneDeclaresAssertedTrust(t *testing.T) {
	tool, run, cleanup := documentSourceDefFixture(t)
	defer cleanup()
	op := tools.WithCallerTier(run, tools.TierOperatorPlane)
	member := tools.WithCallerTier(run, tools.TierMember)
	exec := func(ctx context.Context, body string) (bool, string) {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return !res.IsError, res.Text
	}
	const trusted = `{"op":"create","name":"vouched","overlay":{"config":{"base_url":"https://local.example.com"},"trust":"asserted"}}`

	for name, ctx := range map[string]context.Context{"run": run, "member": member} {
		if ok, text := exec(ctx, trusted); ok || !strings.Contains(text, "trust") {
			t.Errorf("a %s declared trust asserted: ok=%v %s", name, ok, text)
		}
	}
	if ok, text := exec(run, `{"op":"create","name":"vouched","overlay":{"config":{"base_url":"https://local.example.com"},"trust":"blindly"}}`); ok {
		t.Errorf("an unknown trust value was accepted: %s", text)
	}
	if ok, text := exec(op, trusted); !ok {
		t.Fatalf("the operator plane could not declare trust asserted: %s", text)
	}
	if ok, text := exec(run, `{"op":"fork","name":"vouched","overlay":{"config":{"api_version":"v2"}}}`); ok {
		t.Errorf("a run's fork kept the parent's asserted trust: %s", text)
	}
	ok, text := exec(run, `{"op":"fork","name":"vouched","overlay":{"config":{"api_version":"v2"},"trust":"proposed"}}`)
	if !ok {
		t.Fatalf("a run's fork that declares proposed was refused: %s", text)
	}
	def, _ := json.Marshal(decodeResult(t, text)["definition"])
	if !strings.Contains(string(def), `"trust":"proposed"`) {
		t.Errorf("forked definition = %s, want trust proposed", def)
	}
}
