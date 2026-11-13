package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// HumanDuration is a duration written the way an operator states a retention
// age: a Go duration ("12h", "90m", "1h30m"), optionally led by a whole or
// fractional number of days ("30d", "1d12h"). "0" is zero. A bare number with
// no unit is refused: nothing says whether it meant seconds, hours or days.
type HumanDuration time.Duration

// Duration is d as a time.Duration.
func (d HumanDuration) Duration() time.Duration { return time.Duration(d) }

// ParseHumanDuration parses s as described on HumanDuration. A negative
// duration is refused: no setting that takes one has a meaning for it.
func ParseHumanDuration(s string) (time.Duration, error) {
	v := strings.TrimSpace(s)
	if v == "" || v == "0" {
		return 0, nil
	}
	rest := v
	var days float64
	// No Go duration unit contains a "d", so the first one ends the days part.
	if i := strings.IndexByte(v, 'd'); i >= 0 {
		n, err := strconv.ParseFloat(v[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration such as 30d, 12h or 1d12h", s)
		}
		days, rest = n, v[i+1:]
	}
	var d time.Duration
	if rest != "" {
		var err error
		if d, err = time.ParseDuration(rest); err != nil {
			return 0, fmt.Errorf("%q is not a duration such as 30d, 12h or 1d12h", s)
		}
	}
	if days < 0 || d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	const day = 24 * time.Hour
	// 106751 days is the most a time.Duration holds.
	if days > float64((1<<63-1)/int64(day)) {
		return 0, fmt.Errorf("%q is too long", s)
	}
	total := time.Duration(days*float64(day)) + d
	if total < 0 {
		return 0, fmt.Errorf("%q is too long", s)
	}
	return total, nil
}

// UnmarshalYAML reads a duration scalar.
func (d *HumanDuration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration is one value such as 30d or 12h", n.Line)
	}
	v, err := ParseHumanDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = HumanDuration(v)
	return nil
}

