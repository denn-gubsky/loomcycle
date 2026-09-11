package teamrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// Debug breakpoints on a Starter (RFC CY, Decision 4 amended).
//
// WHY THE BREAKPOINT IS HERE AND NOT ON THE CHANNEL. A breakpoint decides when
// work starts, and a node kind is the thing that runs — so it belongs on the
// dispatcher. Two consequences fall out that a channel-level gate could not
// give: a paused Starter needs no storage mechanism at all (the messages simply
// stay on the channel, unread, which is what a cursor is for), and it pauses
// ONE dispatcher rather than stalling a wire for every reader in the tenant.
//
// It is also the only place the interesting data exists. The composed prompt
// for each agent never touches a channel, so no amount of channel inspection
// would ever show what an agent was actually asked — which is the first thing
// anyone wants when a fan-out misbehaves.

// BreakpointPhase is where in a wave the walk paused.
type BreakpointPhase string

const (
	// BeforeDispatch — the source has been read, binds applied and every prompt
	// composed, and NOTHING has been spawned. Step into.
	BeforeDispatch BreakpointPhase = "before_dispatch"
	// AfterCollection — the wave is complete and NOTHING has been published to
	// the sink. Step over.
	AfterCollection BreakpointPhase = "after_collection"
)

// PromptPreview is one pending run, as the operator sees it at BeforeDispatch:
// the agent that will run and the exact text it will receive.
//
// System and Input are the node's RAW templates — placeholders and ${...}
// included — because that is what the definition says, and resolving them here
// would mean resolving them twice. Message is the payload that will fill the
// data slot, which is the part an operator is usually looking for.
type PromptPreview struct {
	Index   int
	Agent   string
	System  string
	Input   string
	Message string
}

// Breakpoint is what the walk hands the operator when it pauses.
type Breakpoint struct {
	State string
	Phase BreakpointPhase
	Wave  string
	// WaveSize is the whole wave's width, which stays fixed while a staged
	// release works through it — so "3 of 8" means the same thing at every
	// pause.
	WaveSize int
	// Pending is how many of the wave are still waiting at this phase: still to
	// dispatch at BeforeDispatch, still to publish at AfterCollection.
	Pending int
	// Prompts is set at BeforeDispatch — the pending runs, in wave order.
	Prompts []PromptPreview
	// Results is set at AfterCollection — the finished runs whose sink messages
	// have not been published yet.
	Results []BreakpointResult
}

// BreakpointResult is one finished run at AfterCollection. It is a VIEW, not a
// store: the run itself is durable and carries the transcript, the tokens and
// the cost. This is the summary that answers "should the next stage see this".
type BreakpointResult struct {
	Index  int
	Agent  string
	Ok     bool
	Output string
	Error  string
}

// BreakAction is what the operator decided.
type BreakAction int

const (
	// BreakAbort terminates the walk. The zero value, so a nil or incomplete
	// decision fails safe rather than releasing a wave nobody approved.
	BreakAbort BreakAction = iota
	// BreakContinue releases everything still pending at this pause.
	BreakContinue
	// BreakRelease releases N and pauses again with the rest.
	BreakRelease
)

// BreakDecision is the operator's answer.
type BreakDecision struct {
	Action BreakAction
	// N applies to BreakRelease. Below 1 is treated as 1 — "release some" with
	// no number is a step, not a no-op that would park forever.
	N int
}

// BreakpointFunc is asked at each pause. An error aborts the walk: a debugger
// whose operator cannot be reached must not silently release the wave.
type BreakpointFunc func(ctx context.Context, bp Breakpoint) (BreakDecision, error)

// BreakpointSource is what the walk asks whether a state is armed. It is
// CONSULTED AT EVERY PAUSE POINT, never captured at dispatch — that is the
// whole interface.
//
// Breakpoints began as an argument fixed when the run started, which serves the
// case where you already know the workflow is broken. It does not serve the
// common one: you start a run expecting it to work, watch a wave go wrong, and
// want to stop before the next one. There is nothing to pass at that moment, so
// "is this armed" has to be a question the walk keeps asking rather than an
// answer it wrote down.
//
// An implementation may therefore change its answers concurrently with the
// walk, and must be safe for concurrent use.
type BreakpointSource interface {
	Armed(state string, phase BreakpointPhase) bool
}

// StaticBreakpoints is a fixed armed set — the dispatch-time argument, and the
// behaviour of a walk nobody is watching live.
type StaticBreakpoints map[string]map[BreakpointPhase]bool

// Armed implements BreakpointSource.
func (s StaticBreakpoints) Armed(state string, phase BreakpointPhase) bool {
	return s[state][phase]
}

