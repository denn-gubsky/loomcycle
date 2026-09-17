package http

import (
	"encoding/json"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runConfigRecord is the run's OWN resolved configuration: the values the run
// actually started with, after per-run overrides were merged over the agent
// definition.
//
// It exists because resume rebuilt every one of these FROM THE DEFINITION. A
// long interactive chat that paused and resumed silently reverted its
// temperature, its compaction policy and its caller host narrowing
// mid-conversation, and nothing in the transcript said so. These are
// properties of the RUN, not of the definition it started from, so they are
// persisted with the run and restored on resume.
//
// Persisted as OPAQUE JSON in runs.run_config: internal/config imports
// internal/store, so the store cannot name these types — and deliberately does
// not interpret the record. It stores bytes.
type runConfigRecord struct {
	Sampling          *config.Sampling   `json:"sampling,omitempty"`
	Compaction        *config.Compaction `json:"compaction,omitempty"`
	Context           *config.Context    `json:"context,omitempty"`
	MaxContextTokens  int                `json:"max_context_tokens,omitempty"`
	RunTimeoutSeconds int                `json:"run_timeout_seconds,omitempty"`

	// Routing is the run's own answer to which model serves it (RFC DC P1).
	// It lives here rather than beside it on the run row for the same reason
	// everything else here does: it must survive a pause, and resume must
	// restore it instead of re-deriving from a definition that may have moved.
	Routing *routingOverride `json:"routing,omitempty"`

	// Resources is the run's own budget (RFC DC P2) — here for the same reason
	// Routing is: it must survive a pause, and a resumed run must not quietly
	// go back to the definition's limits.
	Resources *resourceOverride `json:"resources,omitempty"`

	// Hosts is the caller-authoritative host narrowing. Restoring it makes a
	// resumed run no WIDER than the original: without it the run came back on
	// the bare operator floor, the one case where losing an override weakened
	// a boundary rather than merely changing behaviour.
	Hosts *runHostRecord `json:"hosts,omitempty"`
}

// runHostRecord mirrors tools.HostPolicyValue. HasList is carried explicitly
// because an EMPTY caller list means "allow nothing", which is not the same as
// "the caller sent no list at all" — a distinction the zero value erases.
type runHostRecord struct {
	AllowedHosts    []string `json:"allowed_hosts,omitempty"`
	HasList         bool     `json:"has_list,omitempty"`
	WebSearchFilter string   `json:"web_search_filter,omitempty"`
}

// hostRecordOf captures a run's host policy, or nil when the caller narrowed
// nothing (so an unnarrowed run's record stays absent rather than carrying an
// empty object).
func hostRecordOf(p tools.HostPolicyValue) *runHostRecord {
	if !p.HasList && p.WebSearchFilter == "" && len(p.AllowedHosts) == 0 {
		return nil
	}
	return &runHostRecord{
		AllowedHosts:    p.AllowedHosts,
		HasList:         p.HasList,
		WebSearchFilter: p.WebSearchFilter,
	}
}

// callerHosts returns the caller's list in the nil-vs-empty shape NarrowHosts
// reads: nil means "the caller sent no list", while a non-nil EMPTY list means
// "allow nothing". The JSON round trip erases that difference — omitempty drops
// an empty slice — which is the whole reason HasList is carried.
func (rc runConfigRecord) callerHosts() []string {
	if rc.Hosts == nil || !rc.Hosts.HasList {
		return nil
	}
	if rc.Hosts.AllowedHosts == nil {
		return []string{}
	}
	return rc.Hosts.AllowedHosts
}

// hostPolicy rebuilds the ctx value a resumed run needs, with the caller's list
// in the same nil-vs-empty shape the original run put on ctx.
func (rc runConfigRecord) hostPolicy() tools.HostPolicyValue {
	if rc.Hosts == nil {
		return tools.HostPolicyValue{}
	}
	return tools.HostPolicyValue{
		AllowedHosts:    rc.callerHosts(),
		HasList:         rc.Hosts.HasList,
		WebSearchFilter: rc.Hosts.WebSearchFilter,
	}
}

// marshal encodes the record for the runs.run_config column. A marshal failure
// degrades to "no record": a run must never fail to START because its fidelity
// record could not be encoded, and an absent record resumes exactly the way
// every pre-existing run does.
func (rc runConfigRecord) marshal() json.RawMessage {
	b, err := json.Marshal(rc)
	if err != nil {
		log.Printf("run_config: marshal failed; this run will resume from its definition: %v", err)
		return nil
	}
	return b
}

// decodeRunConfig reads a persisted record. A run started before the column
// existed — or one whose record is corrupt — returns ok=false, and the caller
// re-derives from the definition, which is the behaviour every run had before
// the record existed.
func decodeRunConfig(raw json.RawMessage) (runConfigRecord, bool) {
	if len(raw) == 0 {
		return runConfigRecord{}, false
	}
	var rc runConfigRecord
	if err := json.Unmarshal(raw, &rc); err != nil {
		log.Printf("run_config: decode failed; resuming from the agent definition instead: %v", err)
		return runConfigRecord{}, false
	}
	return rc, true
}
