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

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// teamlocalwebhooks.go — the webhooks a team declares for itself, at run time.
//
// A team's own webhook is an endpoint held by the WALKS of the team: each
// walk registers its team's webhooks here when op=run arms its triggers
// (armWalkTriggers → armTeamWebhooks) and removes them when it ends,
// whichever way it ends. The webhook receiver asks ResolveTeamWebhook, and a
// webhook no running walk has armed answers like one that does not exist.
//
// A delivery publishes ONCE into the team's channel, however many walks of
// the team are running: the channel is the team's, shared by those walks (a
// tenant-scoped one) or keyed by user (a user-scoped one), so one message is
// what every reader of it should see. The publish is made as the EARLIEST
// armed live walk of the team that declares the webhook — under its team
// version and scope — and attributed to the user the payload names
// (payload_mapping.user_id), else to that walk's user. For a tenant-scoped
// channel the user is only the attribution; a user-scoped channel's message
// is stored under the payload's user, and a delivery naming none is refused
// rather than filed under whichever walk happened to arm first.
//
// The registry is this replica's own, and the fast path. A delivery can reach
// any replica, so a walk also records each webhook it arms in the store
// (team_webhook_arms) as a LEASE, and a replica where no walk of the team is
// running answers from there: a delivery is a store write, which any replica
// can make given the team version and scope the lease names. The lease is
// renewed while the walk runs and deleted by its disarm; one a crashed walk
// left behind stops counting when it lapses (a few walk heartbeats), and one
// whose walk run is no longer running never counts. A delivery accepted that
// way is published as the earliest-armed live walk in the store, under the
// same rules as a local one.
//
// The remote path cannot hold the walk's disarm off as the local one does: a
// delivery resolved from a lease just before the walk ended may still publish.
// It re-reads the lease immediately before publishing, which narrows that to
// the one channel write, and what lands is a message in a channel the team
// owns — never a walk started or an agent run.

// teamWebhooks is the registry: (tenant, team, name) → the walks that armed
// that webhook, in the order they armed it.
type teamWebhooks struct {
	// received is set when the webhook receiver mounts the route a team's
	// webhooks are delivered to (ReceiveTeamWebhooks). Without it, a walk of
	// a team declaring webhooks is refused: it would wait on an endpoint that
	// does not exist.
	received atomic.Bool

	mu   sync.Mutex
	live map[teamWebhookKey][]*armedTeamWebhook
}

type teamWebhookKey struct{ tenant, team, name string }

// armedTeamWebhook is one webhook, armed by one walk. One read from a lease
// (leasedTeamWebhook) is never closed: the walk's disarm is on another replica.
type armedTeamWebhook struct {
	key    teamWebhookKey
	walkID string
	sc     store.TeamScope
	userID string // the walk's user
	hook   builtin.LocalWebhookDefinition

	// mu is held shared by a publish and exclusively by the disarm, so the
	// disarm returns only once no publish is in flight, and none starts after.
	mu     sync.RWMutex
	closed bool
}

// ReceiveTeamWebhooks records that this server receives a team's own webhooks
// — the receiver mounts their route — and returns the resolver it reaches
// them through. Called once, by the composition root that mounts the route.
func (s *Server) ReceiveTeamWebhooks() runner.TeamWebhookResolver {
	s.teamHooks.received.Store(true)
	return s
}

