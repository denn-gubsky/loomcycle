package teamgraph

import "strings"

import "testing"

func defWith(states ...State) Definition {
	d := Definition{Entry: states[0].ID, States: states}
	for i := 0; i+1 < len(states); i++ {
		d.Transitions = append(d.Transitions, Transition{From: states[i].ID, To: states[i+1].ID, On: OnSuccess})
	}
	return d
}

func terminal(id string) State { return State{ID: id, Handler: Handler{Kind: HandlerTerminal}} }

// TRUST RULE 1. ${run.credentials.*} and ${run.user_bearer} are FAIL-CLOSED by
// design: unresolved, they drop their whole container rather than emit a
// placeholder. A vars state that could copy one into ${var.x} would convert a
// fail-closed secret into a fail-OPEN plaintext string — and that string is then
// a legitimate Memory key, a prompt fragment, and a value in every transcript,
// snapshot and prompt-cache entry downstream.
func TestValidate_VarsStateMayNotBindFromTheCredentialsNamespace(t *testing.T) {
	for _, expr := range []string{
		"${run.credentials.GITHUB_TOKEN}",
		"${run.credentials.GITHUB_TOKEN:-fallback}",
		"${run.user_bearer}",
		"${run.user_bearer:-fallback}",
		"prefix ${run.credentials.X} suffix",
	} {
		d := defWith(
			State{ID: "stamp", Handler: Handler{Kind: HandlerVars, Set: map[string]string{"leak": expr}}},
			terminal("done"),
		)
		err := Validate(d)
		if err == nil {
			t.Errorf("%s: accepted — a secret copied into a variable becomes plaintext in every transcript downstream", expr)
			continue
		}
		if !strings.Contains(err.Error(), "non-secret by construction") {
			t.Errorf("%s: error %q should explain WHY, not just refuse", expr, err)
		}
	}
}

// The complement: the non-secret ${run.*} identifiers stay allowed. They are
// exactly what a workflow legitimately carries into a Memory key.
func TestValidate_VarsStateMayBindNonSecretRunIdentifiers(t *testing.T) {
	d := defWith(
		State{ID: "stamp", Handler: Handler{Kind: HandlerVars, Set: map[string]string{
			"tenant": "${run.tenant_id}",
			"root":   "${run.root_run_id}",
			"when":   "${now.iso8601}",
		}}},
		terminal("done"),
	)
	if err := Validate(d); err != nil {
		t.Errorf("non-secret identifiers must remain bindable: %v", err)
	}
}

// Assignment belongs on a `vars` state, where a canvas draws it. An invisible
// assignment riding something that looks like an agent is what a visual editor
// exists to prevent.
func TestValidate_SetIsRefusedOnAnyOtherKind(t *testing.T) {
	d := defWith(
		State{ID: "a", Handler: Handler{Kind: HandlerAgent, Agent: "x", Set: map[string]string{"v": "1"}}},
		terminal("done"),
	)
	err := Validate(d)
	if err == nil || !strings.Contains(err.Error(), "where it is visible") {
		t.Errorf("err = %v, want a refusal naming the visibility reason", err)
	}
}

func TestValidate_VarsStateRequiresANonEmptySet(t *testing.T) {
	d := defWith(State{ID: "stamp", Handler: Handler{Kind: HandlerVars}}, terminal("done"))
	if err := Validate(d); err == nil {
		t.Error("a vars state with nothing to set is a state that spends a walk step doing nothing")
	}
}

// Capture paths go through the SAME strict-subset parser the webhook projector
// uses, so a definition cannot smuggle in a JSONPath construct the subset
// deliberately cannot express.
func TestValidate_CapturePathsUseTheStrictSubset(t *testing.T) {
	for _, bad := range []string{"$.a[*]", "$..a", "$.a[?(@.x)]", "a.b", "$.a[-1]"} {
		d := defWith(
			State{ID: "a", Handler: Handler{Kind: HandlerAgent, Agent: "x", Capture: map[string]string{"v": bad}}},
			terminal("done"),
		)
		if err := Validate(d); err == nil {
			t.Errorf("capture path %q accepted; the subset must stay unreachable", bad)
		}
	}
	d := defWith(
		State{ID: "a", Handler: Handler{Kind: HandlerAgent, Agent: "x", Capture: map[string]string{"v": "$.results[0].verdict"}}},
		terminal("done"),
	)
	if err := Validate(d); err != nil {
		t.Errorf("a well-formed capture path was rejected: %v", err)
	}
}

// A name that validates must be a name the expander can resolve — the charset
// is shared with the expander's regex, so a def cannot store a variable that is
// permanently unreadable.
func TestValidate_VariableNamesMatchWhatTheExpanderCanResolve(t *testing.T) {
	for _, bad := range []string{"has space", "has.dot", "", "has/slash"} {
		d := defWith(
			State{ID: "stamp", Handler: Handler{Kind: HandlerVars, Set: map[string]string{bad: "1"}}},
			terminal("done"),
		)
		if err := Validate(d); err == nil {
			t.Errorf("variable name %q accepted but the expander could never resolve it", bad)
		}
	}
}

func TestValidate_InputStateSchemaMustBeJSON(t *testing.T) {
	d := defWith(
		State{ID: "intake", Handler: Handler{Kind: HandlerInput, Schema: []byte(`{"type":`)}},
		terminal("done"),
	)
	if err := Validate(d); err == nil {
		t.Error("a malformed run-form schema should be refused at create, not discovered by a client")
	}
}

func TestValidate_NewKindsAcceptedInAWellFormedGraph(t *testing.T) {
	d := defWith(
		State{ID: "intake", Handler: Handler{Kind: HandlerInput, Schema: []byte(`{"type":"object"}`)}},
		State{ID: "stamp", Handler: Handler{Kind: HandlerVars, Set: map[string]string{"at": "${now.date}"}}},
		State{ID: "review", Handler: Handler{Kind: HandlerAgent, Agent: "reviewer",
			Capture: map[string]string{"verdict": "$.verdict"}}},
		terminal("done"),
	)
	if err := Validate(d); err != nil {
		t.Errorf("a well-formed graph using the new kinds was rejected: %v", err)
	}
}