// MarshalYAML writes d in a form UnmarshalYAML reads back.
func (d HumanDuration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// RetentionConfig is the `retention:` block: what the runtime deletes with
// age, and when. Every family is off until its mode says otherwise, and the
// sweeper runs only when a family's mode is prune or export+prune.
type RetentionConfig struct {
	// Interval is how often the sweeper runs. Unset = 1h.
	Interval HumanDuration `yaml:"interval,omitempty"`
	// ExportDir is where a family in export+prune mode writes what it is
	// about to delete. A family in that mode with no directory deletes
	// nothing.
	ExportDir string `yaml:"export_dir,omitempty"`
	// Defs prunes old versions of substrate definitions.
	Defs RetentionDefs `yaml:"defs,omitempty"`
	// Chats deletes aged sessions with their runs and events; a pinned
	// session is exempt.
	Chats RetentionFamily `yaml:"chats,omitempty"`
	// Mem reclaims the memory of fully retired agents.
	Mem RetentionFamily `yaml:"mem,omitempty"`
	// MemContent prunes superseded memory content from live scopes;
	// evidential content is exempt at any age.
	MemContent RetentionFamily `yaml:"mem_content,omitempty"`
	// EmptyDossier deletes subject documents left with no facts and nothing
	// pointing at them. Off unless asked for: a dossier usually empties as a
	// side effect of someone else's erasure.
	EmptyDossier RetentionFamily `yaml:"empty_dossier,omitempty"`
	// UsageDetail is the age at which per-call usage rows are rolled up into
	// totals and deleted. It has no mode: the rollup always runs. Unset = 30d.
	UsageDetail RetentionAge `yaml:"usage_detail,omitempty"`
	// Trace is how long an indexed conversation turn stays searchable. It has
	// no mode: the trace index has its own switch. Unset = 30d.
	Trace RetentionAge `yaml:"trace,omitempty"`
}

// RetentionFamily is one retention family: whether it deletes, and from what
// age.
type RetentionFamily struct {
	// Mode is off (the default), prune, or export+prune.
	Mode string `yaml:"mode,omitempty"`
	// MaxAge is the age past which an item is eligible. Unset or 0 = any age.
	MaxAge HumanDuration `yaml:"max_age,omitempty"`
}

// RetentionDefs is the definitions family, which also keeps a number of
// versions whatever their age.
type RetentionDefs struct {
	Mode   string        `yaml:"mode,omitempty"`
	MaxAge HumanDuration `yaml:"max_age,omitempty"`
	// KeepLastN is how many of the newest prunable versions of each
	// (tenant, name) are kept. Unset = 5; 0 keeps none.
	KeepLastN *int `yaml:"keep_last_n,omitempty"`
}

// RetentionAge is a retention age with no mode of its own.
type RetentionAge struct {
	MaxAge HumanDuration `yaml:"max_age,omitempty"`
}

// retentionDefsKeepLastNDefault is retention.defs.keep_last_n when unset.
const retentionDefsKeepLastNDefault = 5

// retentionAgeDefault is retention.usage_detail.max_age and
// retention.trace.max_age when unset: the fallback the usage rollup and the
// trace index each apply to a zero age.
const retentionAgeDefault = 30 * 24 * time.Hour

// Retention modes. The empty string is off.
const (
	RetentionModeOff         = "off"
	RetentionModePrune       = "prune"
	RetentionModeExportPrune = "export+prune"
)

// modes returns each family's mode with the yaml key that set it.
func (r *RetentionConfig) modes() []struct{ key, mode string } {
	return []struct{ key, mode string }{
		{"retention.defs.mode", r.Defs.Mode},
		{"retention.chats.mode", r.Chats.Mode},
		{"retention.mem.mode", r.Mem.Mode},
		{"retention.mem_content.mode", r.MemContent.Mode},
		{"retention.empty_dossier.mode", r.EmptyDossier.Mode},
	}
}

// applyRetention copies the `retention:` block into the settings the sweepers
// read, and turns the retention sweeper on when a family deletes. A mode the
// sweeper does not know is refused: read as off, it would keep data the
// operator asked to have deleted.
func applyRetention(cfg *Config) error {
	r := &cfg.Retention
	for _, m := range r.modes() {
		switch m.mode {
		case "", RetentionModeOff, RetentionModePrune, RetentionModeExportPrune:
		default:
			return fmt.Errorf("%s: %q is not off, prune or export+prune", m.key, m.mode)
		}
	}
	if r.Defs.KeepLastN != nil && *r.Defs.KeepLastN < 0 {
		return fmt.Errorf("retention.defs.keep_last_n: %d is negative (0 keeps no version)", *r.Defs.KeepLastN)
	}

	e := &cfg.Env
	e.RetentionInterval = r.Interval.Duration()
	e.RetentionExportDir = r.ExportDir
	// applyEnvCore set the defs defaults: mode "off", keep_last_n 5.
	if r.Defs.Mode != "" {
		e.RetentionDefsMode = r.Defs.Mode
	}
	e.RetentionDefsMaxAge = r.Defs.MaxAge.Duration()
	if r.Defs.KeepLastN != nil {
		e.RetentionDefsKeepLastN = *r.Defs.KeepLastN
	}
	e.RetentionChatsMode = r.Chats.Mode
	e.RetentionChatsMaxAge = r.Chats.MaxAge.Duration()
	e.RetentionMemMode = r.Mem.Mode
	e.RetentionMemMaxAge = r.Mem.MaxAge.Duration()
	e.RetentionMemContentMode = r.MemContent.Mode
	e.RetentionMemContentMaxAge = r.MemContent.MaxAge.Duration()
	e.RetentionEmptyDossierMode = r.EmptyDossier.Mode
	e.RetentionEmptyDossierMaxAge = r.EmptyDossier.MaxAge.Duration()
	e.UsageDetailRetention = r.UsageDetail.MaxAge.Duration()
	e.MemoryTraceMaxAge = r.Trace.MaxAge.Duration()
	e.RetentionEnabled = r.deletes()
	return nil
}

// deletes reports whether any family's mode asks for deletion, which is what
// starts the sweeper.
func (r *RetentionConfig) deletes() bool {
	for _, m := range r.modes() {
		if retentionModeDeletes(m.mode) {
			return true
		}
	}
	return false
}

func retentionModeDeletes(mode string) bool {
	return mode == RetentionModePrune || mode == RetentionModeExportPrune
}
