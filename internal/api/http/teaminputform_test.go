package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A team's input form is checked when op=run starts the walk, over POST
// /v1/_teamdef, before the walk's run row or any member's model call.

// formEntryTeam starts from an input state carrying the pcparts form, then
// hands the captured part to one writer.
const formEntryTeam = `{"entry":"form","states":[` +
	`{"state":"form","handler":{"kind":"input",` +
	`"schema":{"type":"object","required":["document_id","chunk_id"],"properties":{` +
	`"document_id":{"type":"string","x-loomcycle-picker":{"kind":"document","scope":"user"}},` +
	`"chunk_id":{"type":"string","x-loomcycle-picker":{"kind":"chunk","document":"document_id","depth":1}}}},` +
	`"capture":{"chunk_id":"$.chunk_id"}}},` +
	`{"state":"write","handler":{"kind":"agent","agent":"writer","input_template":"Write about ${var.chunk_id}"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"form","to":"write","on":"success"},{"from":"write","to":"done","on":"success"}]}`

// createNamedTeam authors overlay as `name` through op=create.
func (h *docWalkHarness) createNamedTeam(name, overlay string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"op": "create", "name": name, "overlay": json.RawMessage(overlay)})
	if code, out := h.post(string(body)); code != http.StatusOK || strings.Contains(out, `"tool_refused"`) {
		h.t.Fatalf("create %s = %d: %s", name, code, out)
	}
}

// runTeamInput starts `name` with input and returns the status and body.
func (h *docWalkHarness) runTeamInput(name, input string) (int, string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"op": "run", "name": name, "input": input})
	return h.post(string(body))
}

// aliceRuns is every run filed under alice, the user op=run walks as.
func (h *docWalkHarness) aliceRuns() int {
	h.t.Helper()
	runs, err := h.st.ListActiveRunsByUser(context.Background(), "acme", "alice", "")
	if err != nil {
		h.t.Fatalf("list runs: %v", err)
	}
	return len(runs)
}

// assertFormRefused checks a refusal is the 422 tool envelope, classified as
// validation, naming want — and that nothing ran: no run row, no model call.
func (h *docWalkHarness) assertFormRefused(code int, out, want string) {
	h.t.Helper()
	if code != http.StatusUnprocessableEntity {
		h.t.Fatalf("run = %d: %s, want 422", code, out)
	}
	var env struct {
		Code     string `json:"code"`
		Error    string `json:"error"`
		Category string `json:"errorCategory"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		h.t.Fatalf("refusal is not the JSON envelope: %s", out)
	}
	if env.Code != "tool_refused" || env.Category != "validation" || !strings.Contains(env.Error, want) {
		h.t.Errorf("refusal = %+v, want a validation tool_refused naming %q", env, want)
	}
	if sent := h.sent(); len(sent) != 0 {
		h.t.Errorf("the provider saw %d member turns for a refused form: %v", len(sent), sent)
	}
	if n := h.aliceRuns(); n != 0 {
		h.t.Errorf("a refused form left %d run row(s)", n)
	}
}

func TestTeamDefRun_InputFormMissingFieldRefusedBeforeAnyRun(t *testing.T) {
	h := newDocWalkHarness(t)
	h.createNamedTeam("parts-form", formEntryTeam)

	code, out := h.runTeamInput("parts-form", `{"document_id":"d"}`)
	h.assertFormRefused(code, out, `input field "chunk_id" is required`)
}

func TestTeamDefRun_InputFormMistypedFieldRefusedNamingIt(t *testing.T) {
	h := newDocWalkHarness(t)
	h.createNamedTeam("parts-form", formEntryTeam)

	code, out := h.runTeamInput("parts-form", `{"document_id":7,"chunk_id":"c"}`)
	h.assertFormRefused(code, out, `input field "document_id" must be a string`)
}

// mode=detach returns a handle before the walk runs, so the check must come
// before that handle is minted too.
func TestTeamDefRun_InputFormRefusedBeforeADetachedRunIsMinted(t *testing.T) {
	h := newDocWalkHarness(t)
	h.createNamedTeam("parts-form", formEntryTeam)

	body, _ := json.Marshal(map[string]any{"op": "run", "name": "parts-form", "input": `"just text"`, "mode": "detach"})
	code, out := h.post(string(body))
	h.assertFormRefused(code, out, `input must be a JSON object, got a string`)
}

// The same form on an input-sourced Starter entry is checked the same way.
func TestTeamDefRun_InputStarterFormMissingFieldRefusedBeforeAnyRun(t *testing.T) {
	h := newDocWalkHarness(t)
	h.createNamedTeam("parts", inputTeam)

	code, out := h.runTeamInput("parts", `{"document_id":"d"}`)
	h.assertFormRefused(code, out, `input field "chunk_id" is required`)
}

// A form that is filled in runs: the check is what refuses, not something
// else that would refuse any input — and the run-row count the refusals
// assert to be zero does count this walk.
func TestTeamDefRun_InputFormFilledInRuns(t *testing.T) {
	h := newDocWalkHarness(t)
	h.createNamedTeam("parts-form", formEntryTeam)

	code, out := h.runTeamInput("parts-form", `{"document_id":"d","chunk_id":"c"}`)
	if code != http.StatusOK || !strings.Contains(out, `"run_id"`) {
		t.Fatalf("run = %d: %s", code, out)
	}
	if sent := h.sent(); len(sent) != 1 || sent[0] != "Write about c" {
		t.Errorf("member turns = %q, want one: Write about c", sent)
	}
	if n := h.aliceRuns(); n == 0 {
		t.Error("a walk that ran left no run row under alice — the refusal tests' zero would prove nothing")
	}
}
