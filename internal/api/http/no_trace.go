package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// No-trace runs.
//
// A run whose retention mode is store.RetentionNone delivers its content to
// whoever is waiting for it and stores none of it. The caller on the
// connection still gets the full stream, and a parent still gets its child's
// answer: both come from memory. What reaches the store is metadata — that an
// event happened, when, how large it was — and nothing a person wrote or a
// model said.
//
// It works by filtering at the store boundary, in traceFilterStore, and not
// at the places that write. There are sixteen of those for events alone, in
// eleven paths, and a writer was missed here once already. A filter every
// write has to pass cannot be forgotten by the next writer.
//
// The mode comes from the request, the agent's definition, the caller's
// token or the parent run (resolveRetention).

// runModes answers "is this run no-trace" for the store filter.
//
// Runs this replica created are known from the moment their row is written.
// Anything else — a run of another replica, an event that lands after a
// restart — is read from the row once and remembered: the mode never changes
// after the row is written.
type runModes struct {
	mu    sync.RWMutex
	known map[string]bool // run id -> no-trace
}

// runModesMax bounds the map. A full map is dropped whole; what it held is
// read from the row again on its next write.
const runModesMax = 1 << 17

func newRunModes() *runModes { return &runModes{known: make(map[string]bool)} }

func (m *runModes) note(runID string, noTrace bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.known) >= runModesMax {
		m.known = make(map[string]bool)
	}
	m.known[runID] = noTrace
}

// noTrace reports whether runID's content must not be stored.
//
// It FAILS CLOSED: a row that cannot be read is treated as no-trace, and the
// answer is not remembered. A marker written for a run that was in fact
// ordinary loses a transcript line; content written for a run that was in
// fact no-trace cannot be taken back.
func (m *runModes) noTrace(ctx context.Context, st store.Store, runID string) bool {
	m.mu.RLock()
	v, ok := m.known[runID]
	m.mu.RUnlock()
	if ok {
		return v
	}
	// The write may be happening on a ctx already cancelled (a run's last
	// events often are); the lookup must not fail for that reason.
	run, err := st.GetRun(context.WithoutCancel(ctx), runID)
	if err != nil {
		log.Printf("no-trace: could not read the retention mode of run %s (%v); storing a marker", runID, err)
		return true
	}
	none := run.Retention == store.RetentionNone
	m.note(runID, none)
	return none
}

// traceFilterStore is the Server's store handle. It is the store, with the
// three writes that carry a run's content filtered for a no-trace run.
type traceFilterStore struct {
	store.Store
	modes *runModes
}

func newTraceFilterStore(st store.Store) *traceFilterStore {
	return &traceFilterStore{Store: st, modes: newRunModes()}
}

// CapturesChanges forwards the one optional interface the Server asks its
// store for (changeFeedCapturer). An embedded interface does not carry the
// methods of what it holds.
func (t *traceFilterStore) CapturesChanges() bool {
	c, ok := t.Store.(changeFeedCapturer)
	return ok && c.CapturesChanges()
}

// noTraceRunConfig is the configuration record of a no-trace run: its mode.
// The settings a run records are there to resume it, and a no-trace run is
// never resumed; some of them (a hook's literal header, a walk's input) are
// content.
var noTraceRunConfig = json.RawMessage(`{"retention":"none"}`)

// SetRunConfigCAS keeps a no-trace run's record at its mode, whatever a
// writer meant to add.
func (t *traceFilterStore) SetRunConfigCAS(ctx context.Context, runID string, prev, next json.RawMessage) (bool, error) {
	if t.modes.noTrace(ctx, t.Store, runID) {
		next = noTraceRunConfig
	}
	return t.Store.SetRunConfigCAS(ctx, runID, prev, next)
}

// CreateRun records the new run's mode, so its first event needs no lookup.
func (t *traceFilterStore) CreateRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	if identity.Retention == store.RetentionNone {
		identity.RunConfig = noTraceRunConfig
	}
	run, err := t.Store.CreateRun(ctx, sessionID, identity)
	if err == nil {
		t.modes.note(run.ID, identity.Retention == store.RetentionNone)
	}
	return run, err
}

// CreateQueuedRun is CreateRun for a run that waits for its slot.
func (t *traceFilterStore) CreateQueuedRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	if identity.Retention == store.RetentionNone {
		identity.RunConfig = noTraceRunConfig
	}
	run, err := t.Store.CreateQueuedRun(ctx, sessionID, identity)
	if err == nil {
		t.modes.note(run.ID, identity.Retention == store.RetentionNone)
	}
	return run, err
}

