package teamgraph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// localWebhooksJSON is a team whose own `events` channel is fed by its own
// `github` webhook, and whose entry Starter reads it.
const localWebhooksJSON = `{
  "entry": "wave",
  "local": {
    "channels": {"events": {"scope": "tenant"}},
    "webhooks": {"github": {"channel": "./events", "auth": {"kind": "hmac", "signing_secret_env": "LOOMCYCLE_GH_SECRET"}}}
  },
  "states": [
    {"state": "wave", "handler": {"kind": "starter", "source": {"channel": "./events"},
      "fanout": {"agent": "reviewer", "per": "message", "max": 4}}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [{"from": "wave", "to": "done", "on": "success"}]
}`

// withWebhook is localWebhooksJSON with its one webhook's body replaced.
func withWebhook(body string) string {
	return strings.Replace(localWebhooksJSON,
		`{"channel": "./events", "auth": {"kind": "hmac", "signing_secret_env": "LOOMCYCLE_GH_SECRET"}}`, body, 1)
}

func TestValidate_AcceptsADeclaredLocalWebhook(t *testing.T) {
	d := mustParse(t, localWebhooksJSON)
	if err := Validate(d); err != nil {
		t.Fatalf("a webhook publishing into the team's own channel must validate: %v", err)
	}
	body, ok := d.LocalWebhook("github")
	if !ok {
		t.Fatal("LocalWebhook(github) is not declared")
	}
	// Held canonically: keys sorted, no whitespace.
	if got := string(body); got != `{"auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_GH_SECRET"},"channel":"./events"}` {
		t.Errorf("body held as %s, want canonical JSON", got)
	}
	if names := d.LocalWebhookNames(); len(names) != 1 || names[0] != "github" {
		t.Errorf("LocalWebhookNames = %v", names)
	}
}

func TestValidate_RefusesALocalWebhookPublishingOutsideTheTeam(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no channel":        {`{"auth": {}}`, `must name one of the team's own channels`},
		"a global channel":  {`{"channel": "events"}`, `must name one of the team's own channels`},
		"a reserved name":   {`{"channel": "_team/x/events"}`, `must name one of the team's own channels`},
		"not a string":      {`{"channel": 7}`, `must name one of the team's own channels`},
		"an undeclared one": {`{"channel": "./ghost"}`, `"./ghost" names a channel the team does not declare under local.channels (declared: events)`},
	} {
		err := Validate(mustParse(t, withWebhook(tc.body)))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `local.webhooks["github"]`) {
			t.Errorf("%s: want a refusal naming the webhook and %q, got %v", name, tc.want, err)
		}
		// The walk's own re-check refuses it too.
		if err := CheckLocalRefs(mustParse(t, withWebhook(tc.body))); err == nil {
			t.Errorf("%s: CheckLocalRefs must refuse it", name)
		}
	}
	for _, body := range []string{`"./events"`, `[]`, `null`, `7`} {
		if _, err := Parse([]byte(withWebhook(body))); err == nil {
			t.Errorf("a local webhook body of %s must be refused", body)
		}
	}
}

func TestValidate_RefusesBadLocalWebhookName(t *testing.T) {
	for _, name := range []string{"a/b", "a.b", "a b", "", strings.Repeat("x", 65), "./a"} {
		def := strings.Replace(localWebhooksJSON, `"github":`, quote(name)+`:`, 1)
		if err := Validate(mustParse(t, def)); err == nil {
			t.Errorf("local webhook name %q must be refused", name)
		}
	}
}

func TestValidate_CapsTheNumberOfLocalWebhooks(t *testing.T) {
	build := func(n int) Definition {
		d := mustParse(t, localWebhooksJSON)
		for i := 1; i < n; i++ {
			d.Local.Webhooks[fmt.Sprintf("w%d", i)] = json.RawMessage(`{"channel":"./events"}`)
		}
		return d
	}
	if err := Validate(build(MaxLocalWebhooks)); err != nil {
		t.Fatalf("%d local webhooks is the limit and must be accepted: %v", MaxLocalWebhooks, err)
	}
	err := Validate(build(MaxLocalWebhooks + 1))
	if err == nil || !strings.Contains(err.Error(), "more than the maximum 16") {
		t.Fatalf("%d local webhooks must be refused naming the limit, got %v", MaxLocalWebhooks+1, err)
	}
}

// A team with local agents, channels or schedules and no webhooks keeps the
// hash it had before webhooks existed; an empty webhooks kind is no content.
func TestSign_EmptyLocalWebhooksKeepTheRecordedHash(t *testing.T) {
	d := mustParse(t, sdlcJSON)
	d.Local = &Local{Webhooks: map[string]json.RawMessage{}}
	if got := Sign("sdlc", d); got != recordedNoLocalHash {
		t.Errorf("an empty local.webhooks changed the hash to %s, want %s", got, recordedNoLocalHash)
	}
	for name, tc := range map[string]struct{ raw, want string }{
		"channels":  {localChannelsJSON, recordedLocalChannelsHash},
		"agents":    {localJSON, recordedLocalAgentsHash},
		"schedules": {localSchedulesJSON, recordedLocalSchedulesHash},
	} {
		d := mustParse(t, tc.raw)
		d.Local.Webhooks = map[string]json.RawMessage{}
		if got := Sign("t", d); got != tc.want {
			t.Errorf("an empty local.webhooks changed a local-%s team's hash to %s, want %s", name, got, tc.want)
		}
	}
}

// recordedLocalWebhooksHash pins the hash of localWebhooksJSON: the bytes a
// local webhook contributes are part of every recorded hash from here on.
const recordedLocalWebhooksHash = "sha256:df2cece502a73f06b388c8ee7390c62ceb6b242f3a3ba257d9efc344a1037f53"

func TestSign_LocalWebhooksAreContent(t *testing.T) {
	base := Sign("t", mustParse(t, localWebhooksJSON))
	if base != recordedLocalWebhooksHash {
		t.Errorf("hash of the local-webhooks fixture = %s, recorded %s", base, recordedLocalWebhooksHash)
	}
	for what, edit := range map[string][2]string{
		"secret": {`"LOOMCYCLE_GH_SECRET"`, `"LOOMCYCLE_OTHER_SECRET"`},
		"kind":   {`"kind": "hmac", "signing_secret_env"`, `"kind": "bearer", "bearer_token_env"`},
		"name":   {`"github":`, `"gitlab":`},
	} {
		if Sign("t", mustParse(t, strings.Replace(localWebhooksJSON, edit[0], edit[1], 1))) == base {
			t.Errorf("changing a local webhook's %s must change the team's content hash", what)
		}
	}
	relaid := withWebhook(`{ "auth":{"signing_secret_env":"LOOMCYCLE_GH_SECRET","kind":"hmac"}, "channel":"./events" }`)
	if Sign("t", mustParse(t, relaid)) != base {
		t.Error("key order / whitespace inside a local webhook's body changed the hash")
	}
}
