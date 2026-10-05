package teamgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// localschedules.go — the schedules a team declares for itself.
//
// A local schedule is a clock that lives inside the team: while a walk of the
// team runs, it publishes a message into one of the team's own channels on its
// cadence, and when the walk ends it stops. It has no row of its own and is
// not the scheduler's: nothing ticks for a team that has no walk running.
//
// It is deliberately small. It names a cadence, a channel of the team's own,
// and optionally the message to publish — no agent, prompt, credentials,
// on_complete or fire cap. Anything a tick should cause is the walk's to do
// with the message, which keeps every effect of the team inside the team.

// MaxLocalSchedules bounds how many schedules a team may declare. Each is a
// timer per running walk, so the bound is on what one walk costs while it
// waits, not on what a definition can list.
const MaxLocalSchedules = 16

// MinLocalScheduleInterval is the shortest cadence a team's own schedule may
// have. The scheduler has no floor of its own (its sweep interval paces it), and
// a team's timer has no sweep: this is that floor.
const MinLocalScheduleInterval = 10 * time.Second

// MaxLocalSchedulePayloadBytes bounds a schedule's literal payload. Every tick
// writes it again, so it is bounded far below the definition's own cap.
const MaxLocalSchedulePayloadBytes = 16 * 1024

// LocalSchedule is one schedule a team declares for itself. DO NOT reorder the
// fields: their order is the encoding the team's content hash is taken over.
type LocalSchedule struct {
	// Schedule is the cadence, in the grammar a ScheduleDef's `schedule`
	// takes: five-field cron or a descriptor such as "@every 1m".
	Schedule string `json:"schedule"`
	// Channel is the team's own channel each tick is published to, "./<name>".
	Channel string `json:"channel"`
	// Payload, when set, is the message each tick publishes, held canonically
	// (keys sorted, no insignificant whitespace). Unset, a tick publishes the
	// message a schedule's channel delivery does.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// localScheduleRefused names, for a field a ScheduleDef takes and a team's own
// schedule does not, why — the fields an author coming from ScheduleDef is
// most likely to bring along.
var localScheduleRefused = map[string]string{
	"max_fires": "a team's own schedule lives only while a walk of the team runs, and every walk starts it afresh",
	"timezone":  "write the zone into the cadence itself, e.g. \"CRON_TZ=Europe/Berlin 0 9 * * *\"",
	"metadata":  "the message a tick publishes is `payload`",
}

// decodeLocalSchedules decodes local.schedules strictly: a field a team's own
// schedule does not take is refused by name, never dropped.
func decodeLocalSchedules(raw json.RawMessage) (map[string]LocalSchedule, error) {
	var bodies map[string]json.RawMessage
	if err := json.Unmarshal(raw, &bodies); err != nil {
		return nil, &localError{"local.schedules: must be an object of name → schedule definition"}
	}
	// Non-nil even when empty, as for agents: a fork that sends `schedules: {}`
	// is stating the whole list.
	out := make(map[string]LocalSchedule, len(bodies))
	for name, body := range bodies {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return nil, &localError{fmt.Sprintf("local.schedules[%q]: must be an object of schedule, channel and payload", name)}
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var ls LocalSchedule
		for _, k := range keys {
			v := fields[k]
			switch k {
			case "schedule", "channel":
				var s string
				if err := json.Unmarshal(v, &s); err != nil {
					return nil, &localError{fmt.Sprintf("local.schedules[%q].%s: must be a string", name, k)}
				}
				if k == "schedule" {
					ls.Schedule = s
				} else {
					ls.Channel = s
				}
			case "payload":
				if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
					continue
				}
				canon, err := canonicalValue(v)
				if err != nil {
					return nil, &localError{fmt.Sprintf("local.schedules[%q].payload: %s", name, err)}
				}
				ls.Payload = canon
			default:
				msg := fmt.Sprintf("local.schedules[%q]: %q is not a field of a team's own schedule, which takes only schedule, channel and payload", name, k)
				if why, ok := localScheduleRefused[k]; ok {
					msg += " (" + why + ")"
				}
				return nil, &localError{msg}
			}
		}
		out[name] = ls
	}
	return out, nil
}

