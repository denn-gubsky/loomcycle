package config

import (
	"strings"
	"testing"
)

func TestCheckTriggerUserBearer_TakesOnlyAStoredCredential(t *testing.T) {
	for _, ok := range []string{"", "$cred:hook-token", "$cred:a_b-1"} {
		if err := CheckTriggerUserBearer(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"ghp_literal", "Bearer $cred:x", "$cred:x tail", "${LOOMCYCLE_TOKEN}", "$ghapp:app", "$cred:"} {
		if err := CheckTriggerUserBearer(bad); err == nil || !strings.Contains(err.Error(), "only a stored credential") {
			t.Errorf("%q: %v, want a refusal", bad, err)
		}
	}
}

// The yaml load refuses a literal bearer, and a bearer on a delivery that
// starts no agent run.
func TestValidateStatic_UserBearer(t *testing.T) {
	hook := func(mut func(*Webhook)) Webhook {
		w := Webhook{Enabled: true, Delivery: "spawn", Agent: "a", UserBearer: "$cred:tok",
			Auth: WebhookAuth{Kind: "hmac", Header: "X-Sig", SigningSecretEnv: "LOOMCYCLE_S"}}
		if mut != nil {
			mut(&w)
		}
		return w
	}
	if err := validateStaticWebhook("w", hook(nil)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		w    Webhook
		want string
	}{
		"literal": {hook(func(w *Webhook) { w.UserBearer = "abc" }), "only a stored credential"},
		"channel": {hook(func(w *Webhook) { w.Delivery, w.Agent, w.Channel = "channel", "", "c" }), "forbids `user_bearer`"},
		"team":    {hook(func(w *Webhook) { w.Delivery, w.Agent, w.Team = "team", "", "t" }), "credentials"},
	} {
		if err := validateStaticWebhook("w", tc.w); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}

	sched := func(mut func(*ScheduledRun)) ScheduledRun {
		sr := ScheduledRun{Agent: "a", Schedule: "0 6 * * *", UserBearer: "$cred:tok"}
		if mut != nil {
			mut(&sr)
		}
		return sr
	}
	if err := validateScheduleDelivery("s", sched(nil)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		sr   ScheduledRun
		want string
	}{
		"literal": {sched(func(sr *ScheduledRun) { sr.UserBearer = "abc" }), "only a stored credential"},
		"channel": {sched(func(sr *ScheduledRun) { sr.Delivery, sr.Agent, sr.Channel = "channel", "", "c" }), "forbids credentials"},
		"team":    {sched(func(sr *ScheduledRun) { sr.Delivery, sr.Agent, sr.Team = "team", "", "t" }), "forbids credentials"},
	} {
		if err := validateScheduleDelivery("s", tc.sr); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}
}
