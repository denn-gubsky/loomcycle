package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Armed subscriptions: a promoted team whose ENTRY state is a Starter runs on
// its own when its source channel has work.
//
// WHY A SWEEP AND NOT A LONG-LIVED SUBSCRIBER. The design called for arming on
// promote and disarming on retire, and warned that "an armed subscription for a
// retired workflow is a silent resource leak". A sweep removes the leak by
// removing the state: there is nothing armed to forget to disarm. Each tick
// asks the store which teams are promoted, so retiring one — or deleting it, or
// promoting a version whose entry is no longer a Starter — stops it being
// driven, with no lifecycle to get wrong and nothing to reconcile after a
// crash. The verification the design asked for still holds, and two of its
// three items become free: killing the replica that was driving a team means
// the next tick elsewhere picks it up, because picking it up is all there ever
// was.
//
// WHAT IT COSTS, stated plainly: this is the one part of the runtime that
// starts agent runs with no human in the loop. That is why it is default-OFF
// behind LOOMCYCLE_TEAM_SUBSCRIPTIONS, why it holds a per-team lock across the
// whole walk rather than just the decision to start one, and why it peeks
// before it walks — an idle team must cost a query, not a run.

// teamSubscription is one promoted team the sweep may drive.
type teamSubscription struct {
	DefID    string
	TenantID string
	Name     string
	Source   string // the entry Starter's source channel
}

// SweepTeamSubscriptions runs one tick: drive every promoted team whose entry
// state is a Starter and whose source has messages waiting.
//
// Returns how many walks it started, for the caller's log. An error is returned
// only for a failure that stopped the sweep itself — a single team failing is
// logged and the sweep continues, because one broken workflow must not stop
// every other one from running.
func (s *Server) SweepTeamSubscriptions(ctx context.Context) (started int, err error) {
	if s.store == nil {
		return 0, nil
	}
	subs, err := s.listTeamSubscriptions(ctx)
	if err != nil {
		return 0, err
	}
	for _, sub := range subs {
		ran, rerr := s.driveTeamSubscription(ctx, sub)
		if rerr != nil {
			// Logged, not returned: a team whose channel is undeclared or
			// whose def stopped parsing must not silence the rest.
			log.Printf("team-subscriptions: %s/%s: %v", sub.TenantID, sub.Name, rerr)
			continue
		}
		if ran {
			started++
		}
	}
	return started, nil
}

// listTeamSubscriptions enumerates the promoted teams that are driveable.
//
// The enumeration IS the arming. A team appears here because its active pointer
// names a live version whose entry is a Starter — so promote arms it, retire
// and delete disarm it, and promoting a version with a different entry changes
// what is driven, all without a single piece of runtime state.
func (s *Server) listTeamSubscriptions(ctx context.Context) ([]teamSubscription, error) {
	names, err := s.store.TeamDefListNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	var out []teamSubscription
	for _, n := range names {
		// No active pointer = nothing promoted. A retired active version is
		// NOT driven: soft-retire is how an operator takes a workflow out of
		// service, and it would be a poor take-out-of-service that kept
		// running it.
		if n.ActiveDefID == "" || n.ActiveRetired {
			continue
		}
		row, err := s.store.TeamDefGet(ctx, n.ActiveDefID)
		if err != nil {
			log.Printf("team-subscriptions: %s/%s: read active def: %v", n.TenantID, n.Name, err)
			continue
		}
		def, err := teamgraph.Parse(row.Definition)
		if err != nil {
			log.Printf("team-subscriptions: %s/%s: parse: %v", n.TenantID, n.Name, err)
			continue
		}
		entry, ok := teamgraph.StateByID(def, def.Entry)
		if !ok || entry.Handler.Kind != teamgraph.HandlerStarter || entry.Handler.Source == nil {
			continue // not a subscriber — an ordinary team runs when asked
		}
		out = append(out, teamSubscription{
			DefID: row.DefID, TenantID: row.TenantID, Name: row.Name,
			Source: entry.Handler.Source.Channel,
		})
	}
	// Deterministic order so a log reads the same way twice and a test can
	// assert on it.
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// driveTeamSubscription runs one team's walk if its source has work, under the
// team's own advisory lock so exactly one replica drives it at a time.
//
// Reports whether a walk was started. Not started is the common case and is not
// an error: most ticks find an empty channel.
func (s *Server) driveTeamSubscription(ctx context.Context, sub teamSubscription) (bool, error) {
	if !s.subBackoff.ready(sub.DefID) {
		return false, nil // failing; not yet time to try again
	}
	started := false
	run := func(c context.Context) error {
		has, err := s.sourceHasWork(c, sub)
		if err != nil || !has {
			return err
		}
		started = true
		return s.runSubscribedTeam(c, sub)
	}
	// No cluster coordinator = single process; drive it directly. A
	// single-replica deployment must not need Postgres to run its workflows.
	if s.advisoryLock == nil {
		return started, run(ctx)
	}
	// TryRun holds the lock for the whole of run — the peek AND the walk — so
	// a second replica (or this replica's next tick) cannot start a concurrent
	// walk over the same source. A lost race returns (false, nil): another
	// replica has it, which is the expected case and not worth logging.
	if _, err := s.advisoryLock.TryRun(ctx, coord.TeamSubscriptionLockKey(sub.DefID), run); err != nil {
		return started, err
	}
	return started, nil
}

// sourceHasWork peeks the entry Starter's source. A peek, not a read: the walk
// itself moves the cursor, and a sweep that consumed would eat the very message
// it was starting the walk for.
func (s *Server) sourceHasWork(ctx context.Context, sub teamSubscription) (bool, error) {
	def, ok := s.ResolveChannelScope(ctx, sub.Source)
	if !ok {
		return false, fmt.Errorf("source channel %q is not declared", sub.Source)
	}
	scope, scopeID, err := subscriptionScope(def.Scope, sub.Source)
	if err != nil {
		return false, err
	}
	msgs, err := s.store.ChannelPeek(ctx, sub.TenantID, sub.Source, scope, scopeID, "", 1)
	if err != nil {
		return false, fmt.Errorf("peek %q: %w", sub.Source, err)
	}
	return len(msgs) > 0, nil
}

// subscriptionScope resolves where a subscribed team reads from.
//
// A sweep has no user and no agent, so it can drive only a channel whose
// declared scope needs neither. That is a REFUSAL rather than a guess: an
// agent- or user-scoped channel has a separate queue per agent or per user, and
// picking one would silently drive a single arbitrary queue while every other
// one accumulated unread — a workflow that looks like it is running and is
// quietly ignoring most of its work.
//
// tenant and global both work: a tenant-scoped source reads under the team's
// own tenant (the store's tenant column isolates it), and a global one is the
// single shared queue.
func subscriptionScope(declared, channel string) (store.MemoryScope, string, error) {
	switch declared {
	case "global":
		return store.MemoryScopeGlobal, "", nil
	case "tenant":
		return store.MemoryScopeTenant, "", nil
	case "agent", "user":
		return "", "", fmt.Errorf(
			"source channel %q is scope=%s, which has a separate queue per %s — "+
				"a subscribed team has no %s to read as. Declare the source scope: tenant or global",
			channel, declared, declared, declared)
	default:
		return "", "", fmt.Errorf("source channel %q has unknown scope %q", channel, declared)
	}
}

// subscriptionCtx stamps the identity a subscribed walk runs under.
//
// THE TEAM'S OWN TENANT, never an admin. op=run resolves a def_id under the
// caller's tenant and folds a mismatch into an opaque not-found — so a sweep
// running under the empty tenant cannot even see the team it is trying to
// drive. (It could not, until this existed: every drive failed `def_id … not
// found`, which is also exactly the refusal a cross-tenant caller gets, so the
// message was telling the truth.) Taking the tenant from the def ROW keeps the
// authority model intact: it is the authoritative owner, not something the
// sweep chose.
//
// The user is `_system`, the same attribution internal publishes carry, so a
// run started by nobody is not attributed to somebody.
func (s *Server) subscriptionCtx(ctx context.Context, sub teamSubscription) context.Context {
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		TenantID: sub.TenantID,
		UserID:   channels.SystemPublisherUserID,
		AgentID:  "team:" + sub.Name,
	})
}

