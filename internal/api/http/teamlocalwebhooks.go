package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

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
// The registry is this replica's own. A delivery reaching a replica where no
// walk of the team is running gets the not-found answer, though one may be
// running elsewhere: the deployment must route a team's webhook to the
// replica running its walk (or run one replica). Accepting on any replica
// would need a durable record of which walks are armed, and the only store
// facility that holds such a key without a migration is the memory store —
// whose rows the Memory tool, the memory admin surfaces, snapshots and quota
// accounting all treat as an agent's own data. That record needs a table of
// its own, so it is left for a change that adds one.

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

// armedTeamWebhook is one webhook, armed by one walk.
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
// The registry lookup is in memory, so a name no walk armed costs nothing
// more than that; only an armed one reads its version row.
func (s *Server) ResolveTeamWebhook(ctx context.Context, tenant, team, name string) (runner.TeamWebhook, bool) {
	a := s.teamHooks.first(teamWebhookKey{tenant: tenant, team: team, name: name})
	if a == nil {
		return runner.TeamWebhook{}, false
	}
	// A retired version takes nothing new from outside, as it starts no new
	// walk; a version deleted under a running walk is gone.
	row, _, err := s.teamVersion(ctx, tenant, a.sc)
	if err != nil || row.Retired {
		return runner.TeamWebhook{}, false
	}
	var mapping map[string]string
	if a.hook.UserIDPath != "" {
		mapping = map[string]string{"user_id": a.hook.UserIDPath}
	}
	return runner.TeamWebhook{
		Auth:           a.hook.Auth,
		PayloadMapping: mapping,
		Channel:        teamgraph.LocalRefPrefix + a.hook.Channel,
		Publish: func(ctx context.Context, userID string, body json.RawMessage) error {
			return s.publishTeamWebhook(ctx, a, userID, body)
		},
	}, true
}

// publishTeamWebhook publishes one verified delivery into the webhook's
// channel, as walk a: under its team scope, attributed to userID or, when the
// payload named none and the channel is tenant-scoped, to the walk's user.
func (s *Server) publishTeamWebhook(ctx context.Context, a *armedTeamWebhook, userID string, body json.RawMessage) error {
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
	_, err := s.publishTeamLocalChannel(ctx, a.sc.Tenant, a.sc, a.hook.Channel, userID, body)
	return err
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
	log.Printf("team %q (version %s) walk %s: armed its own webhooks: %s", sc.Team, sc.DefID, walkID, strings.Join(names, ", "))
	var once sync.Once
	return func() {
		once.Do(func() {
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
