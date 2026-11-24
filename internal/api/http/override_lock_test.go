package http

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// overrideSamples is a valid JSON value for each lockable override, as a run
// request carries it. A new lockable override needs a sample here, which is
// what makes the per-field refusal test cover it.
var overrideSamples = map[string]string{
	"model": `"stub-model"`, "provider": `"scripted"`, "tier": `"middle"`, "effort": `"low"`, "routing": `{"queue_aware":false}`,
	"user_tier":  `"default"`,
	"max_tokens": `64`, "max_iterations": `3`, "unbounded_iterations": `true`, "max_concurrent_children": `1`,
	"run_timeout_seconds": `30`,
	"sampling":            `{"temperature":0}`, "compaction": `{"enabled":false}`, "context": `{"mode":"compact"}`,
	"max_context_tokens": `4096`, "tool_choice": `{"mode":"auto"}`,
	"output_format":     `{"schema":{"type":"object"}}`,
	"web_search_filter": `"x"`, "retry_attempts": `1`,
	"memory_inject_max_tokens": `10`, "memory_index_max_bytes": `10`, "inject_tool_guide": `false`,
	"interactive": `true`, "review": `true`, "review_ttl_seconds": `60`,
	"interruption": `{"enabled":false}`, "hooks": `{"run_end":["some-hook"]}`,
	"tool_hooks": `{"Bash":{"pre":["some-hook"]}}`,
}