// ResolveTeamWebhook implements runner.TeamWebhookResolver: team `team`'s
// webhook `name` in tenant, when a walk of the team armed it and that walk's
// version is not retired. Everything else is false, alike: the receiver turns
// every false into the same answer.
//
// The registry lookup is in memory; a name no walk here armed costs one
// indexed read of the store's leases, and only an armed one reads its version
// row.
//
// The registry is consulted first, so a replica running a walk of the team
// publishes as its own earliest walk even when a walk elsewhere armed before
// it: either way one message per delivery, into the same channel.
func (s *Server) ResolveTeamWebhook(ctx context.Context, tenant, team, name string) (runner.TeamWebhook, bool) {
	key := teamWebhookKey{tenant: tenant, team: team, name: name}
	if a := s.teamHooks.first(key); a != nil {
		// A retired version takes nothing new from outside, as it starts no
		// new walk; a version deleted under a running walk is gone.
		row, _, err := s.teamVersion(ctx, tenant, a.sc)
		if err != nil || row.Retired {
			return runner.TeamWebhook{}, false
		}
		return teamWebhookOf(a, func(ctx context.Context, userID string, keys []string, body json.RawMessage) error {
			return s.publishTeamWebhook(ctx, a, userID, keys, body)
		}), true
	}
	// No walk of the team armed it here; one on another replica may have.
	a, err := s.leasedTeamWebhook(ctx, key)
	if err != nil {
		log.Printf("team webhook %s/%s: read its lease: %v", team, name, err)
	}
	if a == nil {
		return runner.TeamWebhook{}, false
	}
	return teamWebhookOf(a, func(ctx context.Context, userID string, keys []string, body json.RawMessage) error {
		// Read again now, as the walk may have ended while the delivery was
		// verified: publish as whichever walk holds the webhook at this
		// moment, or not at all.
		live, err := s.leasedTeamWebhook(ctx, key)
		if err != nil {
			return err
		}
		if live == nil {
			return runner.ErrTeamWebhookGone
		}
		return s.publishTeamWebhook(ctx, live, userID, keys, body)
	}), true
}

// teamWebhookOf is the receiver's view of webhook a, publishing through
// publish.
func teamWebhookOf(a *armedTeamWebhook, publish func(ctx context.Context, userID string, keys []string, body json.RawMessage) error) runner.TeamWebhook {
	var mapping map[string]string
	if a.hook.UserIDPath != "" {
		mapping = map[string]string{"user_id": a.hook.UserIDPath}
	}
	return runner.TeamWebhook{
		Auth:           a.hook.Auth,
		PayloadMapping: mapping,
		Channel:        teamgraph.LocalRefPrefix + a.hook.Channel,
		Publish:        publish,
	}
}

// leasedTeamWebhook is webhook key as the earliest-armed live lease in the
// store holds it — a walk running on any replica — read from the team version
// the lease names. nil when there is none, or its version is retired, gone or
// does not declare the webhook: the receiver answers all of those alike. The
// error is a store fault reading the lease.
//
// The lease lookup is one indexed read, made for a request nobody has
// authenticated yet, as resolving a WebhookDef is.
func (s *Server) leasedTeamWebhook(ctx context.Context, key teamWebhookKey) (*armedTeamWebhook, error) {
	if s.store == nil {
		return nil, nil
	}
	lease, ok, err := s.store.TeamWebhookArmLive(ctx, key.tenant, key.team, key.name, s.clock().Now())
	if err != nil || !ok {
		return nil, err
	}
	sc := store.TeamScope{Tenant: lease.TenantID, Team: lease.Team, DefID: lease.DefID}
	row, def, err := s.teamVersion(ctx, key.tenant, sc)
	if err != nil || row.Retired {
		return nil, nil
	}
	hook, err := builtin.LocalWebhookOf(def, key.name)
	if err != nil {
		return nil, nil
	}
	return &armedTeamWebhook{key: key, walkID: lease.WalkRunID, sc: sc, userID: lease.UserID, hook: hook}, nil
}

// teamWebhookDedupTTL is how long an accepted delivery's keys are held in the
// store. A replay inside it — to any replica, across a restart — publishes
// nothing. Longer than the receiver's in-process window (10 minutes) and the
// Stripe-style signature tolerance it backs; a body-only signature (GitHub's)
// has no time limit, so a capture replayed after this publishes again, as a
// WebhookDef's channel delivery does after its in-process window.
const teamWebhookDedupTTL = 24 * time.Hour

