package teamgraph

import "testing"

// The presentation fields — Colors and Layout — must never reach the content
// hash. teamContent is a whitelist, so this holds by omission; these tests exist
// because the property is load-bearing (dragging a node on a canvas must not
// fork a definition) and a future edit to that struct could silently break it.

func baseDef() Definition {
	return Definition{
		Entry: "review",
		States: []State{
			{ID: "review", Handler: Handler{Kind: HandlerAgent, Agent: "reviewer"}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "review", To: "done", On: OnSuccess}},
	}
}

func TestSign_LayoutIsExcludedFromTheContentHash(t *testing.T) {
	plain := baseDef()
	positioned := baseDef()
	positioned.Layout = &Layout{Nodes: map[string]NodePos{
		"review": {X: 120, Y: 40, W: 220, H: 90},
		"done":   {X: 480, Y: 40},
	}}

	if got, want := Sign("sdlc", positioned), Sign("sdlc", plain); got != want {
		t.Errorf("laying out a graph changed its identity:\n  with layout: %s\n  without:     %s\n"+
			"Dragging a node must not mint a new version — add nothing to teamContent that is presentation.", got, want)
	}
}

func TestSign_ColorsRemainExcluded(t *testing.T) {
	plain := baseDef()
	coloured := baseDef()
	coloured.Colors = &Colors{States: map[string]string{"review": "#8ecae6"}}

	if Sign("sdlc", coloured) != Sign("sdlc", plain) {
		t.Error("recolouring changed the content hash; Colors must stay out of teamContent")
	}
}

// The complement: a node's prompts ARE content. Editing what a state tells its
// agent to do is a change to the workflow and must fork the definition — that is
// what makes one AgentDef safely serve N differently-roled states.

func TestSign_NodePromptsAreContentIdentifying(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Handler)
	}{
		{"system_prompt", func(h *Handler) { h.SystemPrompt = "You are a security reviewer." }},
		{"input_template", func(h *Handler) { h.InputTemplate = "Review the diff." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := baseDef()
			edited := baseDef()
			tc.apply(&edited.States[0].Handler)

			if Sign("sdlc", edited) == Sign("sdlc", plain) {
				t.Errorf("%s did not change the content hash; a node's prompt is content and must fork the def", tc.name)
			}
		})
	}
}

// Byte stability: a definition that sets NEITHER new field must hash to exactly
// what it hashed to before they were added. Both carry omitempty, so they
// marshal to nothing — but the hash input is raw json.Marshal output in
// declaration order, so a missing omitempty (or a reorder) would silently
// invalidate every content_sha256 already recorded against a stored TeamDef.
//
// The literal below was produced by the code as it stood BEFORE SystemPrompt and
// Layout existed. Do not regenerate it to make this test pass — a diff here
// means stored hashes just became wrong.
func TestSign_ByteStableForDefinitionsWithoutTheNewFields(t *testing.T) {
	d := Definition{Entry: "a", States: []State{{ID: "a", Handler: Handler{Kind: HandlerTerminal}}}}

	const before = "sha256:4db3ad92f133dda9bd89bcf2d6dc59daa1039e8de15ed16e2d4e60924506a0a0"
	if got := Sign("t", d); got != before {
		t.Errorf("content hash of an unchanged definition moved:\n  now:    %s\n  before: %s\n"+
			"Every stored TeamDef's content_sha256 just stopped matching.", got, before)
	}
}
