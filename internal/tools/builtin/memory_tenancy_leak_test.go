package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMemoryBackendDefTool_RefusesEmptySharedPrefix pins the authoring-time
// guard against the HIGH cross-tenant leak: a MemoryBackendDef with
// shared_key_with_prefix + an empty prefix_pattern must be refused at create
// (pre-fix the validator's `PrefixPattern != "" &&` precondition let an empty
// pattern pass, and an empty key prefix collapses every tenant into one
// keyspace).
//
// The guard is deliberately kind-INDEPENDENT: no shipped backend kind consumes
// tenancy today, so this is what keeps the persisted shape out of the leaky
// state before any future external kind acts on it.
func TestMemoryBackendDefTool_RefusesEmptySharedPrefix(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"leaky","overlay":{"kind":"inprocess","tenancy_strategy":{"kind":"shared_key_with_prefix"}}}`))
	if !res.IsError {
		t.Fatal("shared_key_with_prefix with empty prefix_pattern should be refused")
	}
	if !strings.Contains(res.Text, "{tenant_id}") {
		t.Errorf("refusal should mention {tenant_id}; got %s", res.Text)
	}
}
