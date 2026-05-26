package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// CancelCoordinator implements cancel.ClusterCanceller via the
// v0.12.0 Backplane. One instance per replica, wired in main.go
// inside the cluster-mode init block. When a local cancel registry
// lookup misses on this replica, the registry delegates to
// CancelRemote which:
//
//  1. Queries the runs table for the agent's owning replica_id.
//  2. Checks owner liveness via replicas.IsReplicaAlive — if dead,
//     marks the run failed in the DB and returns success without
//     a broadcast (Phase 5's TTL sweeper will close this loop, but
//     this short-circuit saves the 5s ack wait).
//  3. Publishes a `loomcycle.cancel` event on the backplane carrying
//     {agent_id, reason, from_replica}.
//  4. Waits on a per-call ack channel keyed by agent_id for up to
//     AckTimeout. On ack: returns success with the cascaded child
//     list. On timeout: re-checks the run row (it may have completed
//     during the wait) and returns either the terminal status or
//     `owner_replica_unreachable`.
//
// Two long-lived subscriber goroutines (RunCancelSubscriber +
// RunAckSubscriber) carry the wire side: one listens for incoming
// cancel events and dispatches to the local registry; the other
// receives acks and routes them to the in-flight CancelRemote waiters.
type CancelCoordinator struct {
	bp         Backplane
	replicaID  string
	store      cancelRunStore
	replicas   *ReplicaStore
	ackTimeout time.Duration

	mu sync.Mutex
	// ackSubs is the in-flight waiter registry, fanning out one ack
	// to every concurrent CancelRemote caller for the same agent_id.
	// Slice (not single chan) closes the v0.12.2 review-1 finding #2:
	// with a single shared channel, the second caller for a given
	// agent_id always timed out because only one receiver gets the
	// buffered ack. Each CancelRemote now registers its own buffered
	// chan; RunAckSubscriber iterates the slice and delivers to every
	// waiter.
	ackSubs map[string][]chan cancelAckPayload
}

// CancelCoordinatorConfig is the constructor input.
type CancelCoordinatorConfig struct {
	Backplane    Backplane
	ReplicaID    string
	Store        cancelRunStore
	ReplicaStore *ReplicaStore
	// AckTimeout caps how long CancelRemote waits for the owning
	// replica's ack publish. Default 5s if zero.
	AckTimeout time.Duration
}

// cancelRunStore is the narrow surface CancelCoordinator needs from
// the store layer. *storepostgres.Store satisfies it implicitly.
// Declared here so the test suite can stub without dragging the full
// store.Store interface.
type cancelRunStore interface {
	GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error)
	FinishRun(ctx context.Context, runID string, status store.RunStatus, stopReason string, usage store.Usage, errMsg string) error
}

// staleReplicaThreshold is the dead-owner cutoff. 3× the 30s
// heartbeat interval = 90s. A replica whose last_heartbeat_at is
// older than this is presumed dead and CancelRemote marks the run
// failed without a broadcast.
const staleReplicaThreshold = 90 * time.Second

const (
	topicCancel    = "loomcycle.cancel"
	topicCancelAck = "loomcycle.cancel.ack"
)

