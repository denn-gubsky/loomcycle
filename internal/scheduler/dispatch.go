package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MCPCaller is the narrow interface the scheduler uses to fire
// `mcp.call` hooks. Implemented by the real MCP pool (configured in
// main.go) and by a no-op stub the unit tests inject. Kept as a
// separate interface to avoid a scheduler → internal/tools/mcp
// dependency, which would cycle since the MCP pool itself depends
// on the connector / runtime.
type MCPCaller interface {
	// Call invokes a tool on the named MCP server. Returns the raw
	// JSON result the MCP server emits, or an error. Implementations
	// are responsible for routing — the scheduler doesn't see which
	// MCP server is local vs HTTP.
	Call(ctx context.Context, serverName, toolName string, args map[string]any) (json.RawMessage, error)
}

// dispatchHooks fires each on_complete hook in order. Hooks are
// best-effort: a failed hook is logged and the next one runs.
// Returning an error from this function would block sweeper progress
// for other due schedules, so we collect + log instead.
//
// runID + agentID are stamped into channel messages + memory writes
// for traceability so operators can correlate `last_run_id` on a
// failed schedule to the hook delivery that follow-on consumed.
func (s *Scheduler) dispatchHooks(ctx context.Context, scheduleName string, def scheduleDef, runID, agentID string) {
	for i, h := range def.OnComplete {
		if err := s.dispatchOneHook(ctx, scheduleName, def.UserID, def.Agent, h, runID, agentID); err != nil {
			s.logf("scheduler: schedule %q on_complete[%d] (%s) failed: %v",
				scheduleName, i, h.Kind, err)
		}
	}
}

func (s *Scheduler) dispatchOneHook(ctx context.Context, scheduleName, userID, agentName string, h scheduleHook, runID, agentID string) error {
	switch h.Kind {
	case "channel.publish":
		return s.dispatchChannelPublish(ctx, scheduleName, userID, agentName, h, runID, agentID)
	case "memory.set":
		return s.dispatchMemorySet(ctx, scheduleName, userID, h)
	case "mcp.call":
		return s.dispatchMCPCall(ctx, h)
	case "":
		return fmt.Errorf("hook missing required `kind`")
	default:
		return fmt.Errorf("unknown hook kind %q (must be one of: channel.publish, mcp.call, memory.set)", h.Kind)
	}
}

func (s *Scheduler) dispatchChannelPublish(ctx context.Context, scheduleName, userID, agentName string, h scheduleHook, runID, agentID string) error {
	if h.Channel == "" {
		return fmt.Errorf("channel.publish missing `channel`")
	}
	payload, err := json.Marshal(map[string]any{
		"schedule_name": scheduleName,
		"run_id":        runID,
		"agent_id":      agentID,
		"payload":       h.Payload,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	scope, scopeID, err := s.resolvePublishScope(ctx, h.Channel, userID, agentName)
	if err != nil {
		return err
	}
	msg := store.ChannelMessage{
		Channel:           h.Channel,
		Scope:             scope,
		ScopeID:           scopeID,
		Payload:           payload,
		PublishedAt:       time.Now(),
		PublishedByUserID: userID,
	}
	// maxMessages = 0 means use the store's default cap. The scheduler
	// doesn't manage per-channel sizing — that's an operator concern
	// configured via cfg.Channels.<name>.max_messages, which the
	// channel publish path itself doesn't know about. v1.1 will surface
	// a sweeper-side cap if operators report unbounded growth.
	_, _, err = s.store.ChannelPublish(ctx, msg, 0)
	return err
}

// resolvePublishScope returns the (scope, scopeID) an on_complete:
// channel.publish hook should write under.
//
// With a resolver wired (the normal path), it honors the channel's DECLARED
// scope (F37 / RFC T): a `scope: global` channel publishes at global/"" so a
// global reader (admin peek, Channel.await/subscribe resolving global) can
// see it — not under the run's user scope where it would be invisible. The
// run's userID / agentName fill scope_id for user / agent channels. An
// undeclared channel is an operator error and fails the hook loudly (logged
// by dispatchHooks) rather than silently mis-scoping.
//
// With no resolver (nil — small embeds / tests that don't wire one), it
// falls back to the legacy behavior: user scope when the schedule has a
// user_id, else global.
func (s *Scheduler) resolvePublishScope(ctx context.Context, channel, userID, agentName string) (store.MemoryScope, string, error) {
	if s.chScope == nil {
		if userID != "" {
			return store.MemoryScopeUser, userID, nil
		}
		return store.MemoryScopeGlobal, "", nil
	}
	declared, ok := s.chScope(ctx, channel)
	if !ok {
		return "", "", fmt.Errorf("channel.publish: channel %q is not declared (static yaml or runtime substrate)", channel)
	}
	switch declared {
	case "global":
		return store.MemoryScopeGlobal, "", nil
	case "user":
		if userID == "" {
			return "", "", fmt.Errorf("channel.publish: channel %q has scope=user but schedule has no user_id", channel)
		}
		return store.MemoryScopeUser, userID, nil
	case "agent":
		if agentName == "" {
			return "", "", fmt.Errorf("channel.publish: channel %q has scope=agent but schedule has no agent", channel)
		}
		return store.MemoryScopeAgent, agentName, nil
	default:
		return "", "", fmt.Errorf("channel.publish: channel %q has unknown scope %q", channel, declared)
	}
}

func (s *Scheduler) dispatchMemorySet(ctx context.Context, scheduleName, userID string, h scheduleHook) error {
	if h.Scope == "" || h.Key == "" {
		return fmt.Errorf("memory.set missing `scope` or `key`")
	}
	var scope store.MemoryScope
	scopeID := ""
	switch h.Scope {
	case "agent":
		// Agent-scoped memory uses the agent name as scope_id, but
		// scheduler-driven hooks don't run "as an agent" the way an
		// in-loop tool call does. Using scheduleName here gives a
		// stable, operator-recognisable key. Operators wanting strict
		// agent-scope can use `user` scope with the run's user_id
		// instead.
		scope = store.MemoryScopeAgent
		scopeID = scheduleName
	case "user":
		if userID == "" {
			return fmt.Errorf("memory.set scope=user but schedule has no user_id")
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
	return s.store.MemorySet(ctx, "", scope, scopeID, h.Key, value, 0)
}

func (s *Scheduler) dispatchMCPCall(ctx context.Context, h scheduleHook) error {
	if h.Server == "" || h.Tool == "" {
		return fmt.Errorf("mcp.call missing `server` or `tool`")
	}
	if s.mcp == nil {
		return fmt.Errorf("mcp.call hook fired but no MCPCaller wired (operator must enable MCP pool)")
	}
	_, err := s.mcp.Call(ctx, h.Server, h.Tool, h.Args)
	return err
}
