package snapshot

import (
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Tenant document rows in a snapshot taken before the tenant keyspace fix carry
// the SQL Memory scope id (the tenant, or "default" for the empty tenant), where a
// tenant-scope Memory read never looks. Restore re-keys them to "", the rule
// postgres migration 0113 and store.MigrateTenantDocumentRows apply to a live
// database. Any other scope id is left as captured.

// Restated from internal/memory (a layer above this package);
// TestRestoreDocumentPrefixes_MatchTheMemoryPackage keeps them equal.
const (
	restoreDocumentChunkPrefix = "doc.chunk:"
	restoreDocumentUnitPrefix  = "doc.unit:"
)

// restoredMemoryScopeID is the scope id a captured memory row restores at.
func restoredMemoryScopeID(tenantID string, scope store.MemoryScope, scopeID, key string) string {
	if scope != store.MemoryScopeTenant || scopeID == "" || scopeID != sqlmem.ScopeTenant(tenantID) {
		return scopeID
	}
	if !strings.HasPrefix(key, restoreDocumentChunkPrefix) && !strings.HasPrefix(key, restoreDocumentUnitPrefix) {
		return scopeID
	}
	return ""
}
