package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/turncancel"
)

// TurnCancelCoordinator implements turncancel.ClusterCanceller via the v0.12.0
// Backplane — the cross-replica twin of SteerCoordinator, for operator
// turn-cancel (POST /v1/runs/{run_id}/cancel, RFC BH P3a). One instance per
// replica, wired in main.go's cluster block. When the local turn-cancel registry
// misses (the run isn't armed on this replica), the registry delegates to
// CancelRemote which:
//
//  1. Looks up the run row by run_id for its owning replica_id.
//  2. Short-circuits: unknown run / terminal run / unstamped / self-owned /
//     dead owner all return found=false (the handler serves a not-in-flight 404).
//  3. Publishes a `loomcycle.turncancel` event {run_id, reason, from_replica}.
//  4. Waits on a per-run ack channel for up to AckTimeout. An ack means the owner
//     had the run armed mid-turn and fired it → found=true. A timeout means no
//     replica fired it (already parked / just ended) → found=false.
//
// Two long-lived subscriber goroutines carry the wire side: RunTurnCancelSubscriber
// (owning side: fires the LOCAL armed token via the registry and acks only when it
// fired) and RunTurnCancelAckSubscriber (originator side: routes acks to in-flight
// CancelRemote waiters). Keyed by run_id, like steer.
type TurnCancelCoordinator struct {
	bp         Backplane
	replicaID  string
	store      turnCancelRunStore
	replicas   ReplicaLiveness
	ackTimeout time.Duration

	mu sync.Mutex
	// ackSubs fans one ack out to every concurrent CancelRemote waiter for the
	// same run_id (same per-caller-channel pattern as SteerCoordinator).
	ackSubs map[string][]chan turnCancelAckPayload
}

// TurnCancelCoordinatorConfig is the constructor input.
type TurnCancelCoordinatorConfig struct {
	Backplane    Backplane
	ReplicaID    string
	Store        turnCancelRunStore
	ReplicaStore ReplicaLiveness
	// AckTimeout caps how long CancelRemote waits for the owning replica's ack.
	// Default 5s if zero.
	AckTimeout time.Duration
}

// turnCancelRunStore is the narrow store surface CancelRemote needs.
// *storepostgres.Store satisfies it implicitly. Run lookup is by run_id.
type turnCancelRunStore interface {
	GetRun(ctx context.Context, runID string) (store.Run, error)
}

const (
	topicTurnCancel    = "loomcycle.turncancel"
	topicTurnCancelAck = "loomcycle.turncancel.ack"
)

func NewTurnCancelCoordinator(cfg TurnCancelCoordinatorConfig) (*TurnCancelCoordinator, error) {
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
	return &TurnCancelCoordinator{
		bp:         cfg.Backplane,
		replicaID:  cfg.ReplicaID,
		store:      cfg.Store,
		replicas:   cfg.ReplicaStore,
		ackTimeout: timeout,
		ackSubs:    make(map[string][]chan turnCancelAckPayload),
	}, nil
}

type turnCancelEventPayload struct {
	RunID       string `json:"run_id"`
	Reason      string `json:"reason,omitempty"`
	FromReplica string `json:"from_replica"`
}

type turnCancelAckPayload struct {
	RunID     string `json:"run_id"`
	ByReplica string `json:"by_replica"`
	// Fired is always true on the wire (the owner acks only when it fired the
	// armed token). Carried explicitly for symmetry with the steer ack + so a
	// future not-fired ack is a non-breaking extension.
	Fired bool `json:"fired"`
}

