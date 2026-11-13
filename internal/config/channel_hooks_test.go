package config

import (
	"strings"
	"testing"
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
