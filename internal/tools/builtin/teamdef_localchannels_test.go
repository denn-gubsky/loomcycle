package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// localChannelTeam is a team whose Starter reads its own `events` and sinks to
// its own `verdicts`; `events` has the given body.
func localChannelTeam(eventsBody string) string {
	return `{"entry":"wave","local":{"channels":{"events":` + eventsBody + `,"verdicts":{"scope":"user"}}},
	  "states":[{"state":"wave","handler":{"kind":"starter","source":{"channel":"./events"},
	    "fanout":{"agent":"reviewer","per":"message","max":4},"sink":{"channel":"./verdicts"}}},
	    {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"wave","to":"done","on":"success"}]}`
}

// The team holds its own channels: no ACL entry, no declaration elsewhere, and
// no channel grant on the author.
func TestTeamDefCreate_AcceptsOwnChannelsWithoutAnACL(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	createTeam(t, tool, ctx, "triage", localChannelTeam(`{"scope":"tenant","default_ttl":3600,"max_messages":50,"hold":true,"description":"incoming"}`))
}

// The preflight asks the declared catalog for every channel a definition
// names; the team's own are not in it and must not be asked for.
func TestTeamDefCreate_PreflightSkipsOwnChannels(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	tool.ChannelCatalog = func(context.Context) map[string]tools.ChannelDef { return map[string]tools.ChannelDef{} }
	createTeam(t, tool, ctx, "triage", localChannelTeam(`{"scope":"tenant"}`))
}

