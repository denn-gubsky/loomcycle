package teamgraph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// localChannelsJSON is a team whose Starter reads its own `events` channel and
// publishes each run's result to its own `verdicts` channel.
const localChannelsJSON = `{
  "entry": "wave",
  "local": {"channels": {
    "events":   {"scope": "tenant", "default_ttl": 3600},
    "verdicts": {"scope": "user", "max_messages": 100}
  }},
  "states": [
    {"state": "wave", "handler": {"kind": "starter", "source": {"channel": "./events"},
      "fanout": {"agent": "reviewer", "per": "message", "max": 4}, "sink": {"channel": "./verdicts"}}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [{"from": "wave", "to": "done", "on": "success"}]
}`

func TestValidate_AcceptsDeclaredLocalChannels(t *testing.T) {
	d := mustParse(t, localChannelsJSON)
	if err := Validate(d); err != nil {
		t.Fatalf("a team using its own declared channels must validate without a channels ACL: %v", err)
	}
	if got := d.LocalChannelNames(); len(got) != 2 || got[0] != "events" || got[1] != "verdicts" {
		t.Errorf("LocalChannelNames = %v", got)
	}
}

// Every channel-bearing field is held to the rule, not only the source.
func TestValidate_RefusesUndeclaredLocalChannelInEveryField(t *testing.T) {
	for field, handler := range map[string]string{
		"source":  `{"kind":"starter","source":{"channel":"./ghost"},"fanout":{"agent":"a","per":"message","max":4}}`,
		"sink":    `{"kind":"starter","source":{"channel":"./events"},"fanout":{"agent":"a","per":"message","max":4},"sink":{"channel":"./ghost"}}`,
		"channel": `{"kind":"channel","channel":"./ghost"}`,
		"publish": `{"kind":"input","publish":{"channel":"./ghost"}}`,
	} {
		def := `{"entry":"s","local":{"channels":{"events":{"scope":"tenant"}}},
		  "states":[{"state":"s","handler":` + handler + `},{"state":"done","handler":{"kind":"terminal"}}],
		  "transitions":[{"from":"s","to":"done","on":"success"}]}`
		d, err := Parse([]byte(def))
		if err != nil {
			t.Fatalf("%s: parse: %v", field, err)
		}
		err = Validate(d)
		if err == nil {
			t.Errorf("%s: \"./ghost\" is not declared and must be refused", field)
			continue
		}
		for _, want := range []string{`"./ghost"`, "local.channels", "events"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal %q should name %s", field, err, want)
			}
		}
	}
}

// The walk re-checks a stored body before it starts; CheckLocalRefs is that
// check, so it must cover channels too.
func TestCheckLocalRefs_RefusesUndeclaredLocalChannel(t *testing.T) {
	d := mustParse(t, strings.Replace(localChannelsJSON, `"./verdicts"`, `"./nowhere"`, 1))
	if err := CheckLocalRefs(d); err == nil || !strings.Contains(err.Error(), `"./nowhere"`) {
		t.Fatalf("want a refusal naming \"./nowhere\", got %v", err)
	}
}

// A bare name keeps meaning a declared global channel, even when a local
// channel of that name exists.
func TestValidate_BareChannelNameIsNotALocalReference(t *testing.T) {
	d := mustParse(t, strings.Replace(localChannelsJSON, `"./events"`, `"events"`, 1))
	if err := Validate(d); err != nil {
		t.Fatalf("a bare channel name needs no local declaration: %v", err)
	}
}

func TestValidate_RefusesBadLocalChannelName(t *testing.T) {
	for _, name := range []string{"a/b", "a.b", "a b", "", strings.Repeat("x", 65), "./a", "_team/x"} {
		def := `{"entry":"done","local":{"channels":{` + quote(name) + `:{"scope":"tenant"}}},
		  "states":[{"state":"done","handler":{"kind":"terminal"}}],"transitions":[]}`
		if err := Validate(mustParse(t, def)); err == nil {
			t.Errorf("local channel name %q must be refused", name)
		}
	}
}

// The team holds its own channels; an ACL entry for one would be checked
// against the author's grants, which cannot contain it.
func TestValidate_RefusesLocalChannelInTheTeamACL(t *testing.T) {
	for _, side := range []string{"publish", "subscribe"} {
		def := strings.Replace(localChannelsJSON, `"entry": "wave",`,
			`"entry": "wave", "channels": {"`+side+`": ["./events"]},`, 1)
		err := Validate(mustParse(t, def))
		if err == nil || !strings.Contains(err.Error(), "channels."+side) {
			t.Errorf("%s: a \"./\" ACL entry must be refused naming the side, got %v", side, err)
		}
	}
}

