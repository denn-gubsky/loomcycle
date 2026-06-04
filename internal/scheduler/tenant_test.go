package scheduler

import "testing"

// TestSchedulerBuildRunInput_ThreadsDefTenant pins that a schedule def's
// TenantID reaches the spawned run as RunInput.TenantID, so the run resolves
// that tenant's agents/skills/MCP and isolates its memory/runs (RFC N
// follow-up). Fails on the pre-feature scheduleDef, which had no TenantID
// field. An empty def tenant must stay "" (shared/default, no scoping).
func TestSchedulerBuildRunInput_ThreadsDefTenant(t *testing.T) {
	def := scheduleDef{Agent: "digest", TenantID: "acme"}
	in := buildRunInput(def, map[string]bool{}, nil)
	if in.TenantID != "acme" {
		t.Errorf("schedule def TenantID not threaded to RunInput.TenantID: got %q, want %q", in.TenantID, "acme")
	}

	empty := scheduleDef{Agent: "digest"}
	if got := buildRunInput(empty, map[string]bool{}, nil).TenantID; got != "" {
		t.Errorf("absent def tenant must stay \"\" (shared/default), got %q", got)
	}
}
