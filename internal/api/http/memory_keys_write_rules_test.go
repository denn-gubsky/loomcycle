package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The memory key routes and the Memory tool write the same keyspace, so they
// refuse the same keys: the routes apply the tool's write rule, not a copy of
// it. A namespace the server alone writes (conversation-turn traces, Document
// chunk bodies and derived units) is refused for every caller.

// TestMemoryKeyPut_TraceTurnRefusedForEveryCaller — a trace.turn: row is written
// by the server when it indexes a conversation turn; a row placed there by hand
// would take the trace class's retrieval rules.
func TestMemoryKeyPut_TraceTurnRefusedForEveryCaller(t *testing.T) {
	c := newACLSearchCorpus(t)
	for name, p := range everyCaller() {
		key := "trace.turn:sess-1:1700000000"
		rec := memoryKeyWrite(t, c.s, p, http.MethodPut, key, `{"value":"planted turn"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"reserved_key"`) {
			t.Errorf("%s PUT %s: %d %s, want 400 reserved_key", name, key, rec.Code, rec.Body.String())
		}
		var nf *store.ErrNotFound
		if _, err := c.s.store.MemoryGet(context.Background(), "A", store.MemoryScopeUser, "alice", key); !errors.As(err, &nf) {
			t.Errorf("%s PUT %s: the row was written (err %v)", name, key, err)
		}
	}
}

// TestMemoryKeyWrites_RefuseExactlyWhatTheToolRefuses — per namespace, every
// value-writing op of the Memory tool refuses the same keys, and a PUT and a
// DELETE on the key route are refused exactly when the tool's set and delete
// are, for a caller with no run (the core-block rule reads the run's declared
// blocks, which an off-run caller does not carry). A member's core/ write is
// refused by the route's own operator rule, which
// TestMemoryKeyRoutes_CoreKeysAreOperatorOnly covers.
func TestMemoryKeyWrites_RefuseExactlyWhatTheToolRefuses(t *testing.T) {
	c := newACLSearchCorpus(t)
	unit := memrank.UnitKey(c.unit, memrank.UnitClaim, 0)
	// keys names each namespace; a key with no row yet gets a per-op suffix, so
	// one op's write cannot change what the next op meets.
	keys := []struct {
		base    string
		hasRow  bool
		refused bool
	}{
		{"trace.turn:sess-1:1700000000", false, true},
		{"doc.chunk:" + c.report, true, true}, // prose body
		{"doc.chunk:" + c.fact, true, true},   // a consolidated fact's body
		{"doc.chunk:never-written", false, true},
		{unit, true, true},
		{"core/persona", false, false},
		{"memory/decision/plain", false, false},
		{"notes/plain", false, false},
	}
	keyFor := func(base string, hasRow bool, op string) string {
		if hasRow || base == "doc.chunk:never-written" {
			return base
		}
		return base + "-" + op
	}
	tool := &builtin.Memory{Store: c.s.store}
	toolCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "A", UserID: "alice"})
	toolCtx = tools.UpdateRunPolicy(toolCtx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
	})
	valueOps := map[string]map[string]any{
		"set":           {"value": "probe"},
		"incr":          {"delta": 1},
		"merge":         {"value": map[string]any{"a": 1}},
		"append_dedupe": {"value": "probe"},
		"bounded_list":  {"value": "probe", "limit": 3},
	}
	toolRefuses := func(op, key string) bool {
		t.Helper()
		in := map[string]any{"op": op, "scope": "user", "key": key}
		for k, v := range valueOps[op] {
			in[k] = v
		}
		b, _ := json.Marshal(in)
		res, err := tool.Execute(toolCtx, b)
		if err != nil {
			t.Fatalf("tool %s %s: %v", op, key, err)
		}
		return res.IsError
	}
	// The tool's own answers, pinned for every value op, so a misconfigured
	// tool that refused everything (or nothing) could not make the comparison
	// pass, and no op can refuse a different set of keys than set does.
	for op := range valueOps {
		for _, k := range keys {
			key := keyFor(k.base, k.hasRow, op)
			if got := toolRefuses(op, key); got != k.refused {
				t.Errorf("the tool's %s on %s refuses=%v, want %v", op, key, got, k.refused)
			}
		}
	}
	for name, p := range everyCaller() {
		for _, k := range keys {
			if name == "member" && strings.HasPrefix(k.base, "core/") {
				continue
			}
			key := keyFor(k.base, k.hasRow, "put")
			rec := memoryKeyWrite(t, c.s, p, http.MethodPut, key, `{"value":"probe"}`)
			if gotPut := rec.Code == http.StatusBadRequest; gotPut != k.refused || (!k.refused && rec.Code != http.StatusOK) {
				t.Errorf("%s PUT %s: %d %s; the tool's set refuses=%v", name, key, rec.Code, rec.Body.String(), k.refused)
			}
		}
	}
	// Delete last: it removes what it is allowed to.
	for _, k := range keys {
		key := keyFor(k.base, k.hasRow, "set")
		wantDel := toolRefuses("delete", key)
		rec := memoryKeyWrite(t, c.s, everyCaller()["tenant operator"], http.MethodDelete, key, "")
		if gotDel := rec.Code == http.StatusBadRequest; gotDel != wantDel || (!wantDel && rec.Code != http.StatusNoContent) {
			t.Errorf("tenant operator DELETE %s: %d %s; the tool's delete refuses=%v", key, rec.Code, rec.Body.String(), wantDel)
		}
	}
}
