package builtin

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Memory tool's write rules by key, in one place. The tool's set and delete
// call them, and so do PUT and DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key},
// which write the same keyspace: one rule, so the two cannot refuse different keys.

const coreBlockNext = "Write a different key; this core block is operator-configured."

// MemorySetRefusal is the rule for writing a value at key in scope, valueLen
// bytes long (-1 when the op cannot report it up front: incr, merge,
// append_dedupe and bounded_list, which check the size in their reducer). Every
// value-writing op of the tool calls it. Refused for every caller: a derived unit, a chunk body, and a
// conversation-turn trace — namespaces the server or the Document tool writes.
// A declared core block (core/<label>) refuses a write when it is read_only or
// the value exceeds its limit_bytes; the blocks are the ones declared on the
// caller's run (tools.PolicyOf(ctx).CoreBlocks), so a caller with no run
// declares none and a core/ key writes as any other. op names the operation in
// the refusal.
func MemorySetRefusal(ctx context.Context, op string, scope store.MemoryScope, key string, valueLen int) (tools.Result, bool) {
	if res, refused := refuseUnitWrite(op, key); refused {
		return res, true
	}
	if res, refused := refuseChunkBodyWrite(op, key); refused {
		return res, true
	}
	// THE TRACE NAMESPACE IS SERVER-ONLY. A row's class decides whether it reaches an
	// unfiltered search and whether erasure has to find it, so a caller must not be
	// able to elect its own writes into a class with different retrieval semantics —
	// a `trace.turn:` key from a tool call would be hidden from every default search
	// by the very exclusion that makes the class safe.
	//
	// Refused rather than silently re-prefixed: a write that lands somewhere other
	// than where the caller asked is worse than one that does not land.
	if strings.HasPrefix(key, store.TraceTurnKeyPrefix) {
		return errValidation(op+": the "+store.TraceTurnKeyPrefix+" namespace is written by the "+
			"server when it indexes a conversation turn, and cannot be written through this tool — "+
			"use an ordinary key", ""), true
	}
	// RFC BL P1: a write to a reserved `core/<label>` key is gated by the
	// agent's core-block config — read_only refuses the write entirely,
	// limit_bytes caps it (mirroring the quota refusal). Undeclared core/*
	// keys pass through as normal memory.
	if err := enforceCoreBlockWrite(ctx, scope, key, op, valueLen); err != nil {
		return errBusiness(err.Error(), coreBlockNext), true
	}
	return tools.Result{}, false
}

// deleteRefusal is the rule for removing key from (scope, scopeID): a derived
// unit and a prose chunk body are removed through the Document tool (a fact's
// body row and a key with no row pass), and a read_only core block declared on
// the caller's run is not removed. A row that cannot be classified is refused
// with a failure that is not a validation one.
func (m *Memory) deleteRefusal(ctx context.Context, op string, scope store.MemoryScope, scopeID, key string) (tools.Result, bool) {
	if res, refused := m.refuseDocumentRowRemoval(ctx, op, scope, scopeID, key); refused {
		return res, true
	}
	// RFC BL P1: a read_only core block refuses delete too — otherwise an agent
	// could erase an operator-seeded read-only block. limit_bytes is moot for a
	// delete, so pass -1 to skip the size check.
	if err := enforceCoreBlockWrite(ctx, scope, key, op, -1); err != nil {
		return errBusiness(err.Error(), coreBlockNext), true
	}
	return tools.Result{}, false
}

// MemoryDeleteRefusal is the Memory tool's delete rule for a caller with no
// Memory tool to hand: the row is read from st in tenantID.
func MemoryDeleteRefusal(ctx context.Context, st store.Store, tenantID string, scope store.MemoryScope, scopeID, op, key string) (tools.Result, bool) {
	// The chunk-body check reads the tenant off the run identity; this caller has
	// no run, so the identity carries only the tenant the route resolved. The
	// run policy on ctx (the core blocks) is left as it is.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenantID})
	return (&Memory{Store: st}).deleteRefusal(ctx, op, scope, scopeID, key)
}

// IsDocumentRowKey reports whether key lies in the chunk-body or derived-unit
// namespace, where a refused write is redirected to the Document routes.
func IsDocumentRowKey(key string) bool { return inDocumentNamespace(key) }