func NewCancelCoordinator(cfg CancelCoordinatorConfig) (*CancelCoordinator, error) {
	if cfg.Backplane == nil {
		return nil, errors.New("coord: backplane is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("coord: store is required")
	}
	if cfg.ReplicaStore == nil {
		return nil, errors.New("coord: replica store is required")
	}
	if err := ValidateReplicaID(cfg.ReplicaID); err != nil {
		return nil, err
	}
	timeout := cfg.AckTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &CancelCoordinator{
		bp:         cfg.Backplane,
		replicaID:  cfg.ReplicaID,
		store:      cfg.Store,
		replicas:   cfg.ReplicaStore,
		ackTimeout: timeout,
		ackSubs:    make(map[string][]chan cancelAckPayload),
	}, nil
}

type cancelEventPayload struct {
	AgentID     string `json:"agent_id"`
	Reason      string `json:"reason,omitempty"`
	FromReplica string `json:"from_replica"`
}

type cancelAckPayload struct {
	AgentID   string   `json:"agent_id"`
	ByReplica string   `json:"by_replica"`
	Cascaded  []string `json:"cascaded,omitempty"`
}

// CancelRemote satisfies cancel.ClusterCanceller. Returns the tuple
// the cancel registry needs: (CancelResult, registry_hit, err).
// "registry_hit" semantics: true means we found something
// authoritative (a DB row, dead or alive); the handler treats this
// as "no fall-through to store needed." false means the run row
// doesn't exist and the handler should serve 404 via the store path.
func (c *CancelCoordinator) CancelRemote(ctx context.Context, agentID, reason string) (cancel.CancelResult, bool, error) {
	run, err := c.store.GetRunByAgentID(ctx, agentID)
	if err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			// 404: handler falls through to store, which will also
			// 404. The empty CancelResult signals miss.
			return cancel.CancelResult{}, false, nil
		}
		return cancel.CancelResult{}, false, fmt.Errorf("get run by agent_id: %w", err)
	}
	// Idempotent terminal case — no broadcast needed.
	if run.Status != store.RunRunning && run.Status != store.RunStatus("") {
		return cancel.CancelResult{Cancelled: false, Reason: reason}, true, nil
	}
	// Single-replica row stamped before v0.12.2, or a stamp that
	// somehow points at this replica (registry should have caught the
	// latter — log a warning either way). Treat as un-routable.
	if run.ReplicaID == "" {
		return cancel.CancelResult{}, false, nil
	}
	if run.ReplicaID == c.replicaID {
		log.Printf("coord: CancelRemote called for run %s owned by self (%s) — local registry missed it; treating as deregistered", run.ID, c.replicaID)
		return cancel.CancelResult{}, false, nil
	}

	// Check owner liveness before broadcasting. A dead owner triggers
	// the "mark failed in DB + return success" short-circuit.
	alive, err := c.replicas.IsReplicaAlive(ctx, run.ReplicaID, staleReplicaThreshold)
	if err != nil {
		// Liveness probe failure: log and continue with broadcast. A
		// real cancel may still succeed if the replica responds.
		log.Printf("coord: IsReplicaAlive probe for %s failed: %v (proceeding with broadcast)", run.ReplicaID, err)
	} else if !alive {
		// Owner is gone. Mark the run failed and return success.
		if ferr := c.store.FinishRun(ctx, run.ID, store.RunFailed, "owner_replica_dead", store.Usage{}, "owner replica heartbeat stale; marked failed by cancel handler"); ferr != nil {
			log.Printf("coord: mark run %s failed after dead-owner detection: %v", run.ID, ferr)
		}
		return cancel.CancelResult{Cancelled: true, Reason: "owner_dead_marked_failed"}, true, nil
	}

	// Live owner: broadcast + wait for ack.
	//
	// Each CancelRemote gets its OWN buffered channel — concurrent
	// callers for the same agent_id each receive the ack independently
	// (review-1 finding #2). RunAckSubscriber fans out to every chan
	// in the slice at delivery time.
	ackCh := make(chan cancelAckPayload, 1)
	c.mu.Lock()
	c.ackSubs[agentID] = append(c.ackSubs[agentID], ackCh)
	c.mu.Unlock()
	defer func() {
		// Remove only this caller's channel from the slice — leave
		// the entry in place if other concurrent callers are still
		// waiting. When the slice empties, delete the map key.
		c.mu.Lock()
		slice := c.ackSubs[agentID]
		for i, ch := range slice {
			if ch == ackCh {
				slice = append(slice[:i], slice[i+1:]...)
				break
			}
		}
		if len(slice) == 0 {
			delete(c.ackSubs, agentID)
		} else {
			c.ackSubs[agentID] = slice
		}
		c.mu.Unlock()
	}()

	payload, _ := json.Marshal(cancelEventPayload{
		AgentID:     agentID,
		Reason:      reason,
		FromReplica: c.replicaID,
	})
	if err := c.bp.Publish(ctx, topicCancel, payload); err != nil {
		return cancel.CancelResult{}, false, fmt.Errorf("publish cancel: %w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), c.ackTimeout)
	defer waitCancel()
	select {
	case ack := <-ackCh:
		return cancel.CancelResult{
			Cancelled: true,
			Reason:    reason,
			Cascaded:  ack.Cascaded,
		}, true, nil
	case <-waitCtx.Done():
		// Timeout. Before returning unreachable, re-check the run row
		// — it may have completed naturally during our wait. Use a
		// short fresh context for the re-check.
		recheckCtx, recheckCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer recheckCancel()
		if run2, err := c.store.GetRunByAgentID(recheckCtx, agentID); err == nil && run2.Status != "" && run2.Status != store.RunRunning {
			return cancel.CancelResult{Cancelled: false, Reason: string(run2.Status)}, true, nil
		}
		return cancel.CancelResult{Cancelled: false, Reason: "owner_replica_unreachable"}, true, nil
	}
}

// RunCancelSubscriber is the owning-side listener. Started once per
// replica via `go coordinator.RunCancelSubscriber(bgCtx)`. Reads from
// `loomcycle.cancel`, dispatches to the local registry, and publishes
// an ack on `loomcycle.cancel.ack` when the agent was found locally.
//
// reg is the local cancel.Registry — passed in (not held as a field)
// because Coordinator is constructed before the Server's registry
// exists.
func (c *CancelCoordinator) RunCancelSubscriber(ctx context.Context, reg *cancel.Registry) {
	ch, err := c.bp.Subscribe(ctx, topicCancel)
	if err != nil {
		log.Printf("coord: RunCancelSubscriber subscribe failed: %v", err)
		return
	}
	for evt := range ch {
		var p cancelEventPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			log.Printf("coord: malformed cancel event: %v", err)
			continue
		}
		// CRITICAL: use CancelLocal, NOT Cancel. Cancel delegates to
		// the ClusterCanceller on local miss, which would re-broadcast
		// the same event we just received — causing a O(replicas ×
		// ack_timeout) broadcast storm of cancel events nobody can
		// honor (review-1 finding #1). CancelLocal never delegates.
		// On local miss we silently skip; the owning replica's own
		// subscriber will pick it up.
		res, ok := reg.CancelLocal(p.AgentID, p.Reason)
		if !ok {
			continue
		}
		ack, _ := json.Marshal(cancelAckPayload{
			AgentID:   p.AgentID,
			ByReplica: c.replicaID,
			Cascaded:  res.Cascaded,
		})
		if err := c.bp.Publish(ctx, topicCancelAck, ack); err != nil {
			log.Printf("coord: publish cancel ack for %s: %v", p.AgentID, err)
		}
	}
}

