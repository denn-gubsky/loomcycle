package teamruntest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const (
	runSolo    = `{"op":"run","name":"solo"}`
	cancelWalk = `{"op":"cancel","run_ids":["r_none"]}`
	pollWalks  = `{"op":"poll"}`
)

// memberSurfaces are the surfaces a tenant member's token reaches the tool on
// whatever scopes it holds. The gRPC TeamDef RPC is not one: it has no member
// path and needs substrate:tenant outright.
var memberSurfaces = surfaces[:0:0]

func init() {
	for _, s := range surfaces {
		if s.name != "grpc" {
			memberSurfaces = append(memberSurfaces, s)
		}
	}
}

// refusedFor fails unless o is the per-op scope refusal naming op and scope.
func refusedFor(t *testing.T, o outcome, op, scope string) {
	t.Helper()
	if !o.forbidden || o.atRoute {
		t.Fatalf("op=%s: forbidden=%v atRoute=%v toolError=%v %q; want the per-op refusal", op, o.forbidden, o.atRoute, o.toolError, o.text)
	}
	if !strings.Contains(o.text, "op="+op) || !strings.Contains(o.text, scope) {
		t.Errorf("op=%s refusal %q must name the op and %s", op, o.text, scope)
	}
}

// A member whose token was not granted runs:create cannot start a team walk —
// which spawns runs and spends tokens — nor stop one, on the surfaces that
// admit it to the tool for authoring. Nothing is admitted on the refusal: no
// run row is opened for the walk and no member reaches the model.
func TestTeamRun_AMemberWithoutRunsCreateCannotStartOrStopAWalk(t *testing.T) {
	for _, s := range memberSurfaces {
		for name, scopes := range map[string][]string{
			"runs:read":    {auth.ScopeRunsRead},
			"channel:read": {auth.ScopeChannelRead},
			"no scopes":    nil,
		} {
			t.Run(s.name+"/"+name, func(t *testing.T) {
				e := newEnv(t, false)
				bob := e.mint("bob", scopes...)
				for _, input := range []string{
					runSolo,
					`{"op":"run","name":"solo","mode":"detach"}`,
					// The tool reads its input's keys case-insensitively and takes
					// the last of a repeated key, so the check must too.
					`{"OP":"run","name":"solo"}`,
					`{"op":"list","name":"solo","op":"run"}`,
				} {
					refusedFor(t, s.call(e, bob, input), "run", auth.ScopeRunsCreate)
				}
				refusedFor(t, s.call(e, bob, cancelWalk), "cancel", auth.ScopeRunsCreate)
				if n, rows := e.prov.count(), e.walkRuns(); n != 0 || rows != 0 {
					t.Errorf("after the refusals: %d model calls and %d walk run rows, want none", n, rows)
				}
			})
		}
	}
}

// HTTP says it the way its route gate does: 403, the scope in
// WWW-Authenticate, and a body naming the op.
func TestTeamRun_HTTPRefusalNamesTheScope(t *testing.T) {
	e := newEnv(t, false)
	resp, raw := e.do("/v1/_teamdef", e.mint("bob", auth.ScopeRunsRead), runSolo, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer scope="runs:create"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	var body struct{ Code, Error string }
	if err := json.Unmarshal(raw, &body); err != nil || body.Code != "insufficient_scope" ||
		body.Error != "insufficient scope: TeamDef op=run requires runs:create" {
		t.Errorf("body = %s (%v)", raw, err)
	}
}

// What a member without runs:create keeps: reading walks (with runs:read) and
// every definition op. "Allowed" is that the tool, not a scope check, answers.
func TestTeamRun_AReadOnlyMemberKeepsReadingAndAuthoring(t *testing.T) {
	for _, s := range memberSurfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, false)
			bob := e.mint("bob", auth.ScopeRunsRead)
			for _, input := range []string{`{"op":"get","def_id":"tdf_acme_solo"}`, `{"op":"list","name":"solo"}`} {
				if o := s.call(e, bob, input); o.forbidden || o.toolError || !strings.Contains(o.text, "tdf_acme_solo") {
					t.Errorf("%s: forbidden=%v toolError=%v %q; want the definition", input, o.forbidden, o.toolError, o.text)
				}
			}
			// Off a run there is never a walk to poll, so the tool's own answer
			// (HTTP) or the transport's (MCP) is all "allowed" can mean here.
			if o := s.call(e, bob, pollWalks); o.forbidden || !o.toolError {
				t.Errorf("poll: forbidden=%v toolError=%v %q; want the tool's answer", o.forbidden, o.toolError, o.text)
			}
			if !e.mcpLists(bob) {
				t.Error("the teamdef tool must stay offered to a member: its authoring ops are a member's")
			}
		})
	}
}

