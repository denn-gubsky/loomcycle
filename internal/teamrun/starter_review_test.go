package teamrun

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// liveArming is a BreakpointSource an operator flips while the walk runs.
type liveArming struct{ on atomic.Bool }

func (l *liveArming) Armed(state string, phase BreakpointPhase) bool {
	return state == "wave" && phase == Review && l.on.Load()
}

// Each member of an armed starter carries the walk's review arming — read live,
// so it answers what the walk says NOW — and the walk's deadline.
func TestStarterReview_MembersCarryTheLiveArmingAndDeadline(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(2)}
	arming := &liveArming{}
	arming.on.Store(true)
	var mu sync.Mutex // held for a whole member, so they finish one after the other
	var armedNow []bool
	var ttls []time.Duration
	r := starterRunner(ch, func(ctx context.Context, _ string, _ Prompt, _ string) (SpawnResult, error) {
		mu.Lock()
		defer mu.Unlock()
		armed := ReviewArming(ctx)
		if armed == nil {
			t.Error("a member of an armed starter got no review arming")
			return SpawnResult{Output: "ok"}, nil
		}
		armedNow = append(armedNow, armed(ctx))
		ttls = append(ttls, ReviewTTL(ctx))
		arming.on.Store(false) // disarmed mid-wave: the next member reads the change
		return SpawnResult{Output: "ok", Status: "completed"}, nil
	})
	WithMemberReview(arming, 90*time.Second)(r)
	st := starterState()

	if _, err := r.RunHandler(context.Background(), st, &Task{}); err != nil {
		t.Fatal(err)
	}
	if len(armedNow) != 2 || !armedNow[0] || armedNow[1] {
		t.Errorf("members read arming %v, want [true false] — it must be read live", armedNow)
	}
	for _, ttl := range ttls {
		if ttl != 90*time.Second {
			t.Errorf("member ttl = %v, want 90s", ttl)
		}
	}
}

// A walk with no review never hands its members an arming — the path a walk
// took before review existed.
func TestStarterReview_UnarmedWalkHandsNoArming(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(1)}
	r := starterRunner(ch, func(ctx context.Context, _ string, _ Prompt, _ string) (SpawnResult, error) {
		if ReviewArming(ctx) != nil || ReviewTTL(ctx) != 0 {
			t.Error("a walk with no review armed its member")
		}
		return SpawnResult{Output: "ok"}, nil
	})
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{}); err != nil {
		t.Fatal(err)
	}
}

// A rejected member reaches the sink as "rejected" with the answer it was
// rejected on, and does not count toward the wave's wait. The envelope says so
// too; an unreviewed member's envelope entry is unchanged.
func TestStarterReview_ARejectedMemberIsNotASuccess(t *testing.T) {
	ch := &fakeChannels{inbox: inbox(2)}
	var n atomic.Int32
	r := starterRunner(ch, func(context.Context, string, Prompt, string) (SpawnResult, error) {
		if n.Add(1) == 1 {
			return SpawnResult{Output: "the rejected plan", Status: MemberRejected, RunID: "r_1"}, nil
		}
		return SpawnResult{Output: "the approved plan", Status: "completed", RunID: "r_2"}, nil
	})
	st := starterState()

	// wait:all — one rejected member is one short.
	if _, err := r.RunHandler(context.Background(), st, &Task{}); err == nil || !strings.Contains(err.Error(), "rejected by the reviewer") {
		t.Errorf("wait:all with a rejected member = %v, want the wave short with the reason", err)
	}
	var rejected, ok int
	for _, m := range ch.sinks(t) {
		switch m.Status {
		case SinkRejected:
			rejected++
			if m.Output != "the rejected plan" || m.RunID != "r_1" {
				t.Errorf("rejected sink message = %+v, want the rejected answer and its run", m)
			}
		case SinkOK:
			ok++
		}
	}
	if rejected != 1 || ok != 1 {
		t.Errorf("sink = %d rejected / %d ok, want 1 / 1 — one message per run", rejected, ok)
	}

	// wait:any — the approval is enough.
	ch2 := &fakeChannels{inbox: inbox(2)}
	n.Store(0)
	r.channels = ch2
	st.Handler.Fanout.Wait = teamgraph.WaitAny
	out, err := r.RunHandler(context.Background(), st, &Task{})
	if err != nil {
		t.Fatalf("wait:any with one approval = %v", err)
	}
	if !strings.Contains(out.Output, `"status":"rejected"`) || strings.Count(out.Output, `"status"`) != 1 {
		t.Errorf("envelope = %s, want status only on the rejected member", out.Output)
	}
}

// With review armed the wave does not short-circuit: a member still out may be
// held, and cancelling it would throw a person's review away. Without review it
// does, as before.
func TestStarterReview_NoShortCircuitWhileArmed(t *testing.T) {
	for name, arm := range map[string]bool{"armed": true, "unarmed": false} {
		t.Run(name, func(t *testing.T) {
			ch := &fakeChannels{inbox: inbox(2)}
			arming := &liveArming{}
			arming.on.Store(arm)
			release := make(chan struct{})
			var cancelled atomic.Bool
			r := starterRunner(ch, func(ctx context.Context, _ string, p Prompt, _ string) (SpawnResult, error) {
				if strings.Contains(p.DataSlots[StarterMessageSlot], `"i":0`) {
					return SpawnResult{Output: "fast", Status: "completed"}, nil
				}
				select { // the slow member: held, or still working
				case <-release:
					return SpawnResult{Output: "slow", Status: "completed"}, nil
				case <-ctx.Done():
					cancelled.Store(true)
					return SpawnResult{}, ctx.Err()
				}
			})
			WithMemberReview(arming, 0)(r)
			st := starterState()
			st.Handler.Fanout.Wait = teamgraph.WaitAny

			done := make(chan error, 1)
			go func() { _, err := r.RunHandler(context.Background(), st, &Task{}); done <- err }()
			time.Sleep(100 * time.Millisecond)
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// Read off the sink rather than the fake: a short-circuit can cancel
			// the slow member before it ever starts, and then the fake never
			// sees the cancel at all.
			slow := ""
			for _, m := range ch.sinks(t) {
				if m.Index == 1 {
					slow = m.Status
				}
			}
			want := SinkError
			if arm {
				want = SinkOK
			}
			if slow != want {
				t.Errorf("slow member's sink status = %q with review armed = %v, want %q (cancelled: %v)", slow, arm, want, cancelled.Load())
			}
		})
	}
}

// The walk's own parser agrees with the live set's: review is a phase, and the
// bare form arms only the two debug pauses.
func TestParseBreakpoint_ReviewPhase(t *testing.T) {
	if id, phase, ok := ParseBreakpoint("wave:review"); !ok || id != "wave" || phase != Review {
		t.Errorf("wave:review = (%q, %q, %v)", id, phase, ok)
	}
	src, err := NewStaticBreakpoints([]string{"wave"})
	if err != nil {
		t.Fatal(err)
	}
	if src.Armed("wave", Review) {
		t.Error("the bare form armed review")
	}
}
