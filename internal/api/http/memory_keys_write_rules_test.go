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

// TestMemoryKeyWrites_RefuseExactlyWhatTheToolRefuses — per namespace, a PUT
// and a DELETE on the key route are refused exactly when the Memory tool's set
// and delete are, for a caller with no run (the core-block rule reads the run's
// declared blocks, which an off-run caller does not carry, so core/ writes).
func TestMemoryKeyWrites_RefuseExactlyWhatTheToolRefuses(t *testing.T) {
	c := newACLSearchCorpus(t)
	keys := []string{
		"trace.turn:sess-1:1700000000",
		"doc.chunk:" + c.report, // prose body
		"doc.chunk:" + c.fact,   // a consolidated fact's body
		"doc.chunk:never-written",
		memrank.UnitKey(c.unit, memrank.UnitClaim, 0),
		"core/persona",
		"memory/decision/plain",
		"notes/plain",
	}
	tool := &builtin.Memory{Store: c.s.store}
	toolCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "A", UserID: "alice"})
	toolCtx = tools.UpdateRunPolicy(toolCtx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
	})
	toolRefuses := func(op, key string) bool {
		t.Helper()
		in := map[string]any{"op": op, "scope": "user", "key": key}
		if op == "set" {
			in["value"] = "probe"
		}
		b, _ := json.Marshal(in)
		res, err := tool.Execute(toolCtx, b)
		if err != nil {
			t.Fatalf("tool %s %s: %v", op, key, err)
		}
		return res.IsError
	}
	// The tool's own answers, pinned, so a misconfigured tool that refused
	// everything (or nothing) could not make the comparison pass.
	wantSet := map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true, keys[3]: true, keys[4]: true,
		keys[5]: false, keys[6]: false, keys[7]: false}
	for _, key := range keys {
		if got := toolRefuses("set", key); got != wantSet[key] {
			t.Fatalf("the tool's set on %s refuses=%v, want %v", key, got, wantSet[key])
		}
	}
	for name, p := range everyCaller() {
		for _, key := range keys {
			wantPut := toolRefuses("set", key)
			rec := memoryKeyWrite(t, c.s, p, http.MethodPut, key, `{"value":"probe"}`)
			if gotPut := rec.Code == http.StatusBadRequest; gotPut != wantPut || (!wantPut && rec.Code != http.StatusOK) {
				t.Errorf("%s PUT %s: %d %s; the tool's set refuses=%v", name, key, rec.Code, rec.Body.String(), wantPut)
			}
		}
	}
	// Delete last: it removes what it is allowed to.
	for _, key := range keys {
		wantDel := toolRefuses("delete", key)
		rec := memoryKeyWrite(t, c.s, everyCaller()["member"], http.MethodDelete, key, "")
		if gotDel := rec.Code == http.StatusBadRequest; gotDel != wantDel || (!wantDel && rec.Code != http.StatusNoContent) {
			t.Errorf("member DELETE %s: %d %s; the tool's delete refuses=%v", key, rec.Code, rec.Body.String(), wantDel)
		}
	}
}
