package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseHumanDuration_ReadsDaysAndGoDurations(t *testing.T) {
	const day = 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"30d":    30 * day,
		"1d12h":  36 * time.Hour,
		"0.5d":   12 * time.Hour,
		"12h":    12 * time.Hour,
		"90m":    90 * time.Minute,
		"1h30m":  90 * time.Minute,
		"1500ms": 1500 * time.Millisecond,
		"0":      0,
		"0s":     0,
		" 7d ":   7 * day,
		"0d":     0,
	} {
		got, err := ParseHumanDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseHumanDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"30", "-1d", "-12h", "d", "1d-2h", "thirty days", "1w", "30d12", "9999999d", "", "  ", "nand", "NaNd", "nand12h", "infd", "+Infd", "-infd"} {
		if got, err := ParseHumanDuration(in); err == nil {
			t.Errorf("ParseHumanDuration(%q) = %v, want an error", in, got)
		}
	}
}

// With no `retention:` block every family is off and every age is what the
// environment variables left it at when unset.
func TestRetention_UnsetLeavesEveryFamilyOff(t *testing.T) {
	cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n")
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Env
	if e.RetentionEnabled {
		t.Error("the sweeper is on with no retention block")
	}
	if e.RetentionDefsMode != "off" || e.RetentionDefsKeepLastN != 5 {
		t.Errorf("defs = mode %q keep %d, want off and 5", e.RetentionDefsMode, e.RetentionDefsKeepLastN)
	}
	for name, mode := range map[string]string{
		"chats": e.RetentionChatsMode, "mem": e.RetentionMemMode,
		"mem_content": e.RetentionMemContentMode, "empty_dossier": e.RetentionEmptyDossierMode,
	} {
		if mode != "" {
			t.Errorf("%s mode = %q, want unset", name, mode)
		}
	}
	// Zero is "the consumer's default" (1h, 30d, 30d) or "any age".
	for name, d := range map[string]time.Duration{
		"interval": e.RetentionInterval, "usage_detail": e.UsageDetailRetention, "trace": e.MemoryTraceMaxAge,
		"defs": e.RetentionDefsMaxAge, "chats": e.RetentionChatsMaxAge, "mem": e.RetentionMemMaxAge,
		"mem_content": e.RetentionMemContentMaxAge, "empty_dossier": e.RetentionEmptyDossierMaxAge,
	} {
		if d != 0 {
			t.Errorf("%s = %v, want 0", name, d)
		}
	}
	if e.RetentionExportDir != "" {
		t.Errorf("export dir = %q, want none", e.RetentionExportDir)
	}
}

func TestRetention_BlockSetsEveryFamily(t *testing.T) {
	cfg, err := loadYAMLString(t, `defaults: {provider: mock}
retention:
  enabled: true
  interval: 24h
  export_dir: /data/retention-exports
  defs: {mode: prune, max_age: 30d, keep_last_n: 0}
  chats: {mode: export+prune, max_age: 90d}
  mem: {mode: prune, max_age: 7d}
  mem_content: {mode: export+prune, max_age: 1d12h}
  empty_dossier: {mode: prune, max_age: 12h}
  usage_detail: {max_age: 60d}
  trace: {max_age: 14d}
`)
	if err != nil {
		t.Fatal(err)
	}
	const day = 24 * time.Hour
	e := cfg.Env
	if !e.RetentionEnabled {
		t.Error("the sweeper is off with families set to prune")
	}
	if e.RetentionInterval != day || e.RetentionExportDir != "/data/retention-exports" {
		t.Errorf("interval %v, export dir %q", e.RetentionInterval, e.RetentionExportDir)
	}
	if e.RetentionDefsMode != "prune" || e.RetentionDefsMaxAge != 30*day || e.RetentionDefsKeepLastN != 0 {
		t.Errorf("defs = %q %v keep %d", e.RetentionDefsMode, e.RetentionDefsMaxAge, e.RetentionDefsKeepLastN)
	}
	if e.RetentionChatsMode != "export+prune" || e.RetentionChatsMaxAge != 90*day {
		t.Errorf("chats = %q %v", e.RetentionChatsMode, e.RetentionChatsMaxAge)
	}
	if e.RetentionMemMode != "prune" || e.RetentionMemMaxAge != 7*day {
		t.Errorf("mem = %q %v", e.RetentionMemMode, e.RetentionMemMaxAge)
	}
	if e.RetentionMemContentMode != "export+prune" || e.RetentionMemContentMaxAge != 36*time.Hour {
		t.Errorf("mem_content = %q %v", e.RetentionMemContentMode, e.RetentionMemContentMaxAge)
	}
	if e.RetentionEmptyDossierMode != "prune" || e.RetentionEmptyDossierMaxAge != 12*time.Hour {
		t.Errorf("empty_dossier = %q %v", e.RetentionEmptyDossierMode, e.RetentionEmptyDossierMaxAge)
	}
	if e.UsageDetailRetention != 60*day || e.MemoryTraceMaxAge != 14*day {
		t.Errorf("usage_detail %v, trace %v", e.UsageDetailRetention, e.MemoryTraceMaxAge)
	}
}

