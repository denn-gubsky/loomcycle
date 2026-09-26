package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// setAgentHooks gives a harness agent its hooks, as its yaml would.
func setAgentHooks(s *Server, name string, ev hooks.EventHooks, tools hooks.ToolHooks) {
	cfg := s.cfg()
	a := cfg.Agents[name]
	a.Hooks, a.ToolHooks = ev, tools
	cfg.Agents[name] = a
}

const writeBody = `{"agent":"writer","segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`

// The crossing: the hooks an agent's definition carries fire on its run — no
// registration anywhere — and name where they came from.
func TestRunHooks_AnAgentsOwnHooksFireOnItsRun(t *testing.T) {
	h := newReviewHarness(t)
	end := newRecordingHook(t, `{}`)
	setAgentHooks(h.srv, "writer", hooks.EventHooks{
		hooks.PhaseRunEnd: {{Inline: &hooks.Inline{Name: "log", URL: end.srv.URL}}},
	}, nil)
	runID, _, frames, stop := h.start(writeBody)
	defer stop()
	h.waitFrame(frames, "done")
	body := end.waitBody(t, `"run_id":"`+runID+`"`)
	for _, want := range []string{`"phase":"run_end"`, `"owner":"agent:writer"`, `"hook_name":"log"`} {
		if !strings.Contains(body, want) {
			t.Errorf("payload lacks %s: %s", want, body)
		}
	}
	// Forgotten once the run ended.
	if h.srv.runHookSet(runID) != nil {
		t.Errorf("the finished run's hooks are still held")
	}
}

// A run of an agent that carries no hooks fires none — the hooks are the run's,
// not the server's.
func TestRunHooks_AnotherAgentsRunFiresNone(t *testing.T) {
	h := newReviewHarness(t)
	end := newRecordingHook(t, `{}`)
	cfg := h.srv.cfg()
	cfg.Agents["plain"] = config.AgentDef{Model: "stub-model", SystemPrompt: "plain"}
	setAgentHooks(h.srv, "writer", hooks.EventHooks{
		hooks.PhaseRunEnd: {{Inline: &hooks.Inline{Name: "log", URL: end.srv.URL}}},
	}, nil)
	_, _, frames, stop := h.start(`{"agent":"plain","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`)
	defer stop()
	h.waitFrame(frames, "done")
	time.Sleep(200 * time.Millisecond)
	end.mu.Lock()
	n := len(end.bodies)
	end.mu.Unlock()
	if n != 0 {
		t.Fatalf("writer's hook fired %d times on plain's run", n)
	}
}

