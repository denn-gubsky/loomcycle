package config

import (
	"fmt"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"
)

// history_preload.go — the per-agent `history_preload:` block (RFC HE F3).

// History preload defaults and bounds. A zero field takes its default, so
// `history_preload: {}` turns the preload on with every default.
const (
	HistoryPreloadDefaultSessions  = 3
	HistoryPreloadMaxSessions      = 20
	HistoryPreloadDefaultWithin    = 14 * 24 * time.Hour
	HistoryPreloadDefaultTailTurns = 4
	HistoryPreloadMaxTailTurns     = 20
	HistoryPreloadDefaultMaxTokens = 1500
	HistoryPreloadMaxMaxTokens     = 8000

	// HistoryPreloadIncludeSummary lists each chat by its stored recap (its
	// title when it has none); HistoryPreloadIncludeSummaryTail also adds the
	// newest chat's last turns.
	HistoryPreloadIncludeSummary     = "summary"
	HistoryPreloadIncludeSummaryTail = "summary+tail"
)

// HistoryPreload is an agent's `history_preload:` block. Present (even empty)
// means on: the first run of a NEW top-level session of the agent starts with
// one fenced block listing the same user's latest chats with the same agent.
// Absent means off. Content-identifying (part of content_sha256). A run may
// only switch it off (`history_preload: "off"` on the run request).
type HistoryPreload struct {
	// Sessions is how many earlier chats to list, newest first (default 3, at
	// most 20).
	Sessions int `json:"sessions,omitempty" yaml:"sessions,omitempty"`
	// Within bounds how long ago a chat's last activity may be, written as a
	// duration such as 14d or 36h (default 14d).
	Within string `json:"within,omitempty" yaml:"within,omitempty"`
	// Include is summary (default) or summary+tail.
	Include string `json:"include,omitempty" yaml:"include,omitempty"`
	// TailTurns is how many of the newest chat's last turns summary+tail adds
	// (default 4, at most 20).
	TailTurns int `json:"tail_turns,omitempty" yaml:"tail_turns,omitempty"`
	// MaxTokens caps the whole block (default 1500, at most 8000), estimated
	// at four characters a token like the rest of the runtime's estimates.
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// Clone copies the block.
func (h *HistoryPreload) Clone() *HistoryPreload {
	if h == nil {
		return nil
	}
	out := *h
	return &out
}

// MergeHistoryPreload overlays over onto base per field: a set field in over
// wins, an unset one keeps base. Unlike the sibling blocks, an EMPTY over is
// not "nothing": `history_preload: {}` turns the preload on, so a non-nil over
// always yields a non-nil block. nil only when both are nil; never aliases.
func MergeHistoryPreload(base, over *HistoryPreload) *HistoryPreload {
	if over == nil {
		return base.Clone()
	}
	out := base.Clone()
	if out == nil {
		return over.Clone()
	}
	if over.Sessions != 0 {
		out.Sessions = over.Sessions
	}
	if over.Within != "" {
		out.Within = over.Within
	}
	if over.Include != "" {
		out.Include = over.Include
	}
	if over.TailTurns != 0 {
		out.TailTurns = over.TailTurns
	}
	if over.MaxTokens != 0 {
		out.MaxTokens = over.MaxTokens
	}
	return out
}

// Validate checks the block's own values.
func (h *HistoryPreload) Validate() error {
	if h == nil {
		return nil
	}
	if h.Sessions < 0 || h.Sessions > HistoryPreloadMaxSessions {
		return fmt.Errorf("history_preload.sessions must be between 1 and %d, got %d", HistoryPreloadMaxSessions, h.Sessions)
	}
	if h.Within != "" {
		d, err := ParseHumanDuration(h.Within)
		if err != nil {
			return fmt.Errorf("history_preload.within: %w", err)
		}
		if d <= 0 {
			return fmt.Errorf("history_preload.within must be longer than zero, got %q", h.Within)
		}
	}
	switch h.Include {
	case "", HistoryPreloadIncludeSummary, HistoryPreloadIncludeSummaryTail:
	default:
		return fmt.Errorf("history_preload.include must be %q or %q, got %q",
			HistoryPreloadIncludeSummary, HistoryPreloadIncludeSummaryTail, h.Include)
	}
	if h.TailTurns < 0 || h.TailTurns > HistoryPreloadMaxTailTurns {
		return fmt.Errorf("history_preload.tail_turns must be between 1 and %d, got %d", HistoryPreloadMaxTailTurns, h.TailTurns)
	}
	if h.MaxTokens < 0 || h.MaxTokens > HistoryPreloadMaxMaxTokens {
		return fmt.Errorf("history_preload.max_tokens must be between 1 and %d, got %d", HistoryPreloadMaxMaxTokens, h.MaxTokens)
	}
	return nil
}

// Effective returns the block with every default applied. Call on a validated
// block; an unparseable within falls back to its default.
func (h *HistoryPreload) Effective() HistoryPreload {
	out := HistoryPreload{}
	if h != nil {
		out = *h
	}
	if out.Sessions == 0 {
		out.Sessions = HistoryPreloadDefaultSessions
	}
	if out.Include == "" {
		out.Include = HistoryPreloadIncludeSummary
	}
	if out.TailTurns == 0 {
		out.TailTurns = HistoryPreloadDefaultTailTurns
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = HistoryPreloadDefaultMaxTokens
	}
	return out
}

// WithinDuration is the window as a duration, its default when unset.
func (h HistoryPreload) WithinDuration() time.Duration {
	if h.Within == "" {
		return HistoryPreloadDefaultWithin
	}
	d, err := ParseHumanDuration(h.Within)
	if err != nil || d <= 0 {
		return HistoryPreloadDefaultWithin
	}
	return d
}

func (h *HistoryPreload) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "history_preload", nil, reflect.TypeOf(*h)); err != nil {
		return err
	}
	type plain HistoryPreload
	if err := n.Decode((*plain)(h)); err != nil {
		return err
	}
	return h.Validate()
}
