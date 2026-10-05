package http

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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
	Sampling          *config.Sampling     `json:"sampling,omitempty"`
	ToolChoice        *config.ToolChoice   `json:"tool_choice,omitempty"`   // RFC DI
	OutputFormat      *config.OutputFormat `json:"output_format,omitempty"` // RFC DI
	Compaction        *config.Compaction   `json:"compaction,omitempty"`
	Context           *config.Context      `json:"context,omitempty"`
	MaxContextTokens  int                  `json:"max_context_tokens,omitempty"`
	RunTimeoutSeconds int                  `json:"run_timeout_seconds,omitempty"`

	// Routing is the run's own answer to which model serves it (RFC DC P1).
	// It lives here rather than beside it on the run row for the same reason
	// everything else here does: it must survive a pause, and resume must
	// restore it instead of re-deriving from a definition that may have moved.
	Routing *routingOverride `json:"routing,omitempty"`

	// Resources is the run's own budget (RFC DC P2) — here for the same reason
	// Routing is: it must survive a pause, and a resumed run must not quietly
	// go back to the definition's limits.
	Resources *resourceOverride `json:"resources,omitempty"`

	// Tuning is the run's own shaping (RFC DC §4's tuning row), here for the
	// same reason its siblings are: it must survive a pause.
	Tuning *tuningOverride `json:"tuning,omitempty"`

	// Interactive PROMOTES a run to parking at its turn boundaries instead of
	// finishing — set while the run is already going, so an operator can take
	// hold of an agent mid-flight and correct it.
	//
	// A POINTER because the three states differ: absent means "the run keeps
	// whatever it started as", true promotes, and false demotes a run that was
	// started interactive so it finishes at its next boundary. A bool could not
	// express the first, and "never retuned" is the common case.
	//
	// It lives here rather than only on the runs.interactive column because the
	// column is the run's ORIGINAL shape and this is its current one; keeping
	// them apart is what lets a resume restore a promoted run as promoted.
	Interactive *bool `json:"interactive,omitempty"`

	// Review holds the run for an operator's verdict when its model finishes.
	// Unlike Interactive there is no column for the start-time answer, so the
	// record carries it from the start: true when the run began armed, and
	// whatever a retune set since. Absent means never armed.
	Review *bool `json:"review,omitempty"`

	// ReviewTTLSeconds is the run's review deadline, kept so a restored hold
	// expires when it would have, not a full window after the restart.
	ReviewTTLSeconds int `json:"review_ttl_seconds,omitempty"`

	// Interruption is the run's own answer to whether the agent may ASK a human
	// a question. It NARROWS the definition's policy (interruptionPolicyForRun)
	// and never grants the tool. Recorded at start so a resume re-narrows from
	// it, and by a retune, which a parked run adopts at its next operator turn.
	//
	// The definition's classification used to be notOverridable, filed under
	// "reach". That was wrong: an interruption touches no data and no host — it
	// blocks and waits for a person. The real exposure is LIVENESS, which puts
	// it beside unbounded_iterations rather than beside memory_scopes, and which
	// run_timeout_seconds and the interruption's own timeout already bound.
	Interruption *config.AgentInterruptionACL `json:"interruption,omitempty"`

	// Hosts is the caller-authoritative host narrowing. Restoring it makes a
	// resumed run no WIDER than the original: without it the run came back on
	// the bare operator floor, the one case where losing an override weakened
	// a boundary rather than merely changing behaviour.
	Hosts *runHostRecord `json:"hosts,omitempty"`

	// Hooks are the hooks the run added to its agent's — its request's, and a
	// sub-agent's inherited ones — so a resumed run fires what it fired before.
	Hooks *hooks.Additions `json:"hooks,omitempty"`

	// SourcedHooks are the hooks a definition added to the run — a TeamDef
	// state's — each with the definition it came from, so a resumed run
	// resolves them in that definition's tenant and with its authorship, as it
	// did at start. Kept apart from Hooks because only the runtime writes this
	// record: no request shape can carry a source (hooks.Additions.Sourced).
	SourcedHooks []hooks.SourcedAdditions `json:"sourced_hooks,omitempty"`

	// PinnedHooks is what the run's hooks resolved to when it started, so a
	// resumed run fires those and no others (see pinnedHooks).
	PinnedHooks *pinnedHooks `json:"pinned_hooks,omitempty"`

	// Spawn is what bounded a SUB-run at its spawn beyond its own definition:
	// the parent's volume confinement and the fan-out width it inherited. A
	// live child reads both off its parent's context; a resumed child has no
	// parent context, so without this record it came back on its definition's
	// own volumes and width — wider than it ran live. Absent on a top-level
	// run, and on a sub-run recorded before it existed (see resumedVolumePolicy).
	Spawn *spawnRecord `json:"spawn,omitempty"`

	// AgentVersion is the definition the run's agent NAME resolved to when it
	// started, so a resume continues on that version rather than on whatever
	// the name resolves to by then — a newer version may offer wider tools or a
	// different prompt. Absent on a run recorded before it existed, which
	// resumes by name as every run did before (see resumedAgentDef).
	//
	// Not runs.agent_def_id: that column is the version a parent PINNED when
	// it spawned the run by def_id, which the Evaluation tool attributes scores
	// to. A pinned sub-run's definition is this version with the pinned one laid
	// over it, so resume needs both.
	AgentVersion *agentVersionRecord `json:"agent_version,omitempty"`

	// Team is what a team walk's own run started with: the TeamDef version
	// its name or def_id resolved to, its input and its run arguments. Present
	// only on a walk's run, written once when the row is created and never
	// changed — a walk runs no loop, so nothing retunes or resumes it. Absent
	// on a walk recorded before it existed: its version was not recorded and
	// is not inferred.
	//
	// Not runs.agent_def_id, for the reason AgentVersion is not: that column
	// names an AgentDef version, and the Evaluation tool reads it as one.
	Team *teamWalkRecord `json:"team,omitempty"`

	// TeamScope is the team this run belongs to: set on every run in a walk's
	// spawn tree — its members, their sub-agents, and theirs — and on a
	// continuation of one. A resumed or continued run reads the team's own
	// agents from the version named here, whatever the team is by then.
	// Absent on a run outside every team, and on the walk's own run, which
	// names its team in Team and resolves no agent.
	//
	// Here rather than in parent_context, which a caller supplies: this names
	// definitions a run may execute, so only the runtime may write it.
	TeamScope *teamScopeRecord `json:"team_scope,omitempty"`
}