// RunAckSubscriber is the originator-side ack fan-in. Started once
// per replica. Reads from `loomcycle.cancel.ack` and routes each ack
// to the matching in-flight CancelRemote waiter (by agent_id).
func (c *CancelCoordinator) RunAckSubscriber(ctx context.Context) {
	ch, err := c.bp.Subscribe(ctx, topicCancelAck)
	if err != nil {
		log.Printf("coord: RunAckSubscriber subscribe failed: %v", err)
		return
	}
	for evt := range ch {
		var ack cancelAckPayload
		if err := json.Unmarshal(evt.Payload, &ack); err != nil {
			log.Printf("coord: malformed cancel ack: %v", err)
			continue
		}
		c.mu.Lock()
		waiters := c.ackSubs[ack.AgentID]
		// Snapshot under the lock; deliver outside. Snapshot is cheap
		// (slice of channel headers); per-waiter sends mustn't block
		// the mutex for the whole fan-out.
		snapshot := make([]chan cancelAckPayload, len(waiters))
		copy(snapshot, waiters)
		c.mu.Unlock()
		if len(snapshot) == 0 {
			// Ack for an agent we didn't originate a cancel for — a
			// foreign replica handling its own cancel, or a stale
			// payload. Drop silently.
			continue
		}
		// Fan out: every concurrent CancelRemote caller for this
		// agent_id receives the ack independently. Buffered-1 chans
		// + non-blocking send means a duplicate ack (publisher
		// retransmits) is silently dropped per receiver.
		for _, w := range snapshot {
			select {
			case w <- ack:
			default:
				// Already delivered to this waiter.
			}
		}
	}
}

// Compile-time check that we satisfy the cancel package's interface.
var _ cancel.ClusterCanceller = (*CancelCoordinator)(nil)