// CancelRemote satisfies turncancel.ClusterCanceller. found=false ⇒ no replica
// fired a mid-turn cancel for the run (unknown / terminal / self-owned / dead
// owner / not mid-turn on the owner). found=true ⇒ the owning replica had the
// run armed and fired it.
func (c *TurnCancelCoordinator) CancelRemote(ctx context.Context, runID, reason string) (bool, error) {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("get run: %w", err)
	}
	// Only a running run can be mid-turn. Terminal / unstamped / self-owned all
	// mean "not reachably mid-turn here" (the local registry already missed, so a
	// self-owned row means the turn isn't armed on this replica).
	if run.Status != store.RunRunning && run.Status != store.RunStatus("") {
		return false, nil
	}
	if run.ReplicaID == "" || run.ReplicaID == c.replicaID {
		return false, nil
	}
	if alive, err := c.replicas.IsReplicaAlive(ctx, run.ReplicaID, staleReplicaThreshold); err != nil {
		// Probe failure: proceed with the broadcast — a real cancel may still
		// land if the owner responds.
		log.Printf("coord: turncancel IsReplicaAlive probe for %s failed: %v (proceeding)", run.ReplicaID, err)
	} else if !alive {
		return false, nil
	}

	// Live remote owner: broadcast + wait for ack. Each CancelRemote gets its own
	// buffered channel; RunTurnCancelAckSubscriber fans out to every waiter.
	ackCh := make(chan turnCancelAckPayload, 1)
	c.mu.Lock()
	c.ackSubs[runID] = append(c.ackSubs[runID], ackCh)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		slice := c.ackSubs[runID]
		for i, ch := range slice {
			if ch == ackCh {
				slice = append(slice[:i], slice[i+1:]...)
				break
			}
		}
		if len(slice) == 0 {
			delete(c.ackSubs, runID)
		} else {
			c.ackSubs[runID] = slice
		}
		c.mu.Unlock()
	}()

	payload, _ := json.Marshal(turnCancelEventPayload{
		RunID:       runID,
		Reason:      reason,
		FromReplica: c.replicaID,
	})
	if err := c.bp.Publish(ctx, topicTurnCancel, payload); err != nil {
		return false, fmt.Errorf("publish turncancel: %w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), c.ackTimeout)
	defer waitCancel()
	select {
	case ack := <-ackCh:
		return ack.Fired, nil
	case <-waitCtx.Done():
		// No ack in the window. The owner acks only when it fired a mid-turn
		// token, so no ack means the run wasn't mid-turn on the owner (already
		// parked / just ended) — report not-fired so the handler serves 404.
		return false, nil
	}
}

// RunTurnCancelSubscriber is the owning-side listener. Started once per replica.
// Reads `loomcycle.turncancel`, fires the LOCAL armed token via the registry, and
// acks only when it actually fired.
//
// CRITICAL: uses CancelLocal (never the cluster-delegating Cancel) so a local
// miss doesn't re-broadcast the event we just received (the CancelLocal lesson —
// avoids a broadcast storm). A local miss = not our run OR our run but no longer
// mid-turn; either way we skip silently and do not ack, so only the replica that
// fired a live token acks.
func (c *TurnCancelCoordinator) RunTurnCancelSubscriber(ctx context.Context, reg *turncancel.Registry) {
	ch, err := c.bp.Subscribe(ctx, topicTurnCancel)
	if err != nil {
		log.Printf("coord: RunTurnCancelSubscriber subscribe failed: %v", err)
		return
	}
	for evt := range ch {
		var p turnCancelEventPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			log.Printf("coord: malformed turncancel event: %v", err)
			continue
		}
		if !reg.CancelLocal(p.RunID, p.Reason) {
			continue // not armed here; the owning replica's subscriber will ack
		}
		ack, _ := json.Marshal(turnCancelAckPayload{RunID: p.RunID, ByReplica: c.replicaID, Fired: true})
		if err := c.bp.Publish(ctx, topicTurnCancelAck, ack); err != nil {
			log.Printf("coord: publish turncancel ack for %s: %v", p.RunID, err)
		}
	}
}

// RunTurnCancelAckSubscriber is the originator-side ack fan-in. Started once per
// replica. Routes each ack to the matching in-flight CancelRemote waiter(s).
func (c *TurnCancelCoordinator) RunTurnCancelAckSubscriber(ctx context.Context) {
	ch, err := c.bp.Subscribe(ctx, topicTurnCancelAck)
	if err != nil {
		log.Printf("coord: RunTurnCancelAckSubscriber subscribe failed: %v", err)
		return
	}
	for evt := range ch {
		var ack turnCancelAckPayload
		if err := json.Unmarshal(evt.Payload, &ack); err != nil {
			log.Printf("coord: malformed turncancel ack: %v", err)
			continue
		}
		c.mu.Lock()
		waiters := c.ackSubs[ack.RunID]
		snapshot := make([]chan turnCancelAckPayload, len(waiters))
		copy(snapshot, waiters)
		c.mu.Unlock()
		for _, w := range snapshot {
			select {
			case w <- ack:
			default:
			}
		}
	}
}

// Compile-time check that we satisfy the turncancel package's interface.
var _ turncancel.ClusterCanceller = (*TurnCancelCoordinator)(nil)