// publishTeamWebhook publishes one verified delivery into the webhook's
// channel, as walk a: under its team scope, attributed to userID or, when the
// payload named none and the channel is tenant-scoped, to the walk's user.
//
// The delivery's dedup keys are claimed in the store first, by the insert
// itself, so of two replicas taking one delivery (a replay, or the sender's
// retry racing its original) exactly one publishes; the other gets
// ErrTeamWebhookDuplicate. A publish that then fails releases the claim, so
// the sender's retry is not taken for a duplicate.
func (s *Server) publishTeamWebhook(ctx context.Context, a *armedTeamWebhook, userID string, keys []string, body json.RawMessage) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return runner.ErrTeamWebhookGone
	}
	if userID == "" {
		if a.hook.UserScoped {
			return runner.ErrTeamWebhookNeedsUser
		}
		userID = a.userID
	}
	if s.store != nil && len(keys) > 0 {
		now := s.clock().Now()
		claimed, err := s.store.WebhookDeliveryClaim(ctx, keys, now, now.Add(teamWebhookDedupTTL))
		if err != nil {
			return err
		}
		if !claimed {
			return runner.ErrTeamWebhookDuplicate
		}
	}
	if _, err := s.publishTeamLocalChannel(ctx, a.sc.Tenant, a.sc, a.hook.Channel, userID, body); err != nil {
		if s.store != nil && len(keys) > 0 {
			if rerr := s.store.WebhookDeliveryRelease(context.WithoutCancel(ctx), keys); rerr != nil {
				log.Printf("team %q: release the dedup keys of a delivery that did not publish: %v", a.sc.Team, rerr)
			}
		}
		return err
	}
	return nil
}

// armTeamWebhooks registers the team's own webhooks for the walk running under
// ctx, and returns the disarm: it removes them and returns only once no
// delivery to them is publishing.
//
// Every webhook is decoded and checked once, here: one that could never take a
// delivery refuses the walk instead of answering every delivery with a fault.
func (s *Server) armTeamWebhooks(ctx context.Context, def teamgraph.Definition) (func(), error) {
	names := def.LocalWebhookNames()
	if len(names) == 0 {
		return func() {}, nil
	}
	sc, inTeam := store.TeamScopeFromContext(ctx)
	if !inTeam || sc.Team == "" || sc.DefID == "" {
		return nil, fmt.Errorf("a team's own webhooks are open only inside a walk of that team")
	}
	if !s.teamHooks.received.Load() {
		return nil, fmt.Errorf("team %q declares webhooks of its own, and this server does not receive webhooks (LOOMCYCLE_WEBHOOKS_ENABLED is off)", sc.Team)
	}
	ident := tools.RunIdentity(ctx)
	// Its channels are the team's tenant's, and so is the route a delivery
	// arrives on.
	if sc.Tenant != ident.TenantID {
		return nil, fmt.Errorf("team %q's own webhooks belong to its tenant, and this walk runs in another; run it as that tenant", sc.Team)
	}
	walkID := tools.RunID(ctx)
	armed := make([]*armedTeamWebhook, 0, len(names))
	for _, name := range names {
		hook, err := builtin.LocalWebhookOf(def, name)
		if err != nil {
			return nil, fmt.Errorf("team %q: its webhook %q: %w", sc.Team, name, err)
		}
		armed = append(armed, &armedTeamWebhook{
			key:    teamWebhookKey{tenant: sc.Tenant, team: sc.Team, name: name},
			walkID: walkID,
			sc:     sc,
			userID: ident.UserID,
			hook:   hook,
		})
	}
	s.teamHooks.add(armed)
	endLease := s.leaseTeamWebhooks(ctx, sc, walkID, ident.UserID, names)
	log.Printf("team %q (version %s) walk %s: armed its own webhooks: %s", sc.Team, sc.DefID, walkID, strings.Join(names, ", "))
	var once sync.Once
	return func() {
		once.Do(func() {
			// The lease first, so other replicas stop taking deliveries
			// before this one does.
			endLease()
			s.teamHooks.remove(armed)
			for _, a := range armed {
				a.mu.Lock()
				a.closed = true
				a.mu.Unlock()
			}
			log.Printf("team %q (version %s) walk %s: disarmed its own webhooks", sc.Team, sc.DefID, walkID)
		})
	}, nil
}

