package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// A memory read touches exactly ONE partition. That is a settled decision — a
// multi-scope read was specified, argued for, and withdrawn once it turned out that
// merging two top-k responses by score reconstructs the joint top-k exactly, so the
// combined query bought nothing a caller cannot do itself.
//
// What the decision leaves behind is a DISCOVERABILITY problem, and these tests pin
// both halves of it. The behaviour has always been correct; what was missing was any
// way for an agent to learn it other than by getting an empty result and guessing why.

// TestMemoryScope_UserReadDoesNotSeeTenantRows is the invariant the schema now
// advertises. It is asserted here rather than assumed because the schema text is only
// honest if the partitioning actually holds.
func TestMemoryScope_UserReadDoesNotSeeTenantRows(t *testing.T) {
	m, ctx, _ := tenantMemFixture(t, "acme", "alice")

	if r := memExecJSON(t, m, ctx, `{"op":"set","scope":"tenant","key":"policy/releases","value":"two approvals"}`); r.IsError {
		t.Fatalf("seed the tenant row: %s", r.Text)
	}
	if r := memExecJSON(t, m, ctx, `{"op":"set","scope":"user","key":"pref/editor","value":"vim"}`); r.IsError {
		t.Fatalf("seed the user row: %s", r.Text)
	}

	// The user partition must not carry the tenant's row...
	r := memExecJSON(t, m, ctx, `{"op":"list","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list at user scope: %s", r.Text)
	}
	if strings.Contains(r.Text, "policy/releases") {
		t.Errorf("a user-scope list returned a TENANT row — the partitions have leaked into each other: %s", r.Text)
	}
	if !strings.Contains(r.Text, "pref/editor") {
		t.Errorf("a user-scope list must still return the user's own row: %s", r.Text)
	}

	// ...and the converse, which is the half that makes the second call necessary
	// rather than merely tidy.
	r = memExecJSON(t, m, ctx, `{"op":"list","scope":"tenant"}`)
	if r.IsError {
		t.Fatalf("list at tenant scope: %s", r.Text)
	}
	if strings.Contains(r.Text, "pref/editor") {
		t.Errorf("a tenant-scope list returned a USER row: %s", r.Text)
	}
	if !strings.Contains(r.Text, "policy/releases") {
		t.Errorf("a tenant-scope list must return the shared row: %s", r.Text)
	}
}

// TestMemoryScope_SchemaSaysOneScopePerCall guards the description against being
// "simplified" back to what it used to say. The old wording described who could REACH
// the tenant keyspace ("shared by every user and agent in the tenant") and never that a
// separate call is required to read it, which reads as though a user-scope recall covers
// both. An agent that believes that asks once, gets nothing, and concludes the
// organisation knows nothing.
//
// Only the two load-bearing claims are pinned, not the prose around them, so the wording
// stays free to improve.
func TestMemoryScope_SchemaSaysOneScopePerCall(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(memoryInputSchema), &schema); err != nil {
		t.Fatalf("memoryInputSchema is not valid JSON: %v", err)
	}
	desc := strings.ToLower(schema.Properties["scope"].Description)
	if desc == "" {
		t.Fatal("the input schema documents no `scope` property")
	}
	for _, want := range []string{"one scope per call", "two calls"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the scope description must tell an agent %q; a caller who does not know this "+
				"reads an empty tenant result as \"nothing is known\". Got: %s", want, desc)
		}
	}
}
