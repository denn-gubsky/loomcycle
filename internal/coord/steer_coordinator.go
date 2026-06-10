package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SteerCoordinator implements steer.ClusterSteerer via the v0.12.0 Backplane —
// the cross-replica twin of CancelCoordinator, for operator mid-run steering
// (POST /v1/runs/{run_id}/input). One instance per replica, wired in main.go's
// cluster block. When the local steer registry misses (the run isn't on this
// replica), the registry delegates to PushRemote which:
//
//  1. Looks up the run row by run_id for its owning replica_id.
//  2. Short-circuits: unknown run / terminal run / unstamped / self-owned /
//     dead owner all return "not reachably-live" (the handler serves 404).
//  3. Publishes a `loomcycle.steer` event {run_id, text, source, from_replica}.
//  4. Waits on a per-run ack channel for up to AckTimeout. On ack: returns the
//     owner's delivered bool. On timeout: returns found=true, delivered=false
//     (the operator sees "not delivered" and can retry).
//
// Two long-lived subscriber goroutines carry the wire side: RunSteerSubscriber
// (owning side: dispatches incoming steers to the LOCAL registry and acks) and
// RunSteerAckSubscriber (originator side: routes acks to in-flight PushRemote
// waiters). Keyed by run_id (steering targets one run), unlike cancel's
// agent_id.
type SteerCoordinator struct {
	bp         Backplane
	replicaID  string
	store      steerRunStore
	replicas   ReplicaLiveness
	ackTimeout time.Duration

	mu sync.Mutex
	// ackSubs fans one ack out to every concurrent PushRemote waiter for the
	// same run_id (same per-caller-channel pattern as CancelCoordinator).
	ackSubs map[string][]chan steerAckPayload
}

// SteerCoordinatorConfig is the constructor input.
type SteerCoordinatorConfig struct {
	Backplane    Backplane
	ReplicaID    string
	Store        steerRunStore
	ReplicaStore ReplicaLiveness
	// AckTimeout caps how long PushRemote waits for the owning replica's ack.
	// Default 5s if zero.
	AckTimeout time.Duration
}

// steerRunStore is the narrow store surface PushRemote needs. *storepostgres.Store
// satisfies it implicitly. Run lookup is by run_id (steering's key).
type steerRunStore interface {
	GetRun(ctx context.Context, runID string) (store.Run, error)
}

// ReplicaLiveness is the owner-liveness probe PushRemote needs. *ReplicaStore
// satisfies it; declared as an interface so the unit test can stub it without
// a Postgres fixture.
type ReplicaLiveness interface {
	IsReplicaAlive(ctx context.Context, replicaID string, threshold time.Duration) (bool, error)
}

const (
	topicSteer    = "loomcycle.steer"
	topicSteerAck = "loomcycle.steer.ack"
)

func NewSteerCoordinator(cfg SteerCoordinatorConfig) (*SteerCoordinator, error) {
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
	return &SteerCoordinator{
		bp:         cfg.Backplane,
		replicaID:  cfg.ReplicaID,
		store:      cfg.Store,
		replicas:   cfg.ReplicaStore,
		ackTimeout: timeout,
		ackSubs:    make(map[string][]chan steerAckPayload),
	}, nil
}

type steerEventPayload struct {
	RunID       string `json:"run_id"`
	Text        string `json:"text"`
	Source      string `json:"source,omitempty"`
	FromReplica string `json:"from_replica"`
}

type steerAckPayload struct {
	RunID     string `json:"run_id"`
	ByReplica string `json:"by_replica"`
	Delivered bool   `json:"delivered"`
}

