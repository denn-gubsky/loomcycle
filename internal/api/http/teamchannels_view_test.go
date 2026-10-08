package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// get calls one of the team-channel routes as `as`, through the mux so the
// path is matched as a client's would be.
func (h *channelHarness) get(as func(context.Context) context.Context, path string) (int, map[string]any) {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r = r.WithContext(as(r.Context()))
	rr := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(strings.SplitN(path, "?", 2)[0], "/peek"):
		parts := strings.Split(strings.SplitN(path, "?", 2)[0], "/")
		r.SetPathValue("team", parts[3])
		r.SetPathValue("name", parts[5])
		h.srv.handleTeamChannelPeek(rr, r)
	default:
		r.SetPathValue("team", strings.Split(strings.SplitN(path, "?", 2)[0], "/")[3])
		h.srv.handleTeamChannels(rr, r)
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{"raw": rr.Body.String()}
	}
	return rr.Code, out
}

func acmeAdmin(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
}

func otherTenant(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: "globex", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
}

func channelNamed(t *testing.T, out map[string]any, name string) map[string]any {
	t.Helper()
	list, _ := out["channels"].([]any)
	for _, c := range list {
		if m, _ := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no channel %q in %v", name, out)
	return nil
}

// The gap's acceptance, end to end: after a walk publishes to the team's own
// channel, its operator can read the message — twice, since a peek moves no
// cursor — and the counts show it.
func TestTeamChannels_AnOperatorReadsWhatAWalkPublished(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_intake_1", "intake", intakeTeam(`{"scope":"user","max_messages":50}`))
	h.walk(acmeUser("alice"), "intake", "the first note")

	for i := 0; i < 2; i++ {
		code, out := h.get(acmeUser("alice"), "/v1/_teamdef/intake/channels/events/peek")
		msgs, _ := out["messages"].([]any)
		if code != http.StatusOK || len(msgs) != 1 {
			t.Fatalf("peek %d: HTTP %d %v, want the one message", i+1, code, out)
		}
		if raw, _ := json.Marshal(msgs[0]); !strings.Contains(string(raw), "the first note") {
			t.Errorf("peek %d: message = %s, want what the walk published", i+1, raw)
		}
		if out["scope"] != "user" || out["declared_in"] != "active" || out["name"] != "events" || out["team"] != "intake" {
			t.Errorf("peek %d: %v, want the channel named by team and local name, scope user, declared_in active", i+1, out)
		}
	}

	code, out := h.get(acmeUser("alice"), "/v1/_teamdef/intake/channels")
	if code != http.StatusOK {
		t.Fatalf("list: HTTP %d %v", code, out)
	}
	ch := channelNamed(t, out, "events")
	if ch["message_count"] != float64(1) || ch["scope"] != "user" || ch["max_messages"] != float64(50) ||
		ch["declared_in"] != "active" || ch["def_id"] != "tdf_intake_1" {
		t.Errorf("listed channel = %v, want its definition and one message", ch)
	}
	// The stored name is the runtime's own; a caller is never shown it.
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), store.TeamChannelPrefix) {
		t.Errorf("the list shows the stored name: %s", raw)
	}
}

// A held message is counted and not listed, as on any channel.
func TestTeamChannels_AHeldMessageIsCountedNotListed(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_held_1", "held", intakeTeam(`{"scope":"tenant","hold":true}`))
	h.walk(acmeUser("alice"), "held", "x")

	_, out := h.get(acmeUser("alice"), "/v1/_teamdef/held/channels")
	ch := channelNamed(t, out, "events")
	if ch["hold"] != true || ch["held_count"] != float64(1) || ch["message_count"] != float64(1) {
		t.Errorf("listed channel = %v, want hold with one held message", ch)
	}
	code, peek := h.get(acmeUser("alice"), "/v1/_teamdef/held/channels/events/peek")
	if msgs, _ := peek["messages"].([]any); code != http.StatusOK || len(msgs) != 0 {
		t.Errorf("peek of a held channel: HTTP %d %v, want no messages", code, peek)
	}
}