// StartQueuedRun writes the settings a queued run resolved after its wait;
// a no-trace run's record stays its mode. The row's mode counts as well as
// the identity's: it was fixed when the run was queued.
func (t *traceFilterStore) StartQueuedRun(ctx context.Context, runID string, identity store.RunIdentity) (store.Run, error) {
	if identity.Retention == store.RetentionNone || t.modes.noTrace(ctx, t.Store, runID) {
		identity.RunConfig = noTraceRunConfig
	}
	return t.Store.StartQueuedRun(ctx, runID, identity)
}

// noTraceQueuedInput is a no-trace queued run's stored request: none, and
// why a restart cannot start it again.
var noTraceQueuedInput = func() json.RawMessage {
	b, _ := json.Marshal(queuedRecord{Lost: lostNoTrace})
	return b
}()

// SetQueuedInput stores what a restart needs to start a queued run again. A
// no-trace run's request — its prompt — is not stored.
func (t *traceFilterStore) SetQueuedInput(ctx context.Context, runID string, input json.RawMessage) error {
	if t.modes.noTrace(ctx, t.Store, runID) {
		input = noTraceQueuedInput
	}
	return t.Store.SetQueuedInput(ctx, runID, input)
}

// AppendEvent stores a no-trace run's event as a marker, and an ordinary
// run's record of a no-trace child's result without that result.
func (t *traceFilterStore) AppendEvent(ctx context.Context, runID string, eventType string, payload []byte) error {
	if t.modes.noTrace(ctx, t.Store, runID) {
		payload = eventMarker(eventType, payload)
	} else if eventType == string(providers.EventSpawnChildResult) {
		payload = t.withoutNoTraceChildResult(ctx, payload)
	}
	return t.Store.AppendEvent(ctx, runID, eventType, payload)
}

// notRetainedChildOutput is what a parent's record keeps of a no-trace
// child's answer. A resumed parent reads it in the answer's place.
const notRetainedChildOutput = "(not retained: this sub-agent's answer was delivered and not stored)"

// withoutNoTraceChildResult reduces a spawn_child_result row — the copy of a
// child's answer a parent keeps to be resumed with — when the child is a
// no-trace run: the answer, the error's text and the structured result go,
// how it ended stays.
func (t *traceFilterStore) withoutNoTraceChildResult(ctx context.Context, payload []byte) []byte {
	var ev providers.Event
	if json.Unmarshal(payload, &ev) != nil {
		// Every such row is written by json.Marshal; one that does not parse
		// cannot be shown to be free of a no-trace child's answer.
		return eventMarker(string(providers.EventSpawnChildResult), payload)
	}
	sc := ev.SpawnChild
	if sc == nil {
		return eventMarker(string(providers.EventSpawnChildResult), payload)
	}
	// A child that cannot be shown to be ordinary — no run named, a row that
	// cannot be read or found — is treated as no-trace (runModes.noTrace
	// fails closed). The cost is a refused child's error text, which its
	// parent's model already read.
	if sc.RunID != "" && !t.modes.noTrace(ctx, t.Store, sc.RunID) {
		return payload
	}
	reduced := *sc
	if reduced.Output != "" {
		reduced.Output = notRetainedChildOutput
	}
	reduced.Error = safeErrorText(reduced.Error)
	reduced.State, reduced.Detail = nil, nil
	ev.SpawnChild = &reduced
	b, err := json.Marshal(ev)
	if err != nil {
		return eventMarker(string(providers.EventSpawnChildResult), payload)
	}
	return b
}

// FinishRun stores a no-trace run's end without its answer or its error text.
func (t *traceFilterStore) FinishRun(ctx context.Context, runID string, status store.RunStatus, stopReason string, usage store.Usage, errMsg string) error {
	if t.modes.noTrace(ctx, t.Store, runID) {
		stopReason = safeStopReason(stopReason)
		usage.Result = notRetainedResult(usage.Result)
		errMsg = safeErrorText(errMsg)
	}
	return t.Store.FinishRun(ctx, runID, status, stopReason, usage, errMsg)
}

// eventMarkerRecord is what is stored in place of a no-trace run's event.
//
// Every field is one that cannot hold content: sizes, ids, names of tools and
// agents, token counts. It keeps the shape of a providers.Event for the
// fields it has, so the readers that parse stored events — the transcript
// view, the awaited-state derivation — read a marker as an event with
// nothing in it.
type eventMarkerRecord struct {
	Type string `json:"type,omitempty"`
	// Retained is always false. It is what tells a reader this row is a
	// marker and not an event that happened to be empty.
	Retained bool `json:"retained"`
	// Bytes is the size of the payload that was not stored.
	Bytes int `json:"bytes"`
	// Tool is the tool an event concerns, when it concerns one.
	Tool       string            `json:"tool,omitempty"`
	ToolUse    *markerToolUse    `json:"tool_use,omitempty"`
	IsError    bool              `json:"is_error,omitempty"`
	Usage      *providers.Usage  `json:"usage,omitempty"`
	StopReason string            `json:"stop_reason,omitempty"`
	SpawnChild *markerSpawnChild `json:"spawn_child,omitempty"`
}

type markerToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// markerToolInput is all of a tool call's arguments that a marker keeps: the
// three the awaited-state derivation reads to say what a running agent is
// waiting on. None is free text — an operation name, a kind, a duration.
type markerToolInput struct {
	Op     string `json:"op,omitempty"`
	Kind   string `json:"kind,omitempty"`
	WaitMS int    `json:"wait_ms,omitempty"`
}

type markerSpawnChild struct {
	ToolUseID string `json:"tool_use_id,omitempty"`
	Index     int    `json:"index"`
	RunID     string `json:"run_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// eventMarker builds the marker for one event. A payload that is not an
// event (a run's input is a list of segments, its system prompt a list of
// blocks) becomes a marker with a type and a size and nothing else.
func eventMarker(eventType string, payload []byte) []byte {
	m := eventMarkerRecord{Type: eventType, Bytes: len(payload)}
	var ev providers.Event
	if json.Unmarshal(payload, &ev) == nil {
		m.IsError = ev.IsError
		m.Usage = ev.Usage
		m.StopReason = safeStopReason(ev.StopReason)
		if tu := ev.ToolUse; tu != nil {
			m.Tool = safeName(tu.Name)
			m.ToolUse = &markerToolUse{ID: safeName(tu.ID), Name: m.Tool, Input: markerInput(tu.Input)}
		}
		if sc := ev.SpawnChild; sc != nil {
			m.SpawnChild = &markerSpawnChild{
				ToolUseID: safeName(sc.ToolUseID), Index: sc.Index, RunID: safeName(sc.RunID),
				Agent: safeName(sc.Agent), Mode: safeName(sc.Mode), Kind: safeName(sc.Kind),
			}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		// Nothing above can fail to encode; if it somehow does, the row still
		// must not carry the payload.
		return []byte(`{"retained":false}`)
	}
	return b
}

func markerInput(input json.RawMessage) json.RawMessage {
	var in markerToolInput
	if len(input) == 0 || json.Unmarshal(input, &in) != nil {
		return nil
	}
	in.Op, in.Kind = safeName(in.Op), safeName(in.Kind)
	if in == (markerToolInput{}) {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	return b
}

// nameRe is what an identifier looks like: a tool name, an id, an operation.
// A marker keeps a value only when it matches, so a field that is supposed to
// hold a name cannot be used to store a sentence.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

func safeName(s string) string {
	if nameRe.MatchString(s) {
		return s
	}
	return ""
}

// stopReasonRe is what a stop reason from the runtime's own list looks like.
// A cancel can carry a reason somebody typed; that one is replaced.
var stopReasonRe = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)

func safeStopReason(s string) string {
	if s == "" || stopReasonRe.MatchString(s) {
		return s
	}
	return "other"
}

// notRetainedResult is the result record of a no-trace run: that it ended,
// the walk state it ended in if it was a walk, and that its content was not
// kept.
func notRetainedResult(result json.RawMessage) json.RawMessage {
	rec := struct {
		Terminal        string `json:"terminal,omitempty"`
		ContentRetained bool   `json:"content_retained"`
	}{}
	var had runResultRecord
	if len(result) > 0 && json.Unmarshal(result, &had) == nil {
		rec.Terminal = safeName(had.Terminal)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return json.RawMessage(`{"content_retained":false}`)
	}
	return b
}

// noTraceErrorPrefix marks an error text as already reduced to its class.
const noTraceErrorPrefix = "error class: "

// noTraceErrorText is what is stored for a no-trace run's error: the class of
// the error and none of its text. A provider's error can carry kilobytes of
// its own response, and that response can quote the prompt.
func noTraceErrorText(err error) string {
	if err == nil {
		return ""
	}
	class := "unclassified"
	if info, ok := errclassify.CategoryOf(err); ok && stopReasonRe.MatchString(string(info.Category)) {
		class = string(info.Category)
	}
	return noTraceErrorPrefix + class
}

// safeErrorText is the filter's backstop for an error text: one already
// reduced to its class is kept, anything else is replaced. A caller that has
// the error itself reduces it with noTraceErrorText and keeps the class; one
// that passes text loses it.
func safeErrorText(msg string) string {
	if msg == "" {
		return ""
	}
	if rest, ok := cutPrefix(msg, noTraceErrorPrefix); ok && stopReasonRe.MatchString(rest) {
		return msg
	}
	return noTraceErrorPrefix + "unclassified"
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// resolveRetention is the mode a new run of agent (defined by def) in tenant
// runs under.
//
// The strictest source wins and nothing a caller sends loosens it:
//
//   - what the caller's token requires;
//   - what the run that is starting it runs under — a child of a no-trace run
//     is no-trace whatever its own agent says, because the parent's content
//     reaches it as its prompt;
//   - what the request asked for;
//   - what the agent's definition says.
//
// The last two, and only they, can be lifted for a while by an operator's
// override for that agent (retention_override.go). A token's mode never is:
// a developer's own runs stay unstored whatever an operator switches on.
func (s *Server) resolveRetention(ctx context.Context, tenant, agent string, def config.AgentDef, requested string) string {
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.Restrictions.Retention == store.RetentionNone {
		return store.RetentionNone
	}
	if tools.RunIdentity(ctx).Retention == store.RetentionNone {
		return store.RetentionNone
	}
	if requested != store.RetentionNone && def.Retention != store.RetentionNone {
		return ""
	}
	if s.retentionLifted(ctx, tenant, agent) {
		return ""
	}
	return store.RetentionNone
}

// noTraceRefusal is a request a no-trace run cannot serve, with the HTTP
// status it is refused with. It is runner.ErrRetentionUnsupported to
// errors.Is.
type noTraceRefusal struct {
	Status int
	Msg    string
}

func (e *noTraceRefusal) Error() string {
	return fmt.Sprintf("%s: %s", runner.ErrRetentionUnsupported, e.Msg)
}
func (e *noTraceRefusal) Is(target error) bool { return target == runner.ErrRetentionUnsupported }

// admitNoTraceStart refuses what a no-trace run cannot do at its start. Each
// needs stored content: an interactive run parks and is read back, a review
// holds the answer for someone to read, a continuation replays a transcript.
// Refused, not degraded: a run that quietly lost its review would be worse
// than one that never started.
func admitNoTraceStart(mode string, interactive, review bool, sessionID string) error {
	if mode != store.RetentionNone {
		return nil
	}
	switch {
	case interactive:
		return &noTraceRefusal{Status: 400, Msg: "a no-trace run cannot be interactive: parking it would need its transcript stored"}
	case review:
		return &noTraceRefusal{Status: 400, Msg: "a no-trace run cannot be held for review: the answer would have to be stored for the reviewer"}
	case sessionID != "":
		return errNoTraceSession
	}
	return nil
}

// errNoTraceSession refuses putting a no-trace run in a session that exists,
// or any run in a no-trace session.
var errNoTraceSession = &noTraceRefusal{Status: 409, Msg: "a no-trace run opens its own session, and that session cannot be continued, replayed or compacted"}

// errNoTraceTeamMember refuses a team member whose agent is no-trace.
var errNoTraceTeamMember = &noTraceRefusal{Status: 403, Msg: "a no-trace agent cannot be a team member: a walk records its members' answers"}

// wrapStore is the Server's store handle for st: the filter, or nil when the
// server has no store.
func wrapStore(st store.Store) store.Store {
	if st == nil {
		return nil
	}
	if _, ok := st.(*traceFilterStore); ok {
		return st
	}
	return newTraceFilterStore(st)
}

// writeNoTraceRefusal is the HTTP form of a no-trace refusal.
func writeNoTraceRefusal(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if r, ok := err.(*noTraceRefusal); ok {
		status = r.Status
	}
	writeJSONError(w, status, "retention_none_unsupported", err.Error())
}

// runIsNoTrace reports whether runID is a no-trace run, by the same answer
// the store filter uses (and so failing closed the same way).
func (s *Server) runIsNoTrace(ctx context.Context, runID string) bool {
	f, ok := s.store.(*traceFilterStore)
	return ok && runID != "" && f.modes.noTrace(ctx, f.Store, runID)
}

// FailLostNoTraceRuns fails the no-trace runs a restart lost and releases
// their idempotency keys (store.FailLostNoTraceRuns). bootedAt is when this
// process started: a run begun since then is its own and alive. Called once,
// at boot, before anything can resume or retry.
func (s *Server) FailLostNoTraceRuns(ctx context.Context, bootedAt time.Time) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	return s.store.FailLostNoTraceRuns(ctx, s.replicaID, bootedAt)
}

// principalIsNoTrace reports whether the caller's token makes its runs
// no-trace. Such a caller's content is not stored by any route it can reach,
// not only by the one that starts a run: it cannot write a prompt into a
// draft, or send a message into a run that stores its input.
func principalIsNoTrace(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return ok && p.Restrictions.Retention == store.RetentionNone
}

// errNoTracePrincipalWrite refuses a no-trace token writing content that
// would be stored.
var errNoTracePrincipalWrite = &noTraceRefusal{Status: 409, Msg: "this token's runs are no-trace, so it cannot write content that would be stored: start the run directly"}
