package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadScheduleYAML writes a config with the given scheduled_runs block and
// returns the load error (nil on success).
func loadScheduleYAML(t *testing.T, scheduledRuns string) (*Config, error) {
	t.Helper()
	yamlPath := filepath.Join(t.TempDir(), "c.yaml")
	body := `
defaults: { provider: anthropic, model: x }
agents:
  worker:
    provider: anthropic
    model: x
channels:
  wave-in:
    scope: global
scheduled_runs:
` + scheduledRuns
	if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(yamlPath)
}

// The happy path: a cadence tick needs a channel and nothing else.
func TestScheduleDelivery_ChannelTickNeedsNoAgent(t *testing.T) {
	cfg, err := loadScheduleYAML(t, `
  nightly-tick:
    delivery: channel
    channel: wave-in
    schedule: "0 3 * * *"
    metadata: { batch: nightly }
    enabled: true
`)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	got := cfg.ScheduledRuns["nightly-tick"]
	if got.Delivery != "channel" || got.Channel != "wave-in" {
		t.Errorf("delivery/channel did not load: %+v", got)
	}
	if got.Agent != "" {
		t.Errorf("agent = %q, want empty", got.Agent)
	}
}

// The run-shaped fields are REFUSED on a channel tick, not ignored. Each case
// is a setting an operator would otherwise believe they had made.
func TestScheduleDelivery_ChannelTickRefusesRunShapedFields(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"agent", "    agent: worker\n", "forbids `agent`"},
		{"prompt", "    prompt: [{role: user, content: [{type: trusted-text, text: go}]}]\n", "forbids `prompt`"},
		{"on_complete", "    on_complete: [{kind: memory.set, scope: agent, key: k}]\n", "forbids `on_complete`"},
		{"required_credentials", "    required_credentials: [jobs]\n", "forbids credentials"},
		{"user_credentials_from_env", "    user_credentials_from_env: {jobs: LOOMCYCLE_JOBS_TOKEN}\n", "forbids credentials"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadScheduleYAML(t, "  tick:\n    delivery: channel\n    channel: wave-in\n    schedule: \"0 3 * * *\"\n"+c.extra)
			if err == nil {
				t.Fatalf("delivery=channel with %s loaded; want a refusal", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// A channel tick with no channel can never do anything — fail at boot, where
// the operator is looking, not at 3am on the first fire.
func TestScheduleDelivery_ChannelTickRequiresAChannel(t *testing.T) {
	_, err := loadScheduleYAML(t, `
  tick:
    delivery: channel
    schedule: "0 3 * * *"
`)
	if err == nil || !strings.Contains(err.Error(), "requires `channel`") {
		t.Fatalf("err = %v, want a refusal naming channel", err)
	}
}

// The default delivery is unchanged: an agent is still required, and a channel
// on a run schedule is a mix-up worth naming.
func TestScheduleDelivery_RunIsTheDefaultAndStillNeedsAnAgent(t *testing.T) {
	_, err := loadScheduleYAML(t, `
  tick:
    schedule: "0 3 * * *"
    prompt: [{role: user, content: [{type: trusted-text, text: go}]}]
`)
	if err == nil || !strings.Contains(err.Error(), "agent is required") {
		t.Fatalf("err = %v, want an agent-required refusal", err)
	}

	_, err = loadScheduleYAML(t, `
  tick:
    agent: worker
    channel: wave-in
    schedule: "0 3 * * *"
    prompt: [{role: user, content: [{type: trusted-text, text: go}]}]
`)
	if err == nil || !strings.Contains(err.Error(), "forbids `channel`") {
		t.Fatalf("err = %v, want a channel-forbidden refusal", err)
	}
}

func TestScheduleDelivery_UnknownDeliveryIsRefused(t *testing.T) {
	_, err := loadScheduleYAML(t, `
  tick:
    delivery: carrier-pigeon
    channel: wave-in
    schedule: "0 3 * * *"
`)
	if err == nil || !strings.Contains(err.Error(), "unknown delivery") {
		t.Fatalf("err = %v, want an unknown-delivery refusal", err)
	}
}