func TestRetention_RefusesAValueItCannotApply(t *testing.T) {
	for block, want := range map[string]string{
		"retention: {chats: {mode: delete}}":      "retention.chats.mode",
		"retention: {defs: {mode: Prune}}":        "retention.defs.mode",
		"retention: {empty_dossier: {mode: on}}":  "retention.empty_dossier.mode",
		"retention: {defs: {keep_last_n: -1}}":    "retention.defs.keep_last_n",
		"retention: {chats: {max_age: 30}}":       "not a duration",
		"retention: {chats: {max_age: -1d}}":      "negative",
		"retention: {interval: hourly}":           "not a duration",
		"retention: {trace: {max_age: [1d, 2d]}}": "one value",
		"retention: {enabled: sometimes}":         "into bool",
		// A family that prunes says from what age, even when that is "any".
		"retention: {chats: {mode: prune}}":                        "retention.chats.max_age",
		"retention: {enabled: true, defs: {mode: export+prune}}":   "retention.defs.max_age",
		"retention: {mem: {mode: prune}}":                          "retention.mem.max_age",
		"retention: {mem_content: {mode: prune}}":                  "retention.mem_content.max_age",
		"retention: {empty_dossier: {mode: prune, max_age: null}}": "retention.empty_dossier.max_age",
	} {
		_, err := loadYAMLString(t, "defaults: {provider: mock}\n"+block+"\n")
		if err == nil {
			t.Errorf("%s: loaded, want an error", block)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error does not name %q: %s", block, want, err)
		}
	}
}

// "No minimum age" is a choice a pruning family has to write down: an explicit
// zero is taken, and a family that is off needs no age.
func TestRetention_APruningFamilyTakesAnExplicitZeroAge(t *testing.T) {
	for _, block := range []string{
		"retention: {chats: {mode: prune, max_age: 0s}}",
		"retention: {chats: {mode: prune, max_age: 0}}",
		"retention: {enabled: true, defs: {mode: prune, max_age: 0d}}",
		"retention: {chats: {mode: off}}",
		"retention: {chats: {max_age: 30d}}",
	} {
		cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n"+block+"\n")
		if err != nil {
			t.Errorf("%s: %v", block, err)
			continue
		}
		if cfg.Env.RetentionChatsMaxAge != 0 && !strings.Contains(block, "30d") {
			t.Errorf("%s: chats max age = %v, want 0", block, cfg.Env.RetentionChatsMaxAge)
		}
	}
}

// A later layer's family replaces only the keys it names.
func TestRetention_LayersMergePerKey(t *testing.T) {
	cfg, err := LoadLayers(
		Layer{Name: "base.yaml", Data: []byte("defaults: {provider: mock}\nretention: {chats: {mode: prune, max_age: 90d}}\n")},
		Layer{Name: "site.yaml", Data: []byte("retention: {chats: {max_age: 30d}, defs: {mode: prune, max_age: 7d}}\n")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env.RetentionChatsMode != "prune" || cfg.Env.RetentionChatsMaxAge != 30*24*time.Hour || cfg.Env.RetentionDefsMode != "prune" {
		t.Errorf("chats = %q %v, defs = %q", cfg.Env.RetentionChatsMode, cfg.Env.RetentionChatsMaxAge, cfg.Env.RetentionDefsMode)
	}
}

// Zero is "any age" for a family that deletes, so only a written zero may
// mean it. An empty string (a quoted "${VAR}" whose variable is unset expands
// to one) and a not-a-number day count both used to read as zero.
func TestRetention_RefusesADurationThatWouldReadAsZero(t *testing.T) {
	t.Setenv("LOOMCYCLE_TEST_UNSET_AGE", "")
	for block, key := range map[string]string{
		`retention: {chats: {mode: prune, max_age: ""}}`:                            "retention.chats.max_age",
		`retention: {chats: {mode: prune, max_age: "${LOOMCYCLE_TEST_UNSET_AGE}"}}`: "retention.chats.max_age",
		`retention: {chats: {mode: prune, max_age: nand}}`:                          "retention.chats.max_age",
		`retention: {enabled: true, defs: {mode: export+prune, max_age: NaNd}}`:     "retention.defs.max_age",
		`retention: {mem: {mode: prune, max_age: " "}}`:                             "retention.mem.max_age",
		`retention: {mem_content: {max_age: ""}}`:                                   "retention.mem_content.max_age",
		`retention: {empty_dossier: {mode: prune, max_age: nand12h}}`:               "retention.empty_dossier.max_age",
		`retention: {interval: ""}`:                                                 "retention.interval",
		`retention: {interval: nand}`:                                               "retention.interval",
		`retention: {usage_detail: {max_age: ""}}`:                                  "retention.usage_detail.max_age",
		`retention: {trace: {max_age: nand}}`:                                       "retention.trace.max_age",
		`metrics: {sample_interval: ""}`:                                            "metrics.sample_interval",
		`metrics: {sample_interval: nand}`:                                          "metrics.sample_interval",
	} {
		body := "defaults: {provider: mock}\n" + block + "\n"
		// One file, and the same file as a layer over another: the two paths
		// parse the yaml differently.
		_, one := loadYAMLString(t, body)
		_, layered := LoadLayers(Layer{Name: "base.yaml", Data: []byte("agents: {}\n")}, Layer{Name: "site.yaml", Data: []byte(body)})
		for path, err := range map[string]error{"one file": one, "layered": layered} {
			if err == nil {
				t.Errorf("%s (%s): loaded, want an error", block, path)
				continue
			}
			if !strings.Contains(err.Error(), key+":") {
				t.Errorf("%s (%s): error does not name %s: %s", block, path, key, err)
			}
		}
	}
}

// Every duration key of the config is checked by name before the config is
// decoded. A HumanDuration field added anywhere under Config is found by its
// type, so it cannot be left out of the check.
func TestHumanDurationKeys_ListsEveryDurationOfTheConfig(t *testing.T) {
	got := humanDurationKeys()
	for _, want := range []string{
		"retention.interval", "retention.defs.max_age", "retention.chats.max_age", "retention.mem.max_age",
		"retention.mem_content.max_age", "retention.empty_dossier.max_age", "retention.usage_detail.max_age",
		"retention.trace.max_age", "metrics.sample_interval",
	} {
		if !got[want] {
			t.Errorf("humanDurationKeys() lacks %s (has %v)", want, got)
		}
	}
}