// teamScopeRecord is store.TeamScope as a run records it.
type teamScopeRecord struct {
	Team      string `json:"team"`
	DefID     string `json:"def_id"`
	DefTenant string `json:"def_tenant,omitempty"`
}

// teamScopeRecordOf records the team scope on ctx; nil outside every team.
func teamScopeRecordOf(ctx context.Context) *teamScopeRecord {
	sc, ok := store.TeamScopeFromContext(ctx)
	if !ok {
		return nil
	}
	return &teamScopeRecord{Team: sc.Team, DefID: sc.DefID, DefTenant: sc.Tenant}
}

// withTeamScope returns ctx carrying the team scope the run recorded — or
// carrying NONE when it recorded none, whatever ctx held. A run's scope is its
// own record's, never the scope of whoever is acting on it.
func (rc runConfigRecord) withTeamScope(ctx context.Context) context.Context {
	if rc.TeamScope == nil {
		return store.WithTeamScope(ctx, store.TeamScope{})
	}
	return store.WithTeamScope(ctx, store.TeamScope{
		Tenant: rc.TeamScope.DefTenant, Team: rc.TeamScope.Team, DefID: rc.TeamScope.DefID,
	})
}

// teamWalkRecord is a walk's start, as its run records it. Nothing in it is
// secret: the input and the supplied variable values are masked before they
// are stored, and the input bounded (teamWalkRecordOf).
// No copy of the definition body is kept; content_sha256 still identifies it
// after the row is deleted.
type teamWalkRecord struct {
	Name          string `json:"name"`
	DefID         string `json:"def_id"`
	Version       int    `json:"version"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	DefTenant     string `json:"def_tenant,omitempty"`
	// ResolvedBy is "def_id" (the caller pinned a version) or "name" (it
	// took the active one).
	ResolvedBy string `json:"resolved_by"`
	// Input is the walk's input, masked and cut at teamWalkInputCap bytes.
	// InputBytes is the length of the input as given, so a reader can tell
	// how much a truncated input lost.
	Input          string         `json:"input,omitempty"`
	InputBytes     int            `json:"input_bytes,omitempty"`
	InputTruncated bool           `json:"input_truncated,omitempty"`
	Mode           string         `json:"mode"`
	Board          *teamWalkBoard `json:"board,omitempty"`
	Breakpoints    []string       `json:"breakpoints,omitempty"`
	Review         []string       `json:"review,omitempty"`
	ReviewTTL      int            `json:"review_ttl_seconds,omitempty"`
	InterruptOnCap bool           `json:"interrupt_on_cap,omitempty"`

	// Vars are the variable values the caller supplied at start, each masked
	// like the input. Only those: a default is in the definition version this
	// record names, so a reader tells "given" from "defaulted" by what is
	// here. Not cut — a start refuses a value over 4096 bytes and a name the
	// team does not declare, and a team declares at most 64. Absent when the
	// start supplied none, so such a record is the bytes it always was.
	Vars map[string]string `json:"vars,omitempty"`
}

// teamWalkBoard is the walk's board binding at start. Breakpoints and review
// above are the start seed only: arming a live walk changes an in-memory set,
// not this record.
type teamWalkBoard struct {
	Scope       string `json:"scope"`
	ChunkID     string `json:"chunk_id"`
	ResumedFrom string `json:"resumed_from,omitempty"`
}

// teamWalkInputCap bounds the walk input a run record keeps. Walk input is
// usually a task line; where a member received the full text it stays in that
// member's own transcript.
const teamWalkInputCap = 16 << 10

// teamWalkRecordOf records spec, masking its input with r before cutting it,
// so a secret in a walk's input is not put at rest unmasked on the run row,
// and a cut never splits the mask. The supplied variable values are masked by
// the same r: a caller can put anything in one, so they are held to what the
// input is and are never more readable than it.
func teamWalkRecordOf(r *redact.Redactor, spec builtin.WalkRunSpec) *teamWalkRecord {
	mode := "sync"
	if spec.Detach {
		mode = "detach"
	}
	input := r.String(spec.Input)
	truncated := false
	if len(input) > teamWalkInputCap {
		input, truncated = cutUTF8(input, teamWalkInputCap), true
	}
	rec := &teamWalkRecord{
		Name:           spec.Name,
		DefID:          spec.DefID,
		Version:        spec.Version,
		ContentSHA256:  spec.ContentSHA256,
		DefTenant:      spec.DefTenant,
		ResolvedBy:     spec.ResolvedBy,
		Input:          input,
		InputBytes:     len(spec.Input),
		InputTruncated: truncated,
		Mode:           mode,
		Breakpoints:    spec.Breakpoints,
		Review:         spec.Review,
		ReviewTTL:      spec.ReviewTTLSeconds,
		InterruptOnCap: spec.InterruptOnCap,
	}
	if len(spec.Vars) > 0 {
		rec.Vars = make(map[string]string, len(spec.Vars))
		for name, value := range spec.Vars {
			rec.Vars[name] = r.String(value)
		}
	}
	if spec.Board != nil {
		rec.Board = &teamWalkBoard{Scope: spec.Board.Scope, ChunkID: spec.Board.ChunkID, ResumedFrom: spec.Board.ResumedFrom}
	}
	return rec
}

// cutUTF8 returns at most n bytes of s, ending on a rune boundary so the cut
// never leaves half a character that JSON would turn into U+FFFD.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// agentVersionRecord names the version a run started on. DefID "" means the
// name resolved to a definition with no versions: the operator's yaml, or a
// registered agent. Present-but-empty is how resume tells that apart from a
// run recorded before versions were.
//
// RegisteredSHA256 stands in for a version when the definition was a
// registered agent, whose row is rewritten in place: the digest of the row the
// run started on. "" for every other source, and on a run recorded before it
// existed, which resumes by name as before.
//
// Static marks a run that started on the operator's static configuration
// (yaml, presets, bundles), which a registered agent or a tenant's AgentDef of
// the same name can shadow later; a resume reads the static definition again.
// false for every other source, and on a run recorded before it existed, which
// resumes by name as before.
//
// TeamDefID marks a run of a team's OWN agent, with the team version it was
// read from. A resume reads it from that version and nowhere else — never from
// a global agent that has the same full name by then. "" for every other
// source.
type agentVersionRecord struct {
	DefID            string `json:"def_id,omitempty"`
	RegisteredSHA256 string `json:"registered_sha256,omitempty"`
	Static           bool   `json:"static,omitempty"`
	TeamDefID        string `json:"team_def_id,omitempty"`
}

// agentVersionOf records the version def was read from.
func agentVersionOf(def config.AgentDef) *agentVersionRecord {
	return &agentVersionRecord{DefID: def.DefID, RegisteredSHA256: def.RegisteredSHA256, Static: def.Static, TeamDefID: def.TeamDefID}
}

// spawnRecord is a sub-run's inherited ceiling, captured from the parent's
// context at spawn.
type spawnRecord struct {
	Volumes volumeCeilingRecord `json:"volumes"`
	// FanoutCap is the parallel_spawn width the child inherited on ctx; 0 when
	// its ancestors set none and its definition decides.
	FanoutCap int `json:"fanout_cap,omitempty"`
}

// volumeCeilingRecord mirrors tools.VolumePolicyValue minus each binding's
// Root. The root is deliberately NOT recorded: a record travels in snapshots
// to other instances, and a path carried from one host's config must never
// become a binding on another's. Resume resolves each name again, now, the
// way a fresh run would (see recordedParentVolumes).
type volumeCeilingRecord struct {
	Active   bool                   `json:"active"`
	Bindings []volumeCeilingBinding `json:"bindings,omitempty"`
}

type volumeCeilingBinding struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
	Default  bool   `json:"default,omitempty"`
}

// spawnRecordOf captures a child's ceiling from its parent's volume policy and
// the fan-out cap on the parent's ctx. Always non-nil: its presence is what
// tells resume the child's ceiling is known, including "the parent was not
// confined by volumes" (Active false), which a nil record cannot say.
func spawnRecordOf(parentVol tools.VolumePolicyValue, fanoutCap int) *spawnRecord {
	rec := &spawnRecord{
		Volumes:   volumeCeilingRecord{Active: parentVol.Active},
		FanoutCap: fanoutCap,
	}
	for _, b := range parentVol.Bindings {
		rec.Volumes.Bindings = append(rec.Volumes.Bindings, volumeCeilingBinding{
			Name: b.Name, ReadOnly: b.ReadOnly, Default: b.Default,
		})
	}
	return rec
}

// additions is what the run added to its agent's hooks, as the record keeps
// it: a caller's, and a definition's with their source.
func (rc runConfigRecord) additions() hooks.Additions {
	var a hooks.Additions
	if rc.Hooks != nil {
		a.Hooks, a.ToolHooks = rc.Hooks.Hooks, rc.Hooks.ToolHooks
	}
	a.Sourced = rc.SourcedHooks
	return a
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

// runConfigWriteAttempts bounds updateRunConfig's retries. A run's record has a
// handful of writers, each writing once per operator action or arming change,
// so losing this many reads in a row means something is writing in a loop.
const runConfigWriteAttempts = 8

// errRunConfigContended is updateRunConfig giving up after
// runConfigWriteAttempts refused writes.
var errRunConfigContended = errors.New("run config: another writer kept changing it; not written")

// errRunConfigUnreadable is a change refusing to overwrite a stored record
// that does not decode.
var errRunConfigUnreadable = errors.New("run config: stored record does not decode; not overwritten")

// updateRunConfig applies change to the run's stored record and writes the
// result back only over the record it read, re-reading and re-applying when
// another writer got there first. It returns the record as written.
//
// Every writer that edits a field of a live run's record goes through here.
// Each used to read the record, set its field and replace the column, so two
// of them interleaving (a retune landing as a team member's review arming
// flips) wrote back two versions of the same read, and the later one erased
// the other's field. change must therefore be a pure edit of rec: it may run
// more than once. unreadable is a stored record that does not decode; change
// decides whether to overwrite it, and any error it returns aborts the write.
func (s *Server) updateRunConfig(ctx context.Context, runID string, change func(rec *runConfigRecord, unreadable bool) error) (runConfigRecord, error) {
	for attempt := 0; attempt < runConfigWriteAttempts; attempt++ {
		run, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return runConfigRecord{}, err
		}
		rec, ok := decodeRunConfig(run.RunConfig)
		if err := change(&rec, !ok && len(run.RunConfig) > 0); err != nil {
			return runConfigRecord{}, err
		}
		written, err := s.store.SetRunConfigCAS(ctx, runID, run.RunConfig, rec.marshal())
		if err != nil {
			return runConfigRecord{}, err
		}
		if written {
			return rec, nil
		}
	}
	return runConfigRecord{}, errRunConfigContended
}

// toolChoiceSpent reports whether a paused run already used up its tool_choice
// before it paused (RFC DI), read from the run's OWN events.
//
// Resume re-enters the loop, and the loop starts each entry with a fresh
// policy, so without this a resumed run would force its first call a second
// time — after the run's actual first call happened hours earlier. A session
// continuation is unaffected: it is a new run with no events of its own yet.
//
// A store read failure counts as not spent: re-forcing once is a smaller harm
// than dropping a choice the run never got to use.
//
// A RETUNE starts the count over. tc is the run's current choice, and the
// operator override event naming tool_choice (written when the retune was made)
// means the calls before it spent a DIFFERENT choice. Known imprecision: a run
// retuned while mid-turn keeps its old choice until its next operator turn, so
// that turn's call — completing after the marker — is counted against the new
// choice if the run is paused before that turn.
func toolChoiceSpent(ctx context.Context, st store.Store, runID string, tc *config.ToolChoice) bool {
	if tc.IsZero() || st == nil || tc.EffectiveUntil() == config.ToolChoiceUntilAlways {
		return false
	}
	const page = 500
	var after int64
	spent := false
	for {
		evs, err := st.GetRunEventsSince(ctx, runID, after, page)
		if err != nil {
			return false
		}
		for _, ev := range evs {
			after = ev.Seq
			switch {
			case ev.Type == string(providers.EventOverride):
				var pe providers.Event
				if json.Unmarshal(ev.Payload, &pe) == nil && pe.Override != nil &&
					slices.Contains(pe.Override.Fields, "tool_choice") {
					spent = false
				}
			case spent:
			case tc.EffectiveUntil() == config.ToolChoiceUntilFirstCall && ev.Type == string(providers.EventUsage):
				spent = true // a model call completed
			case tc.EffectiveUntil() == config.ToolChoiceUntilUntilCalled && ev.Type == string(providers.EventToolCall):
				var pe providers.Event
				if json.Unmarshal(ev.Payload, &pe) == nil && pe.ToolUse != nil &&
					(tc.Mode == config.ToolChoiceModeRequired || pe.ToolUse.Name == tc.Name) {
					spent = true
				}
			}
		}
		if len(evs) < page {
			return spent
		}
	}
}

// reviewRecord is the record's Review for a run that starts armed or not: a
// run that was never armed leaves the field absent, so its record is
// byte-identical to one written before review existed.
func reviewRecord(armed bool) *bool {
	if !armed {
		return nil
	}
	return &armed
}

// reviewTTL is the run's review deadline as a duration (0 = none).
func (rc runConfigRecord) reviewTTL() time.Duration {
	return time.Duration(rc.ReviewTTLSeconds) * time.Second
}

// positiveOrZero treats a non-positive count as unset, the way
// run_timeout_seconds does.
func positiveOrZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
