package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// HumanDuration is a duration written the way an operator states a retention
// age: a Go duration ("12h", "90m", "1h30m"), optionally led by a whole or
// fractional number of days ("30d", "1d12h"). "0" is zero. A bare number with
// no unit is refused: nothing says whether it meant seconds, hours or days.
// So is an empty string: zero is "any age" to a family that deletes, and only
// a written zero may mean it.
type HumanDuration time.Duration

// Duration is d as a time.Duration.
func (d HumanDuration) Duration() time.Duration { return time.Duration(d) }

// ParseHumanDuration parses s as described on HumanDuration. A negative
// duration is refused: no setting that takes one has a meaning for it.
func ParseHumanDuration(s string) (time.Duration, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		// A quoted "${VAR}" whose variable is unset or empty expands to this.
		return 0, errors.New("the value is empty (a ${VAR} that expanded to nothing reads this way); write a duration such as 30d or 12h, or 0s for zero")
	}
	if v == "0" {
		return 0, nil
	}
	rest := v
	var days float64
	// No Go duration unit contains a "d", so the first one ends the days part.
	if i := strings.IndexByte(v, 'd'); i >= 0 {
		n, err := strconv.ParseFloat(v[:i], 64)
		// ParseFloat reads "nan" and "inf" as numbers. NaN passes every range
		// check below and converts to zero.
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
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
// age, and when. The sweeper runs only when Enabled is true; each family is
// off until its mode says otherwise, and a mode alone starts nothing.
type RetentionConfig struct {
	// Enabled starts the retention sweeper. Off by default: it deletes data.
	Enabled bool `yaml:"enabled,omitempty"`
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
	// MaxAge is the age past which an item is eligible; 0 = any age. A family
	// whose mode deletes must set it, even to 0: with no minimum age the first
	// sweep deletes everything eligible, which has to be written down.
	MaxAge *HumanDuration `yaml:"max_age,omitempty"`
}

// RetentionDefs is the definitions family, which also keeps a number of
// versions whatever their age.
type RetentionDefs struct {
	Mode   string         `yaml:"mode,omitempty"`
	MaxAge *HumanDuration `yaml:"max_age,omitempty"`
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

// retentionFamilyView is one family as the load checks see it.
type retentionFamilyView struct {
	key    string // the yaml path of the family, e.g. retention.chats
	mode   string
	maxAge *HumanDuration
}

// families returns every family that has a mode.
func (r *RetentionConfig) families() []retentionFamilyView {
	return []retentionFamilyView{
		{"retention.defs", r.Defs.Mode, r.Defs.MaxAge},
		{"retention.chats", r.Chats.Mode, r.Chats.MaxAge},
		{"retention.mem", r.Mem.Mode, r.Mem.MaxAge},
		{"retention.mem_content", r.MemContent.Mode, r.MemContent.MaxAge},
		{"retention.empty_dossier", r.EmptyDossier.Mode, r.EmptyDossier.MaxAge},
	}
}

// age is a family's max_age, zero when the key is absent.
func age(d *HumanDuration) time.Duration {
	if d == nil {
		return 0
	}
	return d.Duration()
}

// applyRetention copies the `retention:` block into the settings the sweepers
// read. A mode the sweeper does not know is refused: read as off, it would
// keep data the operator asked to have deleted. So is a family that deletes
// with no max_age: "any age" deletes everything eligible on the first sweep,
// and has to be asked for with an explicit zero.
func applyRetention(cfg *Config) error {
	r := &cfg.Retention
	for _, f := range r.families() {
		switch f.mode {
		case "", RetentionModeOff, RetentionModePrune, RetentionModeExportPrune:
		default:
			return fmt.Errorf("%s.mode: %q is not off, prune or export+prune", f.key, f.mode)
		}
		if !retentionModeDeletes(f.mode) {
			continue
		}
		if f.maxAge == nil {
			return fmt.Errorf("%s.max_age: mode %s needs a max_age; with none, everything eligible is deleted on the first sweep (write max_age: 0s to mean exactly that)", f.key, f.mode)
		}
		if !r.Enabled {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("%s.mode is %s but retention.enabled is not true: the retention sweeper does not run and nothing is deleted", f.key, f.mode))
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
	e.RetentionDefsMaxAge = age(r.Defs.MaxAge)
	if r.Defs.KeepLastN != nil {
		e.RetentionDefsKeepLastN = *r.Defs.KeepLastN
	}
	e.RetentionChatsMode = r.Chats.Mode
	e.RetentionChatsMaxAge = age(r.Chats.MaxAge)
	e.RetentionMemMode = r.Mem.Mode
	e.RetentionMemMaxAge = age(r.Mem.MaxAge)
	e.RetentionMemContentMode = r.MemContent.Mode
	e.RetentionMemContentMaxAge = age(r.MemContent.MaxAge)
	e.RetentionEmptyDossierMode = r.EmptyDossier.Mode
	e.RetentionEmptyDossierMaxAge = age(r.EmptyDossier.MaxAge)
	e.UsageDetailRetention = r.UsageDetail.MaxAge.Duration()
	e.MemoryTraceMaxAge = r.Trace.MaxAge.Duration()
	e.RetentionEnabled = r.Enabled
	return nil
}

func retentionModeDeletes(mode string) bool {
	return mode == RetentionModePrune || mode == RetentionModeExportPrune
}