// backoff decides whether a team that FAILED recently may be driven again yet,
// and records the outcome.
//
// An autonomous driver needs this and a human-triggered run does not. A walk
// that fails does not ack its source — `after_results` is at-least-once by
// design, so the batch redelivers — which under a person means "they see the
// error and fix it", and under a sweep means the same wave every tick forever.
// A poison message would burn tokens at tick rate until someone noticed.
//
// Exponential, capped, and reset by any success. In-memory and per-replica on
// purpose: another replica may retry sooner, which is right — a failure local
// to one process must not take a workflow out of service cluster-wide.
//
// The ZERO VALUE works. A Server assembled without a constructor — every test
// fixture in this package does that — would otherwise carry a nil backoff and
// silently lose the protection, which is the worst way to lose it. The maps are
// built on first use under the lock.
type subscriptionBackoff struct {
	mu    sync.Mutex
	until map[string]time.Time
	fails map[string]int
}

// maxSubscriptionBackoff caps the delay. Long enough that a broken workflow is
// cheap, short enough that a fixed one recovers without a restart.
const maxSubscriptionBackoff = 10 * time.Minute

func (b *subscriptionBackoff) ready(defID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !time.Now().Before(b.until[defID]) // a nil map reads as the zero Time
}

func (b *subscriptionBackoff) record(defID string, walkErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if walkErr == nil {
		delete(b.until, defID)
		delete(b.fails, defID)
		return
	}
	if b.until == nil {
		b.until, b.fails = map[string]time.Time{}, map[string]int{}
	}
	b.fails[defID]++
	d := time.Duration(1<<min(b.fails[defID], 10)) * time.Second
	if d > maxSubscriptionBackoff {
		d = maxSubscriptionBackoff
	}
	b.until[defID] = time.Now().Add(d)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// runSubscribedTeam starts the walk through the same op=run an operator uses,
// so a subscribed run and a manual one take ONE code path — admission, the
// breakpoint set, the run row and its finish are all identical, and a bug fixed
// in one is fixed in both.
func (s *Server) runSubscribedTeam(ctx context.Context, sub teamSubscription) error {
	input, err := json.Marshal(map[string]any{"op": "run", "def_id": sub.DefID})
	if err != nil {
		return err
	}
	res, err := s.TeamDef(s.subscriptionCtx(ctx, sub), input)
	if err != nil {
		s.subBackoff.record(sub.DefID, err)
		return err
	}
	if res.IsError {
		// A failed walk does NOT ack its source, so without a backoff the same
		// message drives the same failure on the very next tick.
		s.subBackoff.record(sub.DefID, fmt.Errorf("%s", res.Text))
		// The walk ran and failed — the workflow's own outcome, not a sweep
		// fault, and already recorded on its run. Logged so an operator
		// watching the sweep sees it without reading the runs list.
		log.Printf("team-subscriptions: %s/%s walk failed (backing off): %s", sub.TenantID, sub.Name, res.Text)
		return nil
	}
	s.subBackoff.record(sub.DefID, nil)
	return nil
}
