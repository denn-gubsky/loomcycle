package teamgraph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// localwebhooks.go — the webhooks a team declares for itself.
//
// A local webhook is an inbound endpoint that lives inside the team: while a
// walk of the team runs, a signed POST to it publishes the request body into
// one of the team's own channels, and when no walk runs it answers like a
// webhook that does not exist. It has no row of its own and is not a
// WebhookDef: nothing receives for a team that has no walk running.
//
// Its body is a WebhookDef's auth block, the team's own channel it publishes
// into, and optionally where in the body the publisher's user id is — nothing
// that would start a run or reach outside the team. The auth rules are the
// runtime's (a WebhookDef's), which this leaf package cannot import; the
// authoring caller decodes and judges the body. Here: the names, the count,
// and that the channel is one the team declares.

// MaxLocalWebhooks bounds how many webhooks a team may declare: each is an
// endpoint a running walk opens to the outside.
const MaxLocalWebhooks = 16

// LocalWebhook returns the body of the local webhook `name`, if the team
// declares it.
func (d Definition) LocalWebhook(name string) (json.RawMessage, bool) {
	if d.Local == nil {
		return nil, false
	}
	body, ok := d.Local.Webhooks[name]
	return body, ok
}

// LocalWebhookNames returns the declared local webhook names, sorted.
func (d Definition) LocalWebhookNames() []string {
	if d.Local == nil {
		return nil
	}
	out := make([]string, 0, len(d.Local.Webhooks))
	for name := range d.Local.Webhooks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validateLocalWebhookNames checks the declared webhooks' count and names. A
// name is one segment because it is one segment of the URL the webhook is
// reached at.
func validateLocalWebhookNames(out *issues, d Definition) {
	if d.Local != nil && len(d.Local.Webhooks) > MaxLocalWebhooks {
		out.top("local.webhooks", "team definition: local.webhooks declares %d webhooks, more than the maximum %d", len(d.Local.Webhooks), MaxLocalWebhooks)
	}
	for _, name := range d.LocalWebhookNames() {
		if err := validateLocalName("webhook", name); err != nil {
			out.top(PathKey("local.webhooks", name), "team definition: local.webhooks: %v", err)
		}
	}
}

// CheckLocalWebhooks reports the first declared webhook whose channel is not
// one of the team's own channels. Store-free; a walk checks it again before it
// starts (through CheckLocalRefs), for the reason CheckLocalRefs gives.
//
// The channel must be the team's own: a webhook inside a team delivers into
// the team and nothing else, so a bare name — a channel the operator or the
// tenant declared — is refused, not resolved.
func CheckLocalWebhooks(d Definition) error { return first(localWebhookIssues(d)) }

// localWebhookIssues is CheckLocalWebhooks collecting every finding.
func localWebhookIssues(d Definition) []*Issue {
	var out issues
	for _, name := range d.LocalWebhookNames() {
		path := PathKey("local.webhooks", name) + ".channel"
		where := fmt.Sprintf("team definition: local.webhooks[%q]", name)
		// Only the channel is read here; the authoring caller decodes the rest
		// strictly. A channel that is not a string reads as none.
		var target struct {
			Channel string `json:"channel"`
		}
		_ = json.Unmarshal(d.Local.Webhooks[name], &target)
		local, isLocal := LocalRef(target.Channel)
		if !isLocal {
			out.top(path, "%s: channel %q must name one of the team's own channels as \"./<name>\" — "+
				"a team's own webhook publishes only into the team", where, target.Channel)
			continue
		}
		if _, ok := d.LocalChannel(local); !ok {
			declared := "it declares none"
			if names := d.LocalChannelNames(); len(names) > 0 {
				declared = "declared: " + strings.Join(names, ", ")
			}
			out.top(path, "%s: channel %q names a channel the team does not declare under local.channels (%s)", where, target.Channel, declared)
		}
	}
	return out
}
