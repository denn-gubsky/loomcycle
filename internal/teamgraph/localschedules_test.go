package teamgraph

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// localSchedulesJSON is a team that ticks into its own `ticks` channel every
// minute, and whose entry Starter reads it.
const localSchedulesJSON = `{
  "entry": "wave",
  "local": {
    "channels":  {"ticks": {"scope": "tenant"}},
    "schedules": {"minute": {"schedule": "@every 1m", "channel": "./ticks", "payload": {"b": 1, "a": [true, null]}}}
  },
  "states": [
    {"state": "wave", "handler": {"kind": "starter", "source": {"channel": "./ticks"},
      "fanout": {"agent": "reviewer", "per": "message", "max": 4}}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [{"from": "wave", "to": "done", "on": "success"}]
}`

// withSchedule is localSchedulesJSON with its one schedule's body replaced.
func withSchedule(body string) string {
	return strings.Replace(localSchedulesJSON,
		`{"schedule": "@every 1m", "channel": "./ticks", "payload": {"b": 1, "a": [true, null]}}`, body, 1)
}

func TestValidate_AcceptsDeclaredLocalSchedules(t *testing.T) {
	d := mustParse(t, localSchedulesJSON)
	if err := Validate(d); err != nil {
		t.Fatalf("a schedule ticking into the team's own channel must validate: %v", err)
	}
	ls, ok := d.LocalSchedule("minute")
	if !ok || ls.Schedule != "@every 1m" || ls.Channel != "./ticks" {
		t.Fatalf("LocalSchedule(minute) = %+v, %v", ls, ok)
	}
	// Held canonically: keys sorted, no whitespace.
	if got := string(ls.Payload); got != `{"a":[true,null],"b":1}` {
		t.Errorf("payload held as %s, want canonical JSON", got)
	}
	for _, body := range []string{
		`{"schedule": "*/5 * * * *", "channel": "./ticks"}`,
		`{"schedule": "CRON_TZ=Europe/Berlin 0 9 * * 1-5", "channel": "./ticks", "payload": "wake"}`,
		`{"schedule": "@hourly", "channel": "./ticks", "payload": null}`,
		`{"schedule": "@every 10s", "channel": "./ticks", "payload": 3}`,
	} {
		if err := Validate(mustParse(t, withSchedule(body))); err != nil {
			t.Errorf("%s must validate: %v", body, err)
		}
	}
}

func TestValidate_RefusesALocalScheduleThatCouldNotRun(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no cadence":          {`{"channel": "./ticks"}`, "schedule: required"},
		"bad cadence":         {`{"schedule": "every minute", "channel": "./ticks"}`, "invalid cadence"},
		"six fields":          {`{"schedule": "*/5 * * * * *", "channel": "./ticks"}`, "invalid cadence"},
		"below the minimum":   {`{"schedule": "@every 9s", "channel": "./ticks"}`, "at most every 10s"},
		"sub-second":          {`{"schedule": "@every 500ms", "channel": "./ticks"}`, "at most every 10s"},
		"a date that is not":  {`{"schedule": "0 0 30 2 *", "channel": "./ticks"}`, "never fires"},
		"no channel":          {`{"schedule": "@every 1m"}`, `must name one of the team's own channels`},
		"a global channel":    {`{"schedule": "@every 1m", "channel": "ticks"}`, `must name one of the team's own channels`},
		"an undeclared one":   {`{"schedule": "@every 1m", "channel": "./ghost"}`, `"./ghost" names a channel the team does not declare under local.channels (declared: ticks)`},
		"a reserved name":     {`{"schedule": "@every 1m", "channel": "_team/x/ticks"}`, `must name one of the team's own channels`},
		"an oversize payload": {`{"schedule": "@every 1m", "channel": "./ticks", "payload": "` + strings.Repeat("x", MaxLocalSchedulePayloadBytes) + `"}`, "the limit is 16384"},
	} {
		err := Validate(mustParse(t, withSchedule(tc.body)))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `local.schedules["minute"]`) {
			t.Errorf("%s: want a refusal naming the schedule and %q, got %v", name, tc.want, err)
		}
	}
}

// A field a ScheduleDef takes and a team's own schedule does not is refused by
// name when the definition is parsed — never dropped.
func TestParse_RefusesAFieldALocalScheduleDoesNotTake(t *testing.T) {
	for field, value := range map[string]string{
		"max_fires": `3`, "agent": `"reviewer"`, "prompt": `"go"`, "on_complete": `[]`,
		"user_id": `"alice"`, "timezone": `"UTC"`, "metadata": `{}`, "enabled": `true`,
		"delivery": `"channel"`, "required_credentials": `["x"]`, "team": `"t"`,
	} {
		body := `{"schedule": "@every 1m", "channel": "./ticks", "` + field + `": ` + value + `}`
		_, err := Parse([]byte(withSchedule(body)))
		if err == nil || !strings.Contains(err.Error(), `"`+field+`"`) || !strings.Contains(err.Error(), "only schedule, channel and payload") {
			t.Errorf("%s: want a refusal naming the field, got %v", field, err)
		}
	}
	_, err := Parse([]byte(withSchedule(`{"schedule": "@every 1m", "channel": "./ticks", "max_fires": 3}`)))
	if err == nil || !strings.Contains(err.Error(), "only while a walk of the team runs") {
		t.Errorf("max_fires: the refusal should say why, got %v", err)
	}
	for _, body := range []string{`"@every 1m"`, `[]`, `null`, `{"schedule": 60, "channel": "./ticks"}`} {
		if _, err := Parse([]byte(withSchedule(body))); err == nil {
			t.Errorf("a local schedule body of %s must be refused", body)
		}
	}
}