// NewStaticBreakpoints builds a fixed source from breakpoint specs, refusing
// the whole list if any entry is malformed.
func NewStaticBreakpoints(specs []string) (StaticBreakpoints, error) {
	if err := ValidateBreakpoints(specs); err != nil {
		return nil, err
	}
	out := StaticBreakpoints{}
	for _, spec := range specs {
		id, phase, _ := ParseBreakpoint(spec)
		if out[id] == nil {
			out[id] = map[BreakpointPhase]bool{}
		}
		if phase == "" {
			out[id][BeforeDispatch] = true
			out[id][AfterCollection] = true
			continue
		}
		out[id][phase] = true
	}
	return out, nil
}

// stage walks a shrinking set of pending items in operator-sized steps.
//
// It is the shared loop behind both pauses, because "release one, look, release
// three, look, release the rest" is the same gesture whether what is being
// released is a dispatch or a publish.
//
// pending is RE-READ each round rather than tracked as a range, because the set
// is not always a suffix: a state armed part-way through a wave holds only the
// results that had not been published yet, and those are whatever indices
// happened to still be running.
func stage(ctx context.Context, pending func() []int, ask func([]int) (BreakDecision, error), act func([]int) error) error {
	for last := -1; ; {
		p := pending()
		if len(p) == 0 {
			return nil
		}
		// act must shrink the pending set; if it ever does not, this would spin
		// forever asking a human the same question. Fail loudly instead — a
		// wedged walk is far harder to diagnose than an error naming the bug.
		if last >= 0 && len(p) >= last {
			return fmt.Errorf("breakpoint staging made no progress at %d pending — releasing did not retire anything", len(p))
		}
		last = len(p)

		dec, err := ask(p)
		if err != nil {
			return err
		}
		var release []int
		switch dec.Action {
		case BreakContinue:
			release = p
		case BreakRelease:
			n := dec.N
			if n < 1 {
				// "release some" with no number is a step, not a no-op that
				// would park forever.
				n = 1
			}
			if n > len(p) {
				n = len(p)
			}
			release = p[:n]
		default:
			return fmt.Errorf("aborted at the breakpoint")
		}
		if err := act(release); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// ParseBreakpoint splits one breakpoint argument into a state id and the phase
// it arms:
//
//	"review"                   both phases of state `review`
//	"review:after_collection"  that phase only
//
// Both forms exist because the two pauses answer different questions — "what is
// about to run" and "what came back" — and a canvas stepping through a wave
// wants one at a time, while an operator who just wants the walk to stop at a
// state should not have to know there are two.
//
// It reports ok=false for an empty id or an unknown phase, so a typo is REFUSED
// at the run boundary rather than arming nothing and leaving the operator
// watching a walk that never pauses.
func ParseBreakpoint(s string) (id string, phase BreakpointPhase, ok bool) {
	id = s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		id, phase = s[:i], BreakpointPhase(s[i+1:])
		if phase != BeforeDispatch && phase != AfterCollection {
			return "", "", false
		}
	}
	if strings.TrimSpace(id) == "" {
		return "", "", false
	}
	return id, phase, true
}

// ValidateBreakpoints reports the first argument that does not parse, for the
// run boundary to refuse.
func ValidateBreakpoints(bps []string) error {
	for _, b := range bps {
		if _, _, ok := ParseBreakpoint(b); !ok {
			return fmt.Errorf("breakpoint %q: expected \"<state>\" or \"<state>:%s\"|\"<state>:%s\"",
				b, BeforeDispatch, AfterCollection)
		}
	}
	return nil
}

// armed asks the SOURCE, every time — never a value captured when the walk
// started. That is what lets an operator arm a state while the walk is running.
//
// A walk with no breakpoints answers false on the first term and never reaches
// the source, so it takes the same path it took before any of this existed.
func (r *agentRunner) armed(st teamgraph.State, phase BreakpointPhase) bool {
	return r.onBreak != nil && r.breakAt != nil && r.breakAt.Armed(st.ID, phase)
}

// previewPrompts renders the pending runs for the BeforeDispatch pause, in wave
// order. It takes the indices rather than a range because the pending set is
// not always a suffix.
func previewPrompts(h teamgraph.Handler, msgs []ChannelMessage, agentFor func(int) string, idx []int) []PromptPreview {
	out := make([]PromptPreview, 0, len(idx))
	for _, i := range idx {
		p := PromptPreview{Index: i, Agent: agentFor(i)}
		if h.Prompt != nil {
			p.System, p.Input = h.Prompt.System, h.Prompt.Input
		}
		slots := waveSlots(h, msgs, i)
		if m, ok := slots[StarterMessageSlot]; ok {
			p.Message = m
		} else {
			p.Message = slots[StarterMessagesSlot]
		}
		out = append(out, p)
	}
	return out
}

// previewResults renders the finished runs still awaiting publication.
func previewResults(all []agentResult, idx []int) []BreakpointResult {
	out := make([]BreakpointResult, 0, len(idx))
	for _, i := range idx {
		res := all[i]
		out = append(out, BreakpointResult{
			Index: res.Index, Agent: res.Agent, Ok: res.Ok,
			Output: res.Output, Error: res.Error,
		})
	}
	return out
}
