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

// stage walks a pending count in operator-sized steps.
//
// It is the shared loop behind both pauses, because "release one, look, release
// three, look, release the rest" is the same gesture whether what is being
// released is a dispatch or a publish. act is called with the half-open range
// to release; ask is called before each step.
func stage(ctx context.Context, total int, ask func(pending int) (BreakDecision, error), act func(from, to int) error) error {
	for from := 0; from < total; {
		dec, err := ask(total - from)
		if err != nil {
			return err
		}
		to := total
		switch dec.Action {
		case BreakContinue:
			// everything remaining
		case BreakRelease:
			n := dec.N
			if n < 1 {
				n = 1
			}
			if from+n < to {
				to = from + n
			}
		default:
			return fmt.Errorf("aborted at the breakpoint")
		}
		if err := act(from, to); err != nil {
			return err
		}
		from = to
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
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

// armed reports whether this state pauses at this phase. A walk with no
// breakpoints answers false on the first term and never touches the map, so it
// takes the same path it took before this existed.
func (r *agentRunner) armed(st teamgraph.State, phase BreakpointPhase) bool {
	return r.onBreak != nil && r.breakAt[st.ID][phase]
}

// previewPrompts renders the pending half of a wave for the BeforeDispatch
// pause, in wave order.
func previewPrompts(h teamgraph.Handler, msgs []ChannelMessage, agentFor func(int) string, from, to int) []PromptPreview {
	out := make([]PromptPreview, 0, to-from)
	for i := from; i < to; i++ {
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

// previewResults renders finished runs for the AfterCollection pause.
func previewResults(rs []agentResult) []BreakpointResult {
	out := make([]BreakpointResult, 0, len(rs))
	for _, res := range rs {
		out = append(out, BreakpointResult{
			Index: res.Index, Agent: res.Agent, Ok: res.Ok,
			Output: res.Output, Error: res.Error,
		})
	}
	return out
}