// Who may read, and what each refusal says.
func TestTeamChannels_AreReadOnlyByTheTeamsOwnTenant(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_intake_1", "intake", intakeTeam(`{"scope":"user"}`))
	h.walk(acmeUser("alice"), "intake", "alice's note")

	for name, tc := range map[string]struct {
		as       func(context.Context) context.Context
		path     string
		code     int
		wantCode string
	}{
		"another tenant, the list":                    {otherTenant, "/v1/_teamdef/intake/channels", 404, "team_not_found"},
		"another tenant, a peek":                      {otherTenant, "/v1/_teamdef/intake/channels/events/peek", 404, "team_not_found"},
		"another tenant naming this one":              {otherTenant, "/v1/_teamdef/intake/channels?tenant=acme", 404, "team_not_found"},
		"a team that does not exist":                  {acmeUser("alice"), "/v1/_teamdef/nosuch/channels", 404, "team_not_found"},
		"a channel the team does not declare":         {acmeUser("alice"), "/v1/_teamdef/intake/channels/nosuch/peek", 404, "team_channel_not_declared"},
		"another user's keyspace, as a tenant member": {acmeUser("bob"), "/v1/_teamdef/intake/channels/events/peek?user_id=alice", 404, "team_channel_not_declared"},
		"a malformed user id":                         {acmeAdmin, "/v1/_teamdef/intake/channels/events/peek?user_id=a%20b", 400, "user_id_required"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := h.get(tc.as, tc.path)
			if code != tc.code || out["code"] != tc.wantCode {
				t.Errorf("HTTP %d %v, want %d %s", code, out, tc.code, tc.wantCode)
			}
			if raw, _ := json.Marshal(out); strings.Contains(string(raw), "alice's note") {
				t.Errorf("a refused read returned the message: %s", raw)
			}
		})
	}

	// "not declared in operator yaml" is the wrong answer for a team's own
	// channel; the refusal names the team and the channel.
	_, out := h.get(acmeUser("alice"), "/v1/_teamdef/intake/channels/nosuch/peek")
	if msg, _ := out["error"].(string); msg != `team "intake" declares no channel of its own named "nosuch"` {
		t.Errorf("refusal = %q", msg)
	}
	// Another user's keyspace is refused in the very same words: the route
	// must not tell a caller which of the two it was.
	_, other := h.get(acmeUser("bob"), "/v1/_teamdef/intake/channels/events/peek?user_id=alice")
	if msg, _ := other["error"].(string); msg != `team "intake" declares no channel of its own named "events"` {
		t.Errorf("refusal for another user's keyspace = %q", msg)
	}

	// Bob reads his own keyspace, which is empty: the channel is one per user.
	code, bob := h.get(acmeUser("bob"), "/v1/_teamdef/intake/channels/events/peek")
	if msgs, _ := bob["messages"].([]any); code != http.StatusOK || len(msgs) != 0 {
		t.Errorf("bob's own peek: HTTP %d %v, want his own empty keyspace", code, bob)
	}
	// An admin may read a named user's.
	code, adm := h.get(acmeAdmin, "/v1/_teamdef/intake/channels/events/peek?user_id=alice")
	if msgs, _ := adm["messages"].([]any); code != http.StatusOK || len(msgs) != 1 {
		t.Errorf("an admin's peek of alice's keyspace: HTTP %d %v, want her message", code, adm)
	}
	// With no caller identity and no user named, a user-scoped channel cannot
	// be read: there is nobody's keyspace to read.
	code, open := h.get(func(c context.Context) context.Context { return c }, "/v1/_teamdef/intake/channels/events/peek?tenant=acme")
	if code != http.StatusBadRequest || open["code"] != "user_id_required" {
		t.Errorf("an open-mode peek with no user: HTTP %d %v, want 400 user_id_required", code, open)
	}
}

// The reserved name stays unaddressable through the channel surfaces: the new
// routes are the only way in, and they never take it.
func TestTeamChannels_TheStoredNameStaysUnaddressable(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_intake_1", "intake", intakeTeam(`{"scope":"tenant"}`))
	h.walk(acmeUser("alice"), "intake", "x")
	ctx := acmeUser("alice")(context.Background())
	for _, name := range []string{"_team/intake/events", "./events"} {
		_, err := h.srv.PeekChannel(ctx, connector.ChannelPeekRequest{Channel: name, Scope: "tenant"})
		if !errors.Is(err, connector.ErrChannelNotDeclared) {
			t.Errorf("PeekChannel(%q) = %v, want it still not declared", name, err)
		}
	}
	// And the local name is a map key, not a path: nothing but a declared
	// name is ever turned into a stored one.
	for _, local := range []string{"../intake/events", "_team/intake/events", "events/../events", ""} {
		_, err := h.srv.PeekTeamChannel(ctx, connector.TeamChannelPeekRequest{Team: "intake", Name: local})
		if !errors.Is(err, connector.ErrTeamChannelNotDeclared) {
			t.Errorf("PeekTeamChannel(name %q) = %v, want not declared", local, err)
		}
	}
}

