package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// dispatchOnComplete fires each of a WebhookDef's on_complete hooks after a
// spawned run has completed (WH-5b). It MIRRORS the scheduler's dispatch in
// internal/scheduler/dispatch.go rather than importing it — the receiver and
// the scheduler already duplicate the WebhookDef/ScheduleDef hook shape
// (config.ScheduledRunHook), so replicating the two wired arms here keeps the
// two surfaces decoupled and avoids a scheduler refactor.
//
// Hooks are best-effort: the run has ALREADY completed, so a hook failure is
// logged and the next hook runs. This function never returns an error, never
// panics, and never affects the accept/reject decision of the inbound request
// (which was made long before the run finished).
//
// runID + agentID are stamped into channel messages for traceability.
// userID selects the scope: when non-empty, channel.publish + memory.set
// land under the user keyspace; otherwise global (agent scope keys on the
// webhook NAME, matching the scheduler's stable-key choice).
func (rec *Receiver) dispatchOnComplete(ctx context.Context, name string, wd config.Webhook, runID, agentID, userID string) {
	for i, h := range wd.OnComplete {
		if err := rec.dispatchOneOnComplete(ctx, name, h, runID, agentID, userID); err != nil {
			rec.logf("webhook %q: on_complete[%d] (%s) failed: %v", name, i, h.Kind, err)
		}
	}
}

func (rec *Receiver) dispatchOneOnComplete(ctx context.Context, name string, h config.ScheduledRunHook, runID, agentID, userID string) error {
	switch h.Kind {
	case "channel.publish":
		return rec.dispatchOnCompleteChannelPublish(ctx, name, h, runID, agentID, userID)
	case "memory.set":
		return rec.dispatchOnCompleteMemorySet(ctx, name, h, userID)
	case "mcp.call":
		// The receiver has no MCPCaller wired (exactly like the scheduler's
		// nil case). Log + skip — never fail the (already-completed) run.
		rec.logf("webhook %q: on_complete mcp.call not wired (no MCPCaller)", name)
		return nil
	case "":
		// Defensive: validation (WH-1/WH-2) should have rejected a kind-less
		// hook at write time. Log + skip rather than fail.
		rec.logf("webhook %q: on_complete hook missing kind — skipping", name)
		return nil
	default:
		rec.logf("webhook %q: on_complete unknown hook kind %q — skipping", name, h.Kind)
		return nil
	}
}

// dispatchOnCompleteChannelPublish mirrors scheduler.dispatchChannelPublish.
// The published payload carries the webhook name + run/agent ids alongside
// the operator-declared hook payload so a downstream consumer can correlate
// the delivery to the run that produced it.
func (rec *Receiver) dispatchOnCompleteChannelPublish(ctx context.Context, name string, h config.ScheduledRunHook, runID, agentID, userID string) error {
	if h.Channel == "" {
		return fmt.Errorf("channel.publish missing `channel`")
	}
	if rec.store == nil {
		return fmt.Errorf("channel.publish hook fired but no store wired")
	}
	payload, err := json.Marshal(map[string]any{
		"webhook_name": name,
		"run_id":       runID,
		"agent_id":     agentID,
		"payload":      h.Payload,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	scope := store.MemoryScopeGlobal
	scopeID := ""
	if userID != "" {
		scope = store.MemoryScopeUser
		scopeID = userID
	}
	msg := store.ChannelMessage{
		Channel:           h.Channel,
		Scope:             scope,
		ScopeID:           scopeID,
		Payload:           payload,
		PublishedAt:       rec.now(),
		PublishedByUserID: userID,
	}
	// maxMessages = 0 means use the store's default cap (operator sizing is a
	// per-channel cfg concern the publish path doesn't see), mirroring the
	// scheduler.
	_, _, err = rec.store.ChannelPublish(ctx, msg, 0)
	return err
}

// dispatchOnCompleteMemorySet mirrors scheduler.dispatchMemorySet. agent
// scope keys on the webhook NAME (a stable, operator-recognisable key, since
// a webhook hook doesn't run "as an agent" the way an in-loop tool call
// does); user scope requires a userID; global has no scope id.
func (rec *Receiver) dispatchOnCompleteMemorySet(ctx context.Context, name string, h config.ScheduledRunHook, userID string) error {
	if h.Scope == "" || h.Key == "" {
		return fmt.Errorf("memory.set missing `scope` or `key`")
	}
	if rec.store == nil {
		return fmt.Errorf("memory.set hook fired but no store wired")
	}
	var scope store.MemoryScope
	scopeID := ""
	switch h.Scope {
	case "agent":
		scope = store.MemoryScopeAgent
		scopeID = name
	case "user":
		if userID == "" {
			return fmt.Errorf("memory.set scope=user but delivery has no user_id")
		}
		scope = store.MemoryScopeUser
		scopeID = userID
	case "global":
		scope = store.MemoryScopeGlobal
	default:
		return fmt.Errorf("memory.set: unknown scope %q (must be agent|user|global)", h.Scope)
	}
	value, err := json.Marshal(h.Payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	// ttl = 0 means no expiry, mirroring the scheduler's memory.set hook.
	return rec.store.MemorySet(ctx, scope, scopeID, h.Key, value, time.Duration(0))
}
