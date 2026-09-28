package config

import "fmt"

// MemoryRerank is an agent's opt-in to the search rerank: the `memory_rerank`
// block on an agent (yaml, and the AgentDef create/fork overlay).
//
// PER AGENT, AND ONLY PER AGENT. The rerank costs a model call per search (about
// 6,000 prompt tokens), so the decision belongs to whoever owns the agent's cost —
// there is deliberately no tool parameter, and no per-run override: a model could
// not be trusted to turn it off when it helps, or on only when it pays.
//
// What serves it is the operator's memory.reranker. Enabled with no reranker
// declared, a search keeps its own order and reports `not_configured`.
type MemoryRerank struct {
	// Enabled turns the rerank on. A pointer so a fork overlay that only changes
	// `candidates` keeps its parent's switch.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled"`
	// Candidates is how many of the search pool the model is shown. 0 = 20.
	Candidates int `json:"candidates,omitempty" yaml:"candidates"`
	// MaxChars truncates each candidate to this many characters. 0 = 1,200.
	MaxChars int `json:"max_chars,omitempty" yaml:"max_chars"`
}

// Bounds. The pool the rerank reorders holds at most 51 rows; a candidate below
// 200 characters is its header and little else.
const (
	memoryRerankMaxCandidates = 50
	memoryRerankMinMaxChars   = 200
	memoryRerankMaxMaxChars   = 20000
)

// On reports whether the rerank is enabled.
func (m *MemoryRerank) On() bool { return m != nil && m.Enabled != nil && *m.Enabled }

// IsZero reports whether nothing is set, so an empty block collapses to nil and
// an agent that never mentions it keeps a byte-stable content hash.
func (m *MemoryRerank) IsZero() bool {
	return m == nil || (m.Enabled == nil && m.Candidates == 0 && m.MaxChars == 0)
}

// MergeMemoryRerank overlays `over` onto `base` per field, as a fork overlay
// does: a set field wins, an unset one keeps the base's. Never aliases either.
func MergeMemoryRerank(base, over *MemoryRerank) *MemoryRerank {
	if base.IsZero() && over.IsZero() {
		return nil
	}
	out := &MemoryRerank{}
	for _, m := range []*MemoryRerank{base, over} {
		if m == nil {
			continue
		}
		if m.Enabled != nil {
			v := *m.Enabled
			out.Enabled = &v
		}
		if m.Candidates != 0 {
			out.Candidates = m.Candidates
		}
		if m.MaxChars != 0 {
			out.MaxChars = m.MaxChars
		}
	}
	return out
}

// Validate checks the bounds, naming the offending field.
func (m *MemoryRerank) Validate() error {
	if m == nil {
		return nil
	}
	if m.Candidates != 0 && (m.Candidates < 2 || m.Candidates > memoryRerankMaxCandidates) {
		return fmt.Errorf("memory_rerank.candidates %d out of range [2,%d]", m.Candidates, memoryRerankMaxCandidates)
	}
	if m.MaxChars != 0 && (m.MaxChars < memoryRerankMinMaxChars || m.MaxChars > memoryRerankMaxMaxChars) {
		return fmt.Errorf("memory_rerank.max_chars %d out of range [%d,%d]", m.MaxChars, memoryRerankMinMaxChars, memoryRerankMaxMaxChars)
	}
	return nil
}