// PushRemote satisfies steer.ClusterSteerer. found=false ⇒ the handler serves
// a 404 (no reachably-live run). found=true ⇒ the run was reached; delivered
// reports whether the owner's per-run buffer accepted it.
func (c *SteerCoordinator) PushRemote(ctx context.Context, runID string, m steer.Message) (bool, bool, error) {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("get run: %w", err)
	}
	// Only a running run is steerable. Terminal / unstamped / self-owned all
	// mean "not reachably-live here" → 404 (the local registry already missed,
	// so a self-owned row means the run ended).
	if run.Status != store.RunRunning && run.Status != store.RunStatus("") {
		return false, false, nil
	}
	if run.ReplicaID == "" || run.ReplicaID == c.replicaID {
		return false, false, nil
	}
	if alive, err := c.replicas.IsReplicaAlive(ctx, run.ReplicaID, staleReplicaThreshold); err != nil {
		// Probe failure: proceed with the broadcast — a real steer may still
		// land if the owner responds.
		log.Printf("coord: steer IsReplicaAlive probe for %s failed: %v (proceeding)", run.ReplicaID, err)
	} else if !alive {
		return false, false, nil
	}

	// Live remote owner: broadcast + wait for ack. Each PushRemote gets its own
	// buffered channel; RunSteerAckSubscriber fans out to every waiter.
	ackCh := make(chan steerAckPayload, 1)
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

	payload, _ := json.Marshal(steerEventPayload{
		RunID:       runID,
		Text:        m.Text,
		Source:      m.Source,
		FromReplica: c.replicaID,
	})
	if err := c.bp.Publish(ctx, topicSteer, payload); err != nil {
		return false, false, fmt.Errorf("publish steer: %w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), c.ackTimeout)
	defer waitCancel()
	select {
	case ack := <-ackCh:
		return ack.Delivered, true, nil
	case <-waitCtx.Done():
		// Owner didn't ack in time. The run row says running on a live remote
		// replica, so report reached-but-not-delivered (operator can retry)
		// rather than a hard error or a misleading 404.
		return false, true, nil
	}
}

// RunSteerSubscriber is the owning-side listener. Started once per replica.
// Reads `loomcycle.steer`, dispatches to the LOCAL registry, and acks when the
// run is owned here.
//
// CRITICAL: uses PushLocal (never the cluster-delegating Push) so a local miss
// doesn't re-broadcast the event we just received (the CancelLocal lesson —
// avoids a broadcast storm). On local miss we skip silently; the owning
// replica's own subscriber handles it.
func (c *SteerCoordinator) RunSteerSubscriber(ctx context.Context, reg *steer.Registry) {
	ch, err := c.bp.Subscribe(ctx, topicSteer)
	if err != nil {
		log.Printf("coord: RunSteerSubscriber subscribe failed: %v", err)
		return
	}
	for evt := range ch {
		var p steerEventPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			log.Printf("coord: malformed steer event: %v", err)
			continue
		}
		delivered, found := reg.PushLocal(p.RunID, steer.Message{Text: p.Text, Source: p.Source, EnqueuedAt: time.Now()})
		if !found {
			continue // not our run; the owner's subscriber will ack
		}
		ack, _ := json.Marshal(steerAckPayload{RunID: p.RunID, ByReplica: c.replicaID, Delivered: delivered})
		if err := c.bp.Publish(ctx, topicSteerAck, ack); err != nil {
			log.Printf("coord: publish steer ack for %s: %v", p.RunID, err)
		}
	}
}

// RunSteerAckSubscriber is the originator-side ack fan-in. Started once per
// replica. Routes each ack to the matching in-flight PushRemote waiter(s).
func (c *SteerCoordinator) RunSteerAckSubscriber(ctx context.Context) {
	ch, err := c.bp.Subscribe(ctx, topicSteerAck)
	if err != nil {
		log.Printf("coord: RunSteerAckSubscriber subscribe failed: %v", err)
		return
	}
	for evt := range ch {
		var ack steerAckPayload
		if err := json.Unmarshal(evt.Payload, &ack); err != nil {
			log.Printf("coord: malformed steer ack: %v", err)
			continue
		}
		c.mu.Lock()
		waiters := c.ackSubs[ack.RunID]
		snapshot := make([]chan steerAckPayload, len(waiters))
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

// Compile-time check that we satisfy the steer package's interface.
var _ steer.ClusterSteerer = (*SteerCoordinator)(nil)