// canonicalValue re-encodes any JSON value with object keys sorted at every
// depth and no insignificant whitespace. Numbers keep their literal text.
func canonicalValue(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("is not valid JSON")
	}
	return json.Marshal(v)
}

// LocalSchedule returns the local schedule `name`, if the definition declares
// it.
func (d Definition) LocalSchedule(name string) (LocalSchedule, bool) {
	if d.Local == nil {
		return LocalSchedule{}, false
	}
	ls, ok := d.Local.Schedules[name]
	return ls, ok
}

// LocalScheduleNames returns the declared local schedule names, sorted.
func (d Definition) LocalScheduleNames() []string {
	if d.Local == nil {
		return nil
	}
	out := make([]string, 0, len(d.Local.Schedules))
	for name := range d.Local.Schedules {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ParseLocalCadence parses a team's own schedule's cadence with the parser the
// scheduler fires ScheduleDefs with, and refuses one that fires more often than
// MinLocalScheduleInterval.
//
// Only a "@every" descriptor can: the five-field grammar has no seconds field,
// so its finest cadence is one minute.
func ParseLocalCadence(expr string) (cron.Schedule, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("schedule: required (five-field cron, or a descriptor such as \"@every 1m\")")
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("schedule: invalid cadence %q: %w", expr, err)
	}
	if every, ok := sched.(cron.ConstantDelaySchedule); ok && every.Delay < MinLocalScheduleInterval {
		return nil, fmt.Errorf("schedule: %q fires every %s; a team's own schedule fires at most every %s",
			expr, every.Delay, MinLocalScheduleInterval)
	}
	return sched, nil
}

// validateLocalScheduleNames checks the declared schedules' count and names.
func validateLocalScheduleNames(d Definition) error {
	if d.Local != nil && len(d.Local.Schedules) > MaxLocalSchedules {
		return fmt.Errorf("team definition: local.schedules declares %d schedules, more than the maximum %d", len(d.Local.Schedules), MaxLocalSchedules)
	}
	for _, name := range d.LocalScheduleNames() {
		if err := validateLocalName("schedule", name); err != nil {
			return fmt.Errorf("team definition: local.schedules: %w", err)
		}
	}
	return nil
}

// CheckLocalSchedules reports the first declared schedule that could not run:
// its cadence does not parse or is too fast, its payload is too large, or its
// channel is not one of the team's own channels. Store-free; a walk checks it
// again before it starts (through CheckLocalRefs), for the reason
// CheckLocalRefs gives, and a restore before it writes a body.
//
// The channel must be the team's own: a schedule inside a team reaches what is
// inside the team and nothing else, so a bare name — a channel the operator or
// the tenant declared — is refused, not resolved.
func CheckLocalSchedules(d Definition) error {
	for _, name := range d.LocalScheduleNames() {
		ls := d.Local.Schedules[name]
		where := fmt.Sprintf("team definition: local.schedules[%q]", name)
		if _, err := ParseLocalCadence(ls.Schedule); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if len(ls.Payload) > MaxLocalSchedulePayloadBytes {
			return fmt.Errorf("%s: payload is %d bytes; the limit is %d", where, len(ls.Payload), MaxLocalSchedulePayloadBytes)
		}
		local, isLocal := LocalRef(ls.Channel)
		if !isLocal {
			return fmt.Errorf("%s: channel %q must name one of the team's own channels as \"./<name>\" — "+
				"a team's own schedule publishes only into the team", where, ls.Channel)
		}
		if _, ok := d.LocalChannel(local); !ok {
			declared := "it declares none"
			if names := d.LocalChannelNames(); len(names) > 0 {
				declared = "declared: " + strings.Join(names, ", ")
			}
			return fmt.Errorf("%s: channel %q names a channel the team does not declare under local.channels (%s)", where, ls.Channel, declared)
		}
	}
	return nil
}