func TestValidate_RefusesBadLocalScheduleName(t *testing.T) {
	for _, name := range []string{"a/b", "a.b", "a b", "", strings.Repeat("x", 65), "./a"} {
		def := strings.Replace(localSchedulesJSON, `"minute":`, quote(name)+`:`, 1)
		if err := Validate(mustParse(t, def)); err == nil {
			t.Errorf("local schedule name %q must be refused", name)
		}
	}
}

func TestValidate_CapsTheNumberOfLocalSchedules(t *testing.T) {
	build := func(n int) Definition {
		d := mustParse(t, localSchedulesJSON)
		for i := 1; i < n; i++ {
			d.Local.Schedules[fmt.Sprintf("s%d", i)] = LocalSchedule{Schedule: "@every 1m", Channel: "./ticks"}
		}
		return d
	}
	if err := Validate(build(MaxLocalSchedules)); err != nil {
		t.Fatalf("%d local schedules is the limit and must be accepted: %v", MaxLocalSchedules, err)
	}
	err := Validate(build(MaxLocalSchedules + 1))
	if err == nil || !strings.Contains(err.Error(), "more than the maximum 16") {
		t.Fatalf("%d local schedules must be refused naming the limit, got %v", MaxLocalSchedules+1, err)
	}
}

// The walk re-checks a stored body before it starts; CheckLocalRefs is that
// check, so it must cover schedules too.
func TestCheckLocalRefs_RefusesALocalScheduleThatCouldNotRun(t *testing.T) {
	for _, body := range []string{
		`{"schedule": "@every 1s", "channel": "./ticks"}`,
		`{"schedule": "@every 1m", "channel": "./nowhere"}`,
	} {
		if err := CheckLocalRefs(mustParse(t, withSchedule(body))); err == nil || !strings.Contains(err.Error(), "local.schedules") {
			t.Errorf("%s: want a refusal, got %v", body, err)
		}
	}
}

func TestParseLocalCadence_FiresOnTheScheduleDefGrammar(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for expr, want := range map[string]time.Time{
		"@every 10s":  at.Add(10 * time.Second),
		"*/5 * * * *": at.Add(5 * time.Minute),
		"@hourly":     at.Add(time.Hour),
	} {
		sched, err := ParseLocalCadence(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got := sched.Next(at); !got.Equal(want) {
			t.Errorf("%s: next after %s = %s, want %s", expr, at, got, want)
		}
	}
}

// A team with local agents, skills or channels and no schedules keeps the hash
// it had before schedules existed; an empty schedules kind is no content.
func TestSign_EmptyLocalSchedulesKeepTheRecordedHash(t *testing.T) {
	d := mustParse(t, sdlcJSON)
	d.Local = &Local{Schedules: map[string]LocalSchedule{}}
	if got := Sign("sdlc", d); got != recordedNoLocalHash {
		t.Errorf("an empty local.schedules changed the hash to %s, want %s", got, recordedNoLocalHash)
	}
	withChannels := mustParse(t, localChannelsJSON)
	withChannels.Local.Schedules = map[string]LocalSchedule{}
	if got := Sign("t", withChannels); got != recordedLocalChannelsHash {
		t.Errorf("an empty local.schedules changed a local-channels team's hash to %s, want %s", got, recordedLocalChannelsHash)
	}
	withAgents := mustParse(t, localJSON)
	withAgents.Local.Schedules = map[string]LocalSchedule{}
	if got := Sign("t", withAgents); got != recordedLocalAgentsHash {
		t.Errorf("a local-agents team's hash moved to %s, want %s", got, recordedLocalAgentsHash)
	}
}

// recordedLocalSchedulesHash pins the hash of localSchedulesJSON: the bytes a
// local schedule contributes are part of every recorded hash from here on.
const recordedLocalSchedulesHash = "sha256:8e9a01a7f0e9f507fed1b1ec50ecbd27e5302a59937969f57618834da6de855e"

func TestSign_LocalSchedulesAreContent(t *testing.T) {
	base := Sign("t", mustParse(t, localSchedulesJSON))
	if base != recordedLocalSchedulesHash {
		t.Errorf("hash of the local-schedules fixture = %s, recorded %s", base, recordedLocalSchedulesHash)
	}
	for what, edit := range map[string][2]string{
		"cadence": {`"@every 1m"`, `"@every 2m"`},
		"payload": {`"b": 1`, `"b": 2`},
	} {
		if Sign("t", mustParse(t, strings.Replace(localSchedulesJSON, edit[0], edit[1], 1))) == base {
			t.Errorf("changing a local schedule's %s must change the team's content hash", what)
		}
	}
	// Layout of a payload is not content.
	relaid := strings.Replace(localSchedulesJSON, `{"b": 1, "a": [true, null]}`, `{ "a":[true,null],"b":1 }`, 1)
	if Sign("t", mustParse(t, relaid)) != base {
		t.Error("key order / whitespace inside a local schedule's payload changed the hash")
	}
	// An explicit null payload is no payload.
	noPayload := withSchedule(`{"schedule": "@every 1m", "channel": "./ticks"}`)
	nullPayload := withSchedule(`{"schedule": "@every 1m", "channel": "./ticks", "payload": null}`)
	if Sign("t", mustParse(t, noPayload)) != Sign("t", mustParse(t, nullPayload)) {
		t.Error("payload: null and no payload hash differently")
	}
}