// teamHookLeaseBeats is how many renewals a lease outlives: a walk renews its
// leases every walk heartbeat, and a lease lapses this many heartbeats after
// the last renewal — so a crashed walk's webhooks stop answering within about
// that long, and a renewal or two delayed by a slow store does not drop a
// live walk's.
const teamHookLeaseBeats = 3

// leaseTeamWebhooks records in the store that the walk walkID armed the
// team's webhooks names, renews that lease every walk heartbeat while the
// walk runs, and returns the end: it stops the renewals and deletes the
// lease. A store fault is logged and the walk goes on: its webhooks then
// answer only on this replica (the registry), until a renewal lands.
//
// A walk with no run id has nothing to name its lease by, and takes
// deliveries only here.
func (s *Server) leaseTeamWebhooks(ctx context.Context, sc store.TeamScope, walkID, userID string, names []string) func() {
	if s.store == nil || walkID == "" {
		return func() {}
	}
	every := s.walkHeartbeatEvery
	if every <= 0 {
		every = loop.HeartbeatInterval()
	}
	clock := s.clock()
	armedAt := clock.Now()
	// The lease's writes outlive a cancelled walk: its end must still delete.
	bg := context.WithoutCancel(ctx)
	put := func() {
		expires := clock.Now().Add(teamHookLeaseBeats * every)
		rows := make([]store.TeamWebhookArm, 0, len(names))
		for _, name := range names {
			rows = append(rows, store.TeamWebhookArm{
				TenantID: sc.Tenant, Team: sc.Team, Name: name, WalkRunID: walkID,
				DefID: sc.DefID, UserID: userID, ArmedAt: armedAt, ExpiresAt: expires,
			})
		}
		wctx, cancel := context.WithTimeout(bg, 5*time.Second)
		defer cancel()
		if err := s.store.TeamWebhookArmPut(wctx, rows); err != nil {
			log.Printf("team %q walk %s: record its webhooks' lease: %v", sc.Team, walkID, err)
		}
	}
	put()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-clock.After(every):
				put()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			// A renewal in flight would write the lease back after the
			// delete, leaving the webhook open until it lapsed.
			wg.Wait()
			dctx, cancel := context.WithTimeout(bg, 5*time.Second)
			defer cancel()
			if err := s.store.TeamWebhookArmDelete(dctx, sc.Tenant, sc.Team, walkID); err != nil {
				// Not left open: the walk's run is recorded as over next, and
				// a lease of a run no longer running is not read.
				log.Printf("team %q walk %s: delete its webhooks' lease: %v", sc.Team, walkID, err)
			}
		})
	}
}

func (r *teamWebhooks) add(armed []*armedTeamWebhook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		r.live = make(map[teamWebhookKey][]*armedTeamWebhook)
	}
	for _, a := range armed {
		r.live[a.key] = append(r.live[a.key], a)
	}
}

func (r *teamWebhooks) remove(armed []*armedTeamWebhook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range armed {
		walks := r.live[a.key]
		kept := walks[:0]
		for _, w := range walks {
			if w != a {
				kept = append(kept, w)
			}
		}
		if len(kept) == 0 {
			delete(r.live, a.key)
			continue
		}
		r.live[a.key] = kept
	}
}

// first is the earliest-armed live walk's entry for key, nil when none.
func (r *teamWebhooks) first(key teamWebhookKey) *armedTeamWebhook {
	r.mu.Lock()
	defer r.mu.Unlock()
	if walks := r.live[key]; len(walks) > 0 {
		return walks[0]
	}
	return nil
}

// count is how many webhook registrations are live, for tests.
func (r *teamWebhooks) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, walks := range r.live {
		n += len(walks)
	}
	return n
}