func TestValidate_CapsTheNumberOfLocalChannels(t *testing.T) {
	build := func(n int) Definition {
		d := mustParse(t, localChannelsJSON)
		for i := 2; i < n; i++ {
			d.Local.Channels[fmt.Sprintf("c%d", i)] = json.RawMessage(`{"scope":"tenant"}`)
		}
		return d
	}
	if err := Validate(build(MaxLocalChannels)); err != nil {
		t.Fatalf("%d local channels is the limit and must be accepted: %v", MaxLocalChannels, err)
	}
	err := Validate(build(MaxLocalChannels + 1))
	if err == nil || !strings.Contains(err.Error(), "more than the maximum 64") {
		t.Fatalf("%d local channels must be refused naming the limit, got %v", MaxLocalChannels+1, err)
	}
}

func TestParse_RefusesLocalChannelBodyThatIsNotAnObject(t *testing.T) {
	for _, body := range []string{`"tenant"`, `[]`, `null`, `3`} {
		if _, err := Parse([]byte(`{"entry":"s","local":{"channels":{"a":` + body + `}}}`)); err == nil {
			t.Errorf("a local channel body of %s must be refused", body)
		}
	}
}

// A team with local agents and no channels keeps the hash it had before
// channels existed; an empty channels kind is no content.
func TestSign_EmptyLocalChannelsKeepTheRecordedHash(t *testing.T) {
	d := mustParse(t, sdlcJSON)
	d.Local = &Local{Channels: map[string]json.RawMessage{}}
	if got := Sign("sdlc", d); got != recordedNoLocalHash {
		t.Errorf("an empty local.channels changed the hash to %s, want %s", got, recordedNoLocalHash)
	}
	withAgents := mustParse(t, localJSON)
	before := Sign("t", withAgents)
	withAgents.Local.Channels = map[string]json.RawMessage{}
	if got := Sign("t", withAgents); got != before {
		t.Errorf("an empty local.channels changed a local-agents team's hash: %s vs %s", got, before)
	}
}

// recordedLocalChannelsHash pins the hash of localChannelsJSON: the bytes a
// local channel contributes are part of every recorded hash from here on.
const recordedLocalChannelsHash = "sha256:e7340845390e12c52ac2da974ed4c93bb1ae0077ffce36780d2ac956ec86b968"

func TestSign_LocalChannelsAreContent(t *testing.T) {
	d := mustParse(t, localChannelsJSON)
	base := Sign("t", d)
	if base != recordedLocalChannelsHash {
		t.Errorf("hash of the local-channels fixture = %s, recorded %s", base, recordedLocalChannelsHash)
	}
	if Sign("t", mustParse(t, strings.Replace(localChannelsJSON, `"default_ttl": 3600`, `"default_ttl": 60`, 1))) == base {
		t.Error("changing a local channel's ttl must change the team's content hash")
	}
	// Layout of a body is not content.
	relaid := strings.Replace(localChannelsJSON, `{"scope": "tenant", "default_ttl": 3600}`, `{ "default_ttl":3600,"scope":"tenant" }`, 1)
	if Sign("t", mustParse(t, relaid)) != base {
		t.Error("key order / whitespace inside a local channel body changed the hash")
	}
}

// A team declaring all three kinds hashes each: changing any one of them
// changes the team's content hash.
func TestSign_EveryLocalKindIsContent(t *testing.T) {
	const all = `{"entry":"s","local":{
	  "agents":{"a":{"tier":"low","skills":["./sk"]}},
	  "skills":{"sk":{"body":"Do it."}},
	  "channels":{"c":{"scope":"tenant"}}},
	  "states":[{"state":"s","handler":{"kind":"terminal"}}],"transitions":[]}`
	base := Sign("t", mustParse(t, all))
	for kind, edit := range map[string][2]string{
		"agents":   {`"tier":"low"`, `"tier":"high"`},
		"skills":   {`"Do it."`, `"Do it twice."`},
		"channels": {`{"scope":"tenant"}`, `{"scope":"user"}`},
	} {
		if Sign("t", mustParse(t, strings.Replace(all, edit[0], edit[1], 1))) == base {
			t.Errorf("changing local.%s did not change the hash", kind)
		}
	}
}
