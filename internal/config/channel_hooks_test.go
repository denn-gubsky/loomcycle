package config

import (
	"strings"
	"testing"
	"time"
)

// A yaml channel's hooks load, and are refused where they could not work or
// would recurse: a run's event, and the runtime's own channels.
func TestChannelHooks_ValidatedAtLoad(t *testing.T) {
	ok, err := loadYAMLString(t, `
channels:
  inbox:
    scope: global
    hooks:
      channel_publish:
        - screen
        - {name: redact, url: "https://hooks.example/redact", fail_mode: closed}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ch := ok.Channels["inbox"]; !ch.HasHooks() || len(ch.Hooks["channel_publish"]) != 2 {
		t.Fatalf("hooks = %+v", ch.Hooks)
	}
	for body, want := range map[string]string{
		"scope: global\n    hooks: {agent_stop: [screen]}":                                             "channel_publish only",
		"scope: global\n    hooks: {channel_publish: [\"bad@name@\"]}":                                 "version",
		"scope: global\n    publisher: system\n    period: 1m\n    hooks: {channel_publish: [screen]}": "publisher: system",
	} {
		if _, err := loadYAMLString(t, "channels:\n  inbox:\n    "+body+"\n"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
	for _, name := range []string{"_system/interrupts/pending", "documents/d1/chunks"} {
		_, err := loadYAMLString(t, "channels:\n  "+name+":\n    scope: global\n    hooks: {channel_publish: [screen]}\n")
		if err == nil || !strings.Contains(err.Error(), "cannot carry hooks") {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
}

// Channel hooks are off unless asked for; their tuning has sane defaults.
func TestChannelHooks_EnvDefaults(t *testing.T) {
	for _, k := range []string{"LOOMCYCLE_CHANNEL_HOOKS", "LOOMCYCLE_CHANNEL_HOOKS_CONCURRENCY", "LOOMCYCLE_CHANNEL_HOOKS_PER_CHANNEL", "LOOMCYCLE_CHANNEL_HOOKS_MAX_WAIT"} {
		t.Setenv(k, "")
	}
	cfg, err := loadYAMLString(t, "channels: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Env
	if e.ChannelHooksEnabled || e.ChannelHooksConcurrency != 16 || e.ChannelHooksPerChannel != 4 || e.ChannelHooksMaxWait != 15*time.Minute {
		t.Fatalf("defaults = %v %d %d %s", e.ChannelHooksEnabled, e.ChannelHooksConcurrency, e.ChannelHooksPerChannel, e.ChannelHooksMaxWait)
	}
	t.Setenv("LOOMCYCLE_CHANNEL_HOOKS", "1")
	t.Setenv("LOOMCYCLE_CHANNEL_HOOKS_MAX_WAIT", "90s")
	cfg, _ = loadYAMLString(t, "channels: {}\n")
	if !cfg.Env.ChannelHooksEnabled || cfg.Env.ChannelHooksMaxWait != 90*time.Second {
		t.Fatalf("set = %v %s", cfg.Env.ChannelHooksEnabled, cfg.Env.ChannelHooksMaxWait)
	}
}
