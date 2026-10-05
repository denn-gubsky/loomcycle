package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamlocalschedules.go — the schedules a team declares for itself, at run
// time.
//
// A team's own schedule (teamgraph.LocalSchedule) is a timer held by a WALK of
// the team: armed when op=run starts the walk (TeamDef.ArmWalkTriggers →
// armWalkTriggers → armTeamSchedules), stopped when the walk ends, whichever
// way it ends. It has no row anywhere — nothing in schedule_defs or
// schedule_run_state, nothing the scheduler sweeps — so a team with no walk
// running has no live schedule.
//
// Each tick is a message published into one of the team's own channels
// through publishTeamLocalChannel, under the walk's team scope and as the walk
// run's user. It is stored like any message, so a reader of that channel on
// any replica sees it.
//
// Two walks of a team running at once arm a timer each. A tenant-scoped
// channel is one keyspace for every walk of the team in its tenant, so both
// walks' ticks land in it and either walk may read one; a user-scoped channel
// is each walk's user's own.
//
// Nothing is carried over: a walk is not re-entered on another instance (its
// run is a record with no loop to resume), and a walk started again starts
// its schedules afresh — no fire count, no phase.

// walkClock is the time a walk's own schedules run on. The wall clock in
// production; a test drives its own.
type walkClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (s *Server) clock() walkClock {
	if s.walkClock != nil {
		return s.walkClock
	}
	return wallClock{}
}

// armedSchedule is one of a team's schedules, resolved for a walk.
type armedSchedule struct {
	name    string
	cadence interface{ Next(time.Time) time.Time }
	channel string // the team's own channel, without "./"
	payload json.RawMessage
}

// tickCounts is what a walk's schedules did, for the one line its disarm logs.
type tickCounts struct {
	published, paused, failed atomic.Int64
}

// armWalkTriggers arms what wakes the walk running under ctx from inside its
// team — its own schedules, then its own webhooks — and returns the disarm,
// which undoes both and returns only once neither can publish again. Wired as
// TeamDef.ArmWalkTriggers.
func (s *Server) armWalkTriggers(ctx context.Context, def teamgraph.Definition) (func(), error) {
	disarmSchedules, err := s.armTeamSchedules(ctx, def)
	if err != nil {
		return nil, err
	}
	disarmWebhooks, err := s.armTeamWebhooks(ctx, def)
	if err != nil {
		disarmSchedules()
		return nil, err
	}
	return func() {
		disarmWebhooks()
		disarmSchedules()
	}, nil
}

// armTeamSchedules starts the timers of the team's own schedules for the walk
// running under ctx, and returns the disarm: it stops every one of them and
// returns only once none can publish again.
//
// Every schedule's channel is resolved once, here, in the walk's tenant and
// for the walk's user: a schedule that could never publish — the walk runs in
// another tenant than the team, or the channel is user-scoped and the walk has
// no user — refuses the walk instead of failing on every tick.
func (s *Server) armTeamSchedules(ctx context.Context, def teamgraph.Definition) (func(), error) {
	names := def.LocalScheduleNames()
	if len(names) == 0 {
		return func() {}, nil
	}
	sc, inTeam := store.TeamScopeFromContext(ctx)
	if !inTeam || sc.Team == "" || sc.DefID == "" {
		return nil, fmt.Errorf("a team's own schedules run only inside a walk of that team")
	}
	ident := tools.RunIdentity(ctx)
	armed := make([]armedSchedule, 0, len(names))
	for _, name := range names {
		ls := def.Local.Schedules[name]
		cadence, err := teamgraph.ParseLocalCadence(ls.Schedule)
		if err != nil {
			return nil, fmt.Errorf("team %q: its schedule %q: %w", sc.Team, name, err)
		}
		local, isLocal := teamgraph.LocalRef(ls.Channel)
		if !isLocal {
			return nil, fmt.Errorf("team %q: its schedule %q publishes to %q, which is not one of the team's own channels", sc.Team, name, ls.Channel)
		}
		if _, _, _, _, err := s.teamLocalChannelTarget(def, sc, ident.TenantID, local, ident.UserID); err != nil {
			return nil, fmt.Errorf("team %q: its schedule %q: %w", sc.Team, name, err)
		}
		armed = append(armed, armedSchedule{name: name, cadence: cadence, channel: local, payload: ls.Payload})
	}

	tickCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	counts := &tickCounts{}
	for _, a := range armed {
		wg.Add(1)
		s.walkTimers.Add(1)
		go func() {
			defer wg.Done()
			defer s.walkTimers.Add(-1)
			s.runTeamSchedule(tickCtx, sc, ident.TenantID, ident.UserID, a, counts)
		}()
	}
	walkID := tools.RunID(ctx)
	log.Printf("team %q (version %s) walk %s: armed its own schedules: %s", sc.Team, sc.DefID, walkID, strings.Join(names, ", "))
	var once sync.Once
	return func() {
		once.Do(func() {
			stop()
			wg.Wait()
			log.Printf("team %q (version %s) walk %s: disarmed its own schedules (%d tick(s) published, %d skipped while paused, %d failed)",
				sc.Team, sc.DefID, walkID, counts.published.Load(), counts.paused.Load(), counts.failed.Load())
		})
	}, nil
}

// runTeamSchedule ticks one schedule until ctx ends.
func (s *Server) runTeamSchedule(ctx context.Context, sc store.TeamScope, tenant, userID string, a armedSchedule, counts *tickCounts) {
	clock := s.clock()
	ref := teamgraph.LocalRefPrefix + a.name
	reported := false
	for {
		now := clock.Now()
		next := a.cadence.Next(now)
		if next.IsZero() {
			// The parser's answer for a cadence with no time left to fire at;
			// waiting on it would spin.
			log.Printf("team %q: its schedule %q will not fire again", sc.Team, a.name)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.After(next.Sub(now)):
		}
		// A disarm that raced the timer wins: nothing lands once the walk is
		// ending.
		if ctx.Err() != nil {
			return
		}
		// Paused, the runtime is quiesced — for a snapshot, typically — and the
		// walk's members are parked. The scheduler skips its ticks while
		// paused, and so does a team's: the first tick after the resume
		// publishes, and the skipped ones are not made up.
		if s.runtimePaused() {
			counts.paused.Add(1)
			continue
		}
		payload := a.payload
		if payload == nil {
			p, err := scheduler.TickPayload(ref, clock.Now(), nil)
			if err != nil {
				counts.failed.Add(1)
				continue
			}
			payload = p
		}
		if _, err := s.publishTeamLocalChannel(ctx, tenant, sc, a.channel, userID, payload); err != nil {
			if ctx.Err() != nil {
				return
			}
			counts.failed.Add(1)
			// Once per schedule per walk: a channel that refuses one tick will
			// refuse the next the same way.
			if !reported {
				reported = true
				log.Printf("team %q walk %s: its schedule %q could not publish to %q (logged once; counted at disarm): %v",
					sc.Team, tools.RunID(ctx), a.name, teamgraph.LocalRefPrefix+a.channel, err)
			}
			continue
		}
		counts.published.Add(1)
	}
}