func TestTeamDefCreate_RefusesBadLocalChannelBody(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	for body, want := range map[string]string{
		`{"scope":"global"}`:                                    "scope must be tenant",
		`{"scope":"agent"}`:                                     "scope must be tenant",
		`{}`:                                                    "scope must be tenant", // the default is global
		`{"scope":"bogus"}`:                                     "scope must be one of",
		`{"scope":"user","semantic":"fanout"}`:                  "semantic must be one of",
		`{"scope":"user","default_ttl":-1}`:                     ">= 0",
		`{"scope":"user","publisher":"system"}`:                 "operator's config",
		`{"scope":"user","period":"1m"}`:                        "operator's config",
		`{"scope":"user","hooks":{"channel_publish":["gate"]}}`: "cannot carry hooks",
		`{"scope":"user","name":"events"}`:                      "remove `name`",
		`{"scope":"user","colour":"red"}`:                       "not a channel definition",
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"triage","overlay":`+localChannelTeam(body)+`}`))
		wantRefused(t, res, body, `local.channels["events"]`, want)
	}
}

func TestTeamDefCreate_RefusesReservedChannelNameAnywhere(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	admin := authoringCtx([]string{"_team/*", "x"}, []string{"_team/*"})
	// A state naming the stored spelling of another team's channel.
	ref := strings.Replace(localChannelTeam(`{"scope":"tenant"}`), `"./verdicts"`, `"_team/other/verdicts"`, 1)
	res, _ := tool.Execute(admin, json.RawMessage(`{"op":"create","name":"triage","overlay":`+ref+`}`))
	wantRefused(t, res, "a sink under _team/", "reserved name")
	// An ACL entry for it, even by an author who holds it.
	acl := strings.Replace(localChannelTeam(`{"scope":"tenant"}`), `"entry":"wave",`, `"entry":"wave","channels":{"publish":["_team/other/x"]},`, 1)
	res, _ = tool.Execute(admin, json.RawMessage(`{"op":"create","name":"triage","overlay":`+acl+`}`))
	wantRefused(t, res, "an ACL entry under _team/", "reserved name")
}

// A local channel is stored under the team's name, which must split one way.
func TestTeamDefCreate_RefusesLocalChannelsOnALegacyTeamName(t *testing.T) {
	def, err := teamgraph.Parse([]byte(localChannelTeam(`{"scope":"tenant"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLocalChannels("team/with/slashes", def); err == nil || !strings.Contains(err.Error(), "one segment") {
		t.Fatalf("want the one-segment refusal, got %v", err)
	}
}

// A team's own agent is granted one of the team's channels by listing
// "./<name>" in its own ACL; one the team does not declare is refused there.
func TestTeamDefCreate_LocalAgentChannelGrantMustNameADeclaredChannel(t *testing.T) {
	tool, ctx := localTeamFixture(t)
	team := func(acl string) string {
		return `{"entry":"review","local":{"channels":{"events":{"scope":"tenant"}},
		  "agents":{"reviewer":{"tier":"middle","tools":["Read"],"channels":` + acl + `}}},
		  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},
		            {"state":"done","handler":{"kind":"terminal"}}],
		  "transitions":[{"from":"review","to":"done","on":"success"}]}`
	}
	if res := teamOp(t, tool, ctx, "create", "sdlc", team(`{"publish":["./events"],"subscribe":["./events"]}`)); res.IsError {
		t.Fatalf("a grant on a declared channel must be accepted: %s", res.Text)
	}
	for acl, want := range map[string]string{
		`{"subscribe":["./ghost"]}`:         `"./ghost"`,
		`{"publish":["./*"]}`:               `"./*"`,
		`{"publish":["_team/sdlc/events"]}`: "reserved name",
	} {
		res := teamOp(t, tool, ctx, "create", "sdlc2", team(acl))
		wantRefused(t, res, acl, `local.agents["reviewer"]`, want)
	}
}

// A fork that sends local.channels replaces that kind, and one that does not
// keeps the parent's.
func TestTeamDefFork_LocalChannelsReplaceWholesale(t *testing.T) {
	tool, ctx, cleanup := teamDefFixture(t)
	defer cleanup()
	createTeam(t, tool, ctx, "triage", localChannelTeam(`{"scope":"tenant"}`))
	channelsOf := func(res string) map[string]any {
		defID, _ := decodeResult(t, res)["def_id"].(string)
		got, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
		def, _ := decodeResult(t, got.Text)["definition"].(map[string]any)
		local, _ := def["local"].(map[string]any)
		chans, _ := local["channels"].(map[string]any)
		return chans
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"triage","overlay":{"max_iterations":5}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := channelsOf(res.Text); len(got) != 2 {
		t.Errorf("a fork without local.channels must keep the parent's two, got %v", got)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"triage","overlay":{"local":{"channels":{
	  "events":{"scope":"user"},"verdicts":{"scope":"user"},"extra":{"scope":"tenant"}}}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := channelsOf(res.Text); len(got) != 3 || got["extra"] == nil {
		t.Errorf("a fork that sends local.channels must replace them, got %v", got)
	}
}

func TestValidateTeamDefBody_RefusesBadLocalChannel(t *testing.T) {
	if err := ValidateTeamDefBody(json.RawMessage(localChannelTeam(`{"scope":"tenant"}`))); err != nil {
		t.Fatalf("a good body must restore: %v", err)
	}
	err := ValidateTeamDefBody(json.RawMessage(localChannelTeam(`{"scope":"global"}`)))
	if err == nil || !strings.Contains(err.Error(), `local.channels["events"]`) {
		t.Fatalf("a restored global local channel must be refused, got %v", err)
	}
}

// verify reports an undeclared "./x" and is not fooled into asking for an ACL
// entry or a declaration for the team's own channels.
func TestTeamDefVerify_OwnChannelsNeedNoACLOrDeclaration(t *testing.T) {
	tool, _, cleanup := teamDefFixture(t)
	defer cleanup()
	def, err := teamgraph.Parse([]byte(localChannelTeam(`{"scope":"tenant"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if issues := tool.sweepReferences(t.Context(), def); len(issues) != 0 {
		t.Errorf("a team's own channels raised issues: %v", issues)
	}
	def.Local.Channels = nil
	issues := tool.sweepReferences(t.Context(), def)
	if len(issues) != 2 || issues[0]["kind"] != "local_channel_missing" {
		t.Errorf("want two local_channel_missing issues, got %v", issues)
	}
}

// Each kind of `local` needs its own case in applyTeamOverlay, or a fork that
// sends it is accepted and drops it. This walks the struct, so a kind added
// later is held to it too.
func TestApplyTeamOverlay_CarriesEveryLocalKind(t *testing.T) {
	typ := reflect.TypeOf(teamgraph.Local{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Map {
			t.Fatalf("Local.%s: kind %s has no test value; extend this test", f.Name, f.Type.Kind())
		}
		ov := teamgraph.Definition{Local: &teamgraph.Local{}}
		want := reflect.MakeMap(f.Type)
		want.SetMapIndex(reflect.ValueOf("x"), reflect.Zero(f.Type.Elem()))
		reflect.ValueOf(ov.Local).Elem().Field(i).Set(want)
		var base teamgraph.Definition
		applyTeamOverlay(&base, ov)
		if base.Local == nil || reflect.ValueOf(base.Local).Elem().Field(i).Len() != 1 {
			t.Errorf("a fork drops local.%s: applyTeamOverlay has no case for it", f.Name)
		}
	}
}