// A channel's messages belong to the team, not to a version. One that only an
// older or a retired version declares is still listed and readable, and says
// which kind of version speaks for it.
func TestTeamChannels_DeclaredInSaysWhichVersionSpeaksForAChannel(t *testing.T) {
	h := newChannelHarness(t, nil)
	sc := h.seed("tdf_v1", "evolving", intakeTeam(`{"scope":"tenant","max_messages":5}`))
	h.walk(acmeUser("alice"), "evolving", "from v1")
	ctx := context.Background()

	// v2, active, declares a different channel and drops `events`.
	v2 := `{"entry":"form","local":{"channels":{"later":{"scope":"tenant"}}},
	  "states":[{"state":"form","handler":{"kind":"input","publish":{"channel":"./later"}}},{"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"form","to":"done","on":"success"}]}`
	row, err := h.st.TeamDefCreate(ctx, store.TeamDefRow{DefID: "tdf_v2", Name: "evolving", Version: 2, TenantID: "acme", ParentDefID: sc.DefID, Definition: json.RawMessage(v2)})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TeamDefSetActive(ctx, "acme", "evolving", row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatal(err)
	}

	_, out := h.get(acmeUser("alice"), "/v1/_teamdef/evolving/channels")
	if ch := channelNamed(t, out, "later"); ch["declared_in"] != "active" || ch["def_id"] != "tdf_v2" {
		t.Errorf("later = %v, want declared by the active version", ch)
	}
	if ch := channelNamed(t, out, "events"); ch["declared_in"] != "inactive" || ch["def_id"] != "tdf_v1" || ch["message_count"] != float64(1) {
		t.Errorf("events = %v, want declared by the inactive v1, with its message", ch)
	}

	if err := h.st.TeamDefSetRetired(ctx, "tdf_v1", true); err != nil {
		t.Fatal(err)
	}
	_, out = h.get(acmeUser("alice"), "/v1/_teamdef/evolving/channels")
	if ch := channelNamed(t, out, "events"); ch["declared_in"] != "retired" {
		t.Errorf("events = %v, want declared_in retired", ch)
	}
	code, peek := h.get(acmeUser("alice"), "/v1/_teamdef/evolving/channels/events/peek")
	if msgs, _ := peek["messages"].([]any); code != http.StatusOK || len(msgs) != 1 || peek["declared_in"] != "retired" {
		t.Errorf("peek of a channel only a retired version declares: HTTP %d %v, want its message", code, peek)
	}
}

// A team with no channels of its own has an empty list, not an error.
func TestTeamChannels_ATeamWithNoneListsNone(t *testing.T) {
	h := newChannelHarness(t, nil)
	h.seed("tdf_plain_1", "plain", `{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"reviewer"}},{"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"done","on":"success"}]}`)
	code, out := h.get(acmeUser("alice"), "/v1/_teamdef/plain/channels")
	if list, isList := out["channels"].([]any); code != http.StatusOK || !isList || len(list) != 0 {
		t.Errorf("HTTP %d %v, want an empty channels list", code, out)
	}
}

// The routes are gated like the team itself, and are GETs: there is no write.
func TestTeamChannels_RoutesAreGatedLikeTheTeam(t *testing.T) {
	for _, path := range []string{"/v1/_teamdef/intake/channels", "/v1/_teamdef/intake/channels/events/peek"} {
		if got := requiredScopeFor(http.MethodGet, path); got != auth.ScopeTenant {
			t.Errorf("requiredScopeFor(GET %s) = %q, want %q", path, got, auth.ScopeTenant)
		}
	}
	// A path that only resembles one stays where it was.
	for _, path := range []string{"/v1/_teamdef/channels", "/v1/_teamdef/intake/channelsx", "/v1/_teamdef/intake/other"} {
		if isTeamChannelsPath(path) {
			t.Errorf("%s is treated as a team-channel route", path)
		}
	}
}