func runBodyWith(agent, field, value string) string {
	return `{"agent":"` + agent + `","` + field + `":` + value +
		`,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
}

// A token whose overrides are locked is refused a run request that sets any
// per-run override — every one of them, one at a time — and is told which.
// The refusal comes before anything else looks at the value, so it does not
// depend on the value being one the agent would have accepted.
func TestLockedToken_EveryOverrideIsRefusedOnARunStart(t *testing.T) {
	ts, _ := restrictedFixture(t)
	for _, field := range auth.LockableOverrides {
		sample, ok := overrideSamples[field]
		if !ok {
			t.Errorf("no sample value for lockable override %q: add one to overrideSamples", field)
			continue
		}
		code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/runs", runBodyWith("other", field, sample))
		if code != 403 || !strings.Contains(body, "override_not_allowed") || !strings.Contains(body, field) {
			t.Errorf("%s: status %d, want 403 override_not_allowed naming it: %s", field, code, body)
		}
	}
	// With none set, the same token runs the agent on its own settings.
	if code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/runs", runBody("other")); code != 200 {
		t.Errorf("a locked token with no overrides: status %d: %s", code, body)
	}
}

// What is not an override stays usable: fields that can only narrow the run,
// bound it, or name and deliver it.
func TestLockedToken_NarrowingAndDeliveryFieldsStillWork(t *testing.T) {
	ts, _ := restrictedFixture(t)
	for field, value := range map[string]string{
		"tools":            `[]`,
		"allowed_hosts":    `["example.com"]`,
		"max_wall_seconds": `60`,
		"metadata":         `{"k":"v"}`,
		"idempotency_key":  `"k-1"`,
		"agent_id":         `"a_locked_1"`,
		"user_bearer":      `"x"`,
	} {
		code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/runs", runBodyWith("other", field, value))
		if strings.Contains(body, "override_not_allowed") {
			t.Errorf("%s was refused as an override (status %d): %s", field, code, body)
		}
	}
}

// The list form permits exactly the named overrides, and a token with no
// policy is not affected at all.
func TestLockedToken_TheAllowListPermitsOnlyWhatItNames(t *testing.T) {
	ts, _ := restrictedFixture(t)
	if code, body := bearerCall(t, ts, effortOnlyBearer, "POST", "/v1/runs", runBodyWith("other", "max_tokens", `64`)); code != 403 || !strings.Contains(body, "override_not_allowed") {
		t.Errorf("an override outside the list: status %d, want 403: %s", code, body)
	}
	if _, body := bearerCall(t, ts, effortOnlyBearer, "POST", "/v1/runs", runBodyWith("other", "effort", `"low"`)); strings.Contains(body, "override_not_allowed") {
		t.Errorf("the allowed override was refused: %s", body)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runBodyWith("other", "max_tokens", `64`)); code != 200 {
		t.Errorf("a token with no policy setting max_tokens: status %d: %s", code, body)
	}
}

// The other ways to set an override are held to the same policy: a
// continuation, a draft, a batch child, and a retune of a run that is going.
func TestLockedToken_EveryOtherOverridePathIsHeldToThePolicy(t *testing.T) {
	s, ts, st := restrictedFixtureServer(t)
	ctx := t.Context()

	sess, err := st.CreateSession(ctx, "acme", "other", "dev")
	if err != nil {
		t.Fatal(err)
	}
	msg := `{"max_tokens":64,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`
	if code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/sessions/"+sess.ID+"/messages", msg); code != 403 || !strings.Contains(body, "override_not_allowed") {
		t.Errorf("a continuation with an override: status %d, want 403: %s", code, body)
	}

	draft := `{"agent":"other","start":false,"max_tokens":64,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/runs", draft); code != 403 || !strings.Contains(body, "override_not_allowed") {
		t.Errorf("a draft with an override: status %d, want 403: %s", code, body)
	}

	batch := `{"spawns":[` + runBody("other") + `,` + runBodyWith("other", "model", `"stub-model"`) + `]}`
	code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/runs:batch", batch)
	if code != 200 || strings.Count(body, "override not allowed") != 1 {
		t.Errorf("batch: status %d, want the one child with an override refused: %s", code, body)
	}

	// A retune: the policy is checked before the run is even looked at for
	// its settings, so a finished run of the token's own is enough to show it.
	start := `{"agent":"other","agent_id":"a_lock_retune","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, body := bearerCall(t, ts, lockedBearer, "POST", "/v1/runs", start); code != 200 {
		t.Fatalf("start: status %d: %s", code, body)
	}
	run, err := st.GetRunByAgentID(ctx, "a_lock_retune")
	if err != nil {
		t.Fatal(err)
	}
	locked := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev",
		Scopes:       []string{auth.ScopeRunsCreate},
		Restrictions: auth.Restrictions{Overrides: &auth.OverridePolicy{}}})
	free := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate}})
	if _, err := s.retuneRun(locked, run, &runOverridesWire{Model: "stub-model"}); err == nil || !strings.Contains(err.Error(), "override not allowed") {
		t.Errorf("a retune by a locked token: err = %v, want it refused", err)
	}
	if _, err := s.retuneRun(free, run, &runOverridesWire{Model: "stub-model"}); err != nil && strings.Contains(err.Error(), "override not allowed") {
		t.Errorf("a retune by an unlocked token was refused as locked: %v", err)
	}
}

// Every field of every run request shape is classified: a per-run override a
// token can be locked out of, or a field the lock deliberately leaves alone.
// A new request field fails this test until someone decides which it is —
// without that, a new override would be settable by a token locked to "none".
func TestOverrideLock_EveryRequestFieldIsClassified(t *testing.T) {
	// Not overrides: identity and delivery, the content itself, and fields
	// that can only narrow or bound the run.
	exempt := map[string]string{
		"Agent": "which agent — the allowlist's business", "Start": "draft or run", "Segments": "the content",
		"Prompt": "the content", "Tools": "narrow-only", "AllowedHosts": "narrow-only (a locked caller's list stays a narrowing in caller-authoritative mode: callerHostsNarrowOnly)",
		"SessionID": "identity", "TenantID": "identity (the principal wins)", "UserID": "identity (the principal wins)",
		"AgentID": "identity", "UserBearer": "the caller's own credential", "UserCredentials": "the caller's own credentials",
		"ParentContext": "lineage labels", "IdempotencyKey": "delivery", "ClientIdempotencyKey": "delivery",
		"DeliveryAltKey": "delivery", "MaxWallSeconds": "bounds the run's life; cannot extend it",
		"Retention": "narrow-only: it can only make a run store less, never more",
		"Metadata":  "non-secret labels", "PayloadMetadata": "set by a trigger, not a caller",
		"OperatorKeyRestricted": "server-derived", "Isolated": "server-derived",
		"CredentialsNoUserScope": "set by a trigger, never by a request; it only narrows",
		"ACLOwner":               "set by a trigger from its definition's recorded owner, never by a request; it only narrows",
		"ConfiguredRunID":        "which draft to start", "ConfiguredDraft": "the stored draft", "Detached": "delivery",
		"QueuedRunID":               "which queued run a restart starts; set by the boot, never by a request",
		"Admission":                 "delivery: wait for a run slot or be refused at once; it takes no capacity the caps do not already bound",
		"RateAdmitted":              "server-set: the HTTP route already counted the start; never a wire field",
		"SessionLockAcrossReplicas": "server-set: how the session lock is taken; never a wire field",
		"HoldUntilEnd":              "server-set: how long the call holds the run's session and slot; never a wire field",
	}
	lockable := map[string]string{} // Go field → wire name
	for wire, field := range lockableOverrideFields {
		lockable[field] = wire
	}

	// The HTTP layer's map and auth's list are the same set.
	var mapped []string
	for wire := range lockableOverrideFields {
		mapped = append(mapped, wire)
	}
	sort.Strings(mapped)
	listed := append([]string(nil), auth.LockableOverrides...)
	sort.Strings(listed)
	if !reflect.DeepEqual(mapped, listed) {
		t.Fatalf("lockableOverrideFields and auth.LockableOverrides differ:\n http: %v\n auth: %v", mapped, listed)
	}

	for _, shape := range []struct {
		name string
		typ  reflect.Type
		// full is true for a shape that must be able to carry EVERY lockable
		// override (so none can be set on it under another name).
		full bool
	}{
		{"runRequest", reflect.TypeOf(runRequest{}), true},
		{"runner.RunInput", reflect.TypeOf(runner.RunInput{}), true},
		{"messagesRequest", reflect.TypeOf(messagesRequest{}), false},
		{"runDraft", reflect.TypeOf(runDraft{}), false},
	} {
		seen := map[string]bool{}
		for _, f := range reflect.VisibleFields(shape.typ) {
			if f.Anonymous || !f.IsExported() {
				continue
			}
			seen[f.Name] = true
			if _, ok := lockable[f.Name]; ok {
				continue
			}
			if _, ok := exempt[f.Name]; ok {
				continue
			}
			t.Errorf("%s.%s is neither a lockable override nor an exempt field: classify it (auth.LockableOverrides + lockableOverrideFields, or the exempt list here)", shape.name, f.Name)
		}
		if shape.full {
			for field, wire := range lockable {
				if !seen[field] {
					t.Errorf("%s has no field %s for the lockable override %q", shape.name, field, wire)
				}
			}
		}
	}

	// A retune's own field list names only lockable overrides, so the check
	// in retuneRun covers everything a retune can change.
	full := runOverridesWire{}
	v := reflect.ValueOf(&full).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if _, ok := lockable[name]; !ok {
			t.Errorf("runOverridesWire.%s is a retune field that is not a lockable override", name)
		}
	}
}

func TestOverridesSetIn_ReadsEveryShapeAndPromotedFields(t *testing.T) {
	yes := true
	if got := overridesSetIn(&runRequest{Agent: "a", Model: "m", UnboundedIterations: &yes, Tools: []string{"x"}}); !reflect.DeepEqual(got, []string{"model", "unbounded_iterations"}) {
		t.Errorf("runRequest: %v", got)
	}
	if got := overridesSetIn(runner.RunInput{Effort: "low", Interactive: true}); !reflect.DeepEqual(got, []string{"effort", "interactive"}) {
		t.Errorf("RunInput by value: %v", got)
	}
	d := runDraft{}
	d.Model, d.RunTimeoutSeconds = "m", 5
	if got := overridesSetIn(&d); !reflect.DeepEqual(got, []string{"model", "run_timeout_seconds"}) {
		t.Errorf("draft (promoted + own field): %v", got)
	}
	if got := overridesSetIn(&runRequest{Agent: "a"}); len(got) != 0 {
		t.Errorf("no overrides set: %v", got)
	}
	var nilReq *runRequest
	if got := overridesSetIn(nilReq); got != nil {
		t.Errorf("nil request: %v", got)
	}
}

// A draft is held to the policy at both ends, on the surface gRPC and MCP use
// (the HTTP route is refused earlier, by the run handler). An edit is checked
// against the MERGED draft: a locked token must not be the one that runs an
// override somebody else wrote into it.
func TestLockedToken_DraftCreateAndEditAreHeldToThePolicy(t *testing.T) {
	s, _, _ := restrictedFixtureServer(t)
	ctx := t.Context()
	principal := func(r auth.Restrictions) auth.Principal {
		return auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, Restrictions: r}
	}
	locked := auth.WithPrincipal(ctx, principal(auth.Restrictions{Overrides: &auth.OverridePolicy{}}))
	free := auth.WithPrincipal(ctx, principal(auth.Restrictions{}))
	draft := func(model string) runDraft {
		var d runDraft
		d.Agent, d.Model = "other", model
		d.Segments = []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
		return d
	}

	if _, err := s.CreateConfiguredRun(locked, draft("stub-model")); err == nil || !strings.Contains(err.Error(), "override not allowed") {
		t.Errorf("a locked token creating a draft with an override: err = %v, want it refused", err)
	}
	plain, err := s.CreateConfiguredRun(locked, draft(""))
	if err != nil {
		t.Fatalf("a locked token creating a draft with no override: %v", err)
	}
	if _, err := s.UpdateConfiguredRun(locked, plain.RunID, []byte(`{"model":"stub-model"}`)); err == nil || !strings.Contains(err.Error(), "override not allowed") {
		t.Errorf("a locked token editing an override into its draft: err = %v, want it refused", err)
	}

	// Another caller's draft that already carries an override: the locked
	// token may not edit it, even with a patch that names no override.
	theirs, err := s.CreateConfiguredRun(free, draft("stub-model"))
	if err != nil {
		t.Fatalf("an unlocked token creating a draft with an override: %v", err)
	}
	if _, err := s.UpdateConfiguredRun(locked, theirs.RunID, []byte(`{"metadata":{"k":"v"}}`)); err == nil || !strings.Contains(err.Error(), "override not allowed") {
		t.Errorf("a locked token editing a draft that carries an override: err = %v, want it refused", err)
	}
	if _, err := s.UpdateConfiguredRun(free, theirs.RunID, []byte(`{"metadata":{"k":"v"}}`)); err != nil {
		t.Errorf("the unlocked token editing its own draft: %v", err)
	}
}