// A member with runs:create and nothing else starts a walk and reaches cancel,
// and is refused poll, which reads runs.
func TestTeamRun_ACreateOnlyMemberStartsAWalkAndCannotPoll(t *testing.T) {
	for _, s := range memberSurfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, false)
			bob := e.mint("bob", auth.ScopeRunsCreate)
			if o := s.call(e, bob, runSolo); o.forbidden || o.toolError || !strings.Contains(o.text, `"status":"completed"`) {
				t.Fatalf("run: forbidden=%v toolError=%v %q; want a completed walk", o.forbidden, o.toolError, o.text)
			}
			if n, rows := e.prov.count(), e.walkRuns(); n != 1 || rows != 1 {
				t.Errorf("%d model calls and %d walk run rows, want one of each", n, rows)
			}
			if o := s.call(e, bob, cancelWalk); o.forbidden || !o.toolError {
				t.Errorf("cancel: forbidden=%v toolError=%v %q; want the tool's answer", o.forbidden, o.toolError, o.text)
			}
			refusedFor(t, s.call(e, bob, pollWalks), "poll", auth.ScopeRunsRead)
		})
	}
}

// Authoring needs no run scope: a member holding neither creates and verifies
// a team as before.
func TestTeamRun_AMemberWithNoRunScopeStillAuthors(t *testing.T) {
	for _, s := range memberSurfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, false)
			bob := e.mint("bob", auth.ScopeChannelRead)
			created := s.call(e, bob, `{"op":"create","name":"mine","overlay":`+soloTeam+`}`)
			if created.forbidden || created.toolError {
				t.Fatalf("create: forbidden=%v %q", created.forbidden, created.text)
			}
			var row struct {
				DefID string `json:"def_id"`
			}
			if err := json.Unmarshal([]byte(created.text), &row); err != nil || row.DefID == "" {
				t.Fatalf("create answered %q", created.text)
			}
			for _, input := range []string{
				`{"op":"verify","name":"mine","overlay":` + soloTeam + `}`,
				`{"op":"render_diagram","def_id":"` + row.DefID + `"}`,
				`{"op":"promote","def_id":"` + row.DefID + `"}`,
				`{"op":"retire","def_id":"` + row.DefID + `","retired":true}`,
			} {
				if o := s.call(e, bob, input); o.forbidden || o.toolError {
					t.Errorf("%s: forbidden=%v toolError=%v %q", input, o.forbidden, o.toolError, o.text)
				}
			}
		})
	}
}

// The callers the check must not touch start a walk on every surface, as
// before: a tenant operator, an admin, the single shared token, and nobody at
// all when no authentication is configured.
func TestTeamRun_OperatorsAreUnchanged(t *testing.T) {
	for name, caller := range map[string]struct {
		open   bool
		bearer func(e *env) string
	}{
		"substrate:tenant": {bearer: func(e *env) string { return e.mint("op", auth.ScopeTenant) }},
		"substrate:admin":  {bearer: func(e *env) string { return e.mint("root", auth.ScopeAdmin) }},
		"the legacy token": {bearer: func(*env) string { return legacyToken }},
		"open mode":        {open: true, bearer: func(*env) string { return "" }},
	} {
		for _, s := range surfaces {
			t.Run(name+"/"+s.name, func(t *testing.T) {
				e := newEnv(t, caller.open)
				bearer := caller.bearer(e)
				if o := s.call(e, bearer, runSolo); o.forbidden || o.toolError || !strings.Contains(o.text, `"status":"completed"`) {
					t.Fatalf("run: forbidden=%v toolError=%v %q; want a completed walk", o.forbidden, o.toolError, o.text)
				}
				for _, input := range []string{cancelWalk, pollWalks} {
					if o := s.call(e, bearer, input); o.forbidden || !o.toolError {
						t.Errorf("%s: forbidden=%v toolError=%v %q; want the tool's answer", input, o.forbidden, o.toolError, o.text)
					}
				}
			})
		}
	}
}