// A HookDef the agent names is resolved when the run starts and fires in it.
func TestRunHooks_AHookDefReferenceFires(t *testing.T) {
	h := newReviewHarness(t)
	deny := newRecordingHook(t, `{"decision":"deny","reason":"not today"}`)
	def, _ := json.Marshal(hooks.Def{Event: hooks.PhaseAgentStart, Body: hooks.DefBody{Kind: hooks.BodyKindHTTP, URL: deny.srv.URL}})
	row, err := h.st.HookDefCreate(t.Context(), store.HookDefRow{DefID: "hdf_gate", Name: "gate", Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.HookDefSetActive(t.Context(), "", "gate", row.DefID, ""); err != nil {
		t.Fatal(err)
	}
	setAgentHooks(h.srv, "writer", hooks.EventHooks{hooks.PhaseAgentStart: {{Ref: "gate"}}}, nil)
	runID, _, _, stop := h.start(writeBody)
	defer stop()
	if !strings.Contains(deny.waitBody(t, `"phase":"agent_start"`), `"hook_name":"gate"`) {
		t.Errorf("the HookDef did not fire as gate")
	}
	waitRunStatus(t, h.st, runID, store.RunFailed)
	if n := len(h.prov.lastUsers); n != 0 {
		t.Errorf("the model was called %d times for a run its hook denied", n)
	}
}

// A HookDef an agent names but that does not exist stops the run before any
// model call: a gate the definition names must not silently be missing.
func TestRunHooks_AMissingHookDefStopsTheRun(t *testing.T) {
	h := newReviewHarness(t)
	setAgentHooks(h.srv, "writer", hooks.EventHooks{hooks.PhaseAgentStart: {{Ref: "no-such-hook"}}}, nil)
	runID, _, _, stop := h.start(writeBody)
	defer stop()
	run := waitRunStatus(t, h.st, runID, store.RunFailed)
	if !strings.Contains(run.ErrorMsg, "no-such-hook") {
		t.Errorf("run error = %q; want it to name the missing hook", run.ErrorMsg)
	}
	if n := len(h.prov.lastUsers); n != 0 {
		t.Errorf("the model was called %d times", n)
	}
}

// A sub-agent starts from its parent's ctx, which carries the parent's hooks;
// the child fires its own definition's, so a child with none gets an empty set.
func TestRunHooks_AChildDoesNotFireItsParentsHooks(t *testing.T) {
	h := newReviewHarness(t)
	parent := hooks.NewSet()
	if _, err := parent.Register(&hooks.Hook{Owner: "agent:parent", Name: "p", Phase: hooks.PhasePre, CallbackURL: "https://h.example"}); err != nil {
		t.Fatal(err)
	}
	ctx := h.srv.withRunHooks(hooks.WithSet(context.Background(), parent), "run_child", "child", config.AgentDef{}, hooks.Additions{})
	if got := hooks.SetFrom(ctx); got == nil || got.Len() != 0 {
		t.Fatalf("child set = %v; want an empty set in place of the parent's", got)
	}
}

// Host widening is decided when the run's hooks are resolved: the permit list
// names the hook AND the definition is the operator's.
func TestRunHooks_WideningNeedsThePermitAndAnOperatorAuthoredDefinition(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.hookPermits = hooks.NewPermits([]string{"url-gate", "acme:url-gate"})
	gate := hooks.EventHooks{hooks.PhasePre: {{Inline: &hooks.Inline{Name: "url-gate", URL: "https://h.example"}}}}
	cases := []struct {
		name string
		def  config.AgentDef
		want bool
	}{
		{"operator yaml", config.AgentDef{Hooks: gate, OperatorAuthored: true}, true},
		{"operator-authored tenant def", config.AgentDef{Hooks: gate, OperatorAuthored: true, OwnerTenant: "acme"}, true},
		{"agent-authored def", config.AgentDef{Hooks: gate, OwnerTenant: "acme"}, false},
		{"another tenant's def", config.AgentDef{Hooks: gate, OperatorAuthored: true, OwnerTenant: "other"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			set := hooks.SetFrom(h.srv.withRunHooks(context.Background(), "", "w", c.def, hooks.Additions{}))
			if got := set.List()[0].WidenPermitted; got != c.want {
				t.Fatalf("WidenPermitted = %v, want %v", got, c.want)
			}
		})
	}
}

func waitRunStatus(t *testing.T, st store.Store, runID string, want store.RunStatus) store.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		run, err := st.GetRun(t.Context(), runID)
		if err == nil && run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s status = %q (err %v), want %q", runID, run.Status, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The crossing for a hook's headers: the credential resolver the server is
// given reaches a real run's hook call, with the run's identity on its ctx.
func TestRunHooks_AHeaderCredentialIsResolvedForTheRun(t *testing.T) {
	h := newReviewHarness(t)
	got := make(chan string, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("Authorization"):
		default:
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hook.Close()
	var seenAgent string
	h.srv.SetHookHeaderSubstitute(func(ctx context.Context, s string) (string, []string, error) {
		seenAgent = tools.AgentName(ctx)
		return strings.ReplaceAll(s, "$cred:hook_secret", "s3cr3t"), nil, nil
	})
	setAgentHooks(h.srv, "writer", hooks.EventHooks{hooks.PhaseAgentStart: {{Inline: &hooks.Inline{
		Name: "gate", URL: hook.URL, Headers: map[string]string{"Authorization": "Bearer $cred:hook_secret"}}}}}, nil)
	_, _, frames, stop := h.start(writeBody)
	defer stop()
	h.waitFrame(frames, "done")
	select {
	case a := <-got:
		if a != "Bearer s3cr3t" {
			t.Fatalf("Authorization = %q", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hook was not called")
	}
	if seenAgent != "writer" {
		t.Fatalf("the resolver saw agent %q; want the run's", seenAgent)
	}
}

// The run request's hooks fire on the run, after the agent's own, named as the
// run's.
func TestRunHooks_ARunRequestAddsHooks(t *testing.T) {
	h := newReviewHarness(t)
	end := newRecordingHook(t, `{}`)
	body := `{"agent":"writer","segments":[{"role":"user","content":[{"type":"trusted-text","text":"write"}]}],
	  "hooks":{"run_end":[{"name":"audit","url":"` + end.srv.URL + `"}]}}`
	runID, _, frames, stop := h.start(body)
	defer stop()
	h.waitFrame(frames, "done")
	got := end.waitBody(t, `"run_id":"`+runID+`"`)
	if !strings.Contains(got, `"owner":"run"`) || !strings.Contains(got, `"hook_name":"audit"`) {
		t.Fatalf("payload = %s", got)
	}
	run, _ := h.st.GetRun(t.Context(), runID)
	if !strings.Contains(string(run.RunConfig), `"audit"`) {
		t.Fatalf("the run's record does not keep its additions: %s", run.RunConfig)
	}
}

// A caller's hook on a tool the agent does not have stops the run: the caller
// believes a gate is there.
func TestRunHooks_ARequestHookOnAToolTheAgentLacksStopsTheRun(t *testing.T) {
	h := newReviewHarness(t)
	body := `{"agent":"writer","segments":[{"role":"user","content":[{"type":"trusted-text","text":"write"}]}],
	  "tool_hooks":{"WebFetch":{"pre":[{"name":"gate","url":"https://h.example"}]}}}`
	runID, _, _, stop := h.start(body)
	defer stop()
	run := waitRunStatus(t, h.st, runID, store.RunFailed)
	if !strings.Contains(run.ErrorMsg, "WebFetch") {
		t.Fatalf("error = %q", run.ErrorMsg)
	}
}

// Additions follow the agent's own hooks, reach the sub-agents a run starts,
// and never widen hosts, whatever the permit list says.
func TestRunHooks_AdditionsFollowTheAgentsHooksAndReachSubAgents(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.hookPermits = hooks.NewPermits([]string{"gate", "own"})
	def := config.AgentDef{Tools: []string{"WebFetch"}, OperatorAuthored: true,
		Hooks: hooks.EventHooks{hooks.PhasePre: {{Inline: &hooks.Inline{Name: "own", URL: "https://h.example"}}}}}
	added := hooks.Additions{ToolHooks: hooks.ToolHooks{"WebFetch": {hooks.PhasePre: {{Inline: &hooks.Inline{Name: "gate", URL: "https://h.example"}}}}}}
	ctx := h.srv.withRunHooks(context.Background(), "", "parent", def, added)
	pre := hooks.SetFrom(ctx).Match("parent", "WebFetch", hooks.PhasePre)
	if len(pre) != 2 || pre[0].Name != "own" || pre[1].Name != "gate" {
		t.Fatalf("chain = %v; want the agent's hook, then the run's", pre)
	}
	if !pre[0].WidenPermitted || pre[1].WidenPermitted {
		t.Fatalf("widen = %v / %v; a run's addition must never widen", pre[0].WidenPermitted, pre[1].WidenPermitted)
	}
	// A child with no hooks of its own still fires what its parent run added.
	child := h.srv.withRunHooks(ctx, "", "child", config.AgentDef{Tools: []string{"WebFetch"}}, hooks.Additions{})
	if got := hooks.SetFrom(child).Match("child", "WebFetch", hooks.PhasePre); len(got) != 1 || got[0].Name != "gate" {
		t.Fatalf("child chain = %v; want the inherited addition only", got)
	}
}

// A resumed run fires what it added before it paused: its record's additions
// come back as an inheritance, not re-checked against its tools.
func TestRunHooks_AResumedRunKeepsItsAdditions(t *testing.T) {
	rec := runConfigRecord{Hooks: additionsRecord(hooks.Additions{Hooks: hooks.EventHooks{hooks.PhaseRunEnd: {{Inline: &hooks.Inline{Name: "audit", URL: "https://h.example"}}}}})}
	back, ok := decodeRunConfig(rec.marshal())
	if !ok || back.Hooks == nil {
		t.Fatalf("the record lost its additions: %s", rec.marshal())
	}
	h := newReviewHarness(t)
	ctx := h.srv.withRunHooks(hooks.WithAdditions(context.Background(), *back.Hooks), "", "writer", config.AgentDef{}, hooks.Additions{})
	if got := hooks.SetFrom(ctx).Match("writer", "", hooks.PhaseRunEnd); len(got) != 1 {
		t.Fatalf("resumed chain = %v", got)
	}
}
