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
		if err := rec.dispatchOneOnComplete(ctx, name, wd.TenantID, h, runID, agentID, userID); err != nil {
			rec.logf("webhook %q: on_complete[%d] (%s) failed: %v", name, i, h.Kind, err)
		}
	}
}

func (rec *Receiver) dispatchOneOnComplete(ctx context.Context, name, tenantID string, h config.ScheduledRunHook, runID, agentID, userID string) error {
	switch h.Kind {
	case "channel.publish":
		return rec.dispatchOnCompleteChannelPublish(ctx, name, tenantID, h, runID, agentID, userID)
	case "memory.set":
		return rec.dispatchOnCompleteMemorySet(ctx, name, tenantID, h, userID)
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
func (rec *Receiver) dispatchOnCompleteChannelPublish(ctx context.Context, name, tenantID string, h config.ScheduledRunHook, runID, agentID, userID string) error {
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
		Channel: h.Channel,
		// RFC N: the owning tenant comes from the webhook def (wd.TenantID),
		// threaded down from dispatchOnComplete — never from the hook payload.
		TenantID:          tenantID,
		Scope:             scope,
		ScopeID:           scopeID,
		Payload:           payload,
		PublishedAt:       rec.now(),
		PublishedByUserID: userID,
	}
	// A HELD channel stores the hook's message without delivering it, like every
	// other write to that channel. The rest of the definition — max_messages,
	// default_ttl, the declared scope — is still not consulted here (this path
	// has always passed 0 and derived the scope from the user id); widening that
	// moves where existing messages LAND, which is a separate change. A hold is
	// not: honouring it can only withhold a message the operator asked to be
	// withheld.
	if rec.channelHeld(ctx, tenantID, h.Channel) {
		msg.VisibleAt = store.ChannelHeldVisibleAt()
	}
	_, _, err = rec.store.ChannelPublish(ctx, msg, 0)
	return err
}

// channelHeld reports whether a channel is declared `hold:` — yaml first
// (operator-global, so every tenant sees it), then the tenant's runtime row.
//
// A store fault answers false: an unreachable definition plane must not start
// holding channels nobody declared held. The choice is nearly moot in practice
// — the publish on the next line uses the SAME store, so a fault here means the
// message is not written either — and "don't invent a hold" is the safer
// default for a transient error. A yaml-declared hold never touches the store
// at all.
//
// Mirrors (*http.Server).ChannelHeld, which the in-process publishers use; the
// receiver has its own because it holds a narrow store interface, not a server.
func (rec *Receiver) channelHeld(ctx context.Context, tenantID, channel string) bool {
	if rec.cfg != nil {
		if def, ok := rec.cfg.Channels[channel]; ok {
			return def.Hold
		}
	}
	if rec.store == nil {
		return false
	}
	row, err := rec.store.ChannelGet(ctx, tenantID, channel)
	if err != nil {
		return false
	}
	return row.Hold
}

// dispatchOnCompleteMemorySet mirrors scheduler.dispatchMemorySet. agent
// scope keys on the webhook NAME (a stable, operator-recognisable key, since
// a webhook hook doesn't run "as an agent" the way an in-loop tool call
// does); user scope requires a userID; global has no scope id.
func (rec *Receiver) dispatchOnCompleteMemorySet(ctx context.Context, name, tenantID string, h config.ScheduledRunHook, userID string) error {
	if h.Scope == "" || h.Key == "" {
		return fmt.Errorf("memory.set missing `scope` or `key`")
	}
	if rec.store == nil {
		return fmt.Errorf("memory.set hook fired but no store wired")
	}
	var scope store.MemoryScope
	scopeID := ""
	// RFC BL: agent/user-scoped memory is partitioned by the webhook def's tenant
	// (the spawned run fires as this tenant — sourced from the def row via
	// migration 0044, NOT a run ctx there is none). global scope is the
	// cross-tenant shared partition and stays "".
	memTenant := tenantID
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
		memTenant = ""
	default:
		return fmt.Errorf("memory.set: unknown scope %q (must be agent|user|global)", h.Scope)
	}
	value, err := json.Marshal(h.Payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	// ttl = 0 means no expiry, mirroring the scheduler's memory.set hook.
	return rec.store.MemorySet(ctx, memTenant, scope, scopeID, h.Key, value, time.Duration(0))
}