// The gRPC TeamDef RPC never had the gap: it needs substrate:tenant, which
// carries both run scopes, and has no member path. Pinned so that opening the
// RPC to members does not open op=run with it.
func TestTeamRun_GRPCRefusesEveryMemberAtTheRPC(t *testing.T) {
	for name, scopes := range map[string][]string{
		"runs:read":              {auth.ScopeRunsRead},
		"runs:create":            {auth.ScopeRunsCreate},
		"both run scopes":        {auth.ScopeRunsCreate, auth.ScopeRunsRead},
		"an isolated user":       {auth.ScopeUser},
		"channel scopes":         {auth.ScopeChannelRead, auth.ScopeChannelPublish},
		"no scopes on the token": nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, false)
			bob := e.mint("bob", scopes...)
			for _, input := range []string{runSolo, cancelWalk, pollWalks, `{"op":"get","name":"solo"}`} {
				o := e.teamGRPC(bob, input)
				if !o.forbidden || !o.atRoute {
					t.Errorf("%s: forbidden=%v atRoute=%v %q; want PermissionDenied for substrate:tenant", input, o.forbidden, o.atRoute, o.text)
				}
			}
			if n, rows := e.prov.count(), e.walkRuns(); n != 0 || rows != 0 {
				t.Errorf("%d model calls and %d walk run rows, want none", n, rows)
			}
		})
	}
}

// An isolated user reaches the tool on no surface, before and after: the HTTP
// route and the RPC refuse the token, and its MCP session is confined to its
// self-service tool. Its scope implies runs:create, so this is the check that
// the new one only narrows.
func TestTeamRun_AnIsolatedUserStaysOutside(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			e := newEnv(t, false)
			eve := e.mint("eve", auth.ScopeUser)
			for _, input := range []string{runSolo, `{"op":"get","name":"solo"}`} {
				o := s.call(e, eve, input)
				if !o.forbidden || strings.Contains(o.text, "op=") {
					t.Errorf("%s: forbidden=%v %q; want the surface's own refusal of the token", input, o.forbidden, o.text)
				}
			}
			if n, rows := e.prov.count(), e.walkRuns(); n != 0 || rows != 0 {
				t.Errorf("%d model calls and %d walk run rows, want none", n, rows)
			}
		})
	}
}

// An agent granted the tool is governed by its own grants, not by the scopes
// of the token that started its run. A run started by a member holding only
// runs:create carries that principal, and its agent still polls the walk it
// started: the scope check belongs to the operator surfaces alone.
func TestTeamRun_AnAgentInsideARunIsNotHeldToItsStartersScopes(t *testing.T) {
	e := newEnv(t, false)
	e.prov.leadScript = [][]providers.Event{
		callsTeamDef("tu_1", `{"op":"run","name":"solo","mode":"poll"}`),
		says("started"),
		callsTeamDef("tu_2", `{"op":"poll"}`),
		says("all done"),
	}
	bob := e.mint("bob", auth.ScopeRunsCreate) // no runs:read
	resp, raw := e.do("/v1/runs", bob,
		`{"agent":"lead","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/runs = %d: %s", resp.StatusCode, raw)
	}
	results := e.prov.toolResults()
	if len(results) != 2 {
		t.Fatalf("the lead saw %d tool results, want the run's and the poll's:\n%s", len(results), raw)
	}
	if !strings.Contains(results[0], `"run_id"`) {
		t.Errorf("TeamDef run inside the run answered %q", results[0])
	}
	var polled struct {
		Walks []map[string]any `json:"walks"`
	}
	if err := json.Unmarshal([]byte(results[1]), &polled); err != nil || len(polled.Walks) != 1 || polled.Walks[0]["status"] != "completed" {
		t.Errorf("TeamDef poll inside the run answered %q (%v), want the completed walk", results[1], err)
	}
}
