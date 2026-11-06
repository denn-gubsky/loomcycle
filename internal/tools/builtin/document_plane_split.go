package builtin

import (
	"context"
	"log/slog"
)

// SplitDocumentPlanes moves every sqlite scope whose document tables still sit in
// its agent file — every scope from before the document plane had a file of its
// own — into its document file, and returns how many it moved.
//
// The Document tool's provisioning moves a scope on its first use anyway
// (provisionSchema). This pass runs at boot so the move does not wait for that:
// several readers do not provision (the ontology resolver, the change feed, the
// memory labels), and until a scope has moved they find no document file and so
// no documents. A scope this pass cannot move — busy, or failing — is logged and
// left for its first use. On postgres the planes split long ago, and a scope
// still waiting there keeps moving on first use; this pass does not touch it.
func (d *Document) SplitDocumentPlanes(ctx context.Context) (int, error) {
	if d == nil || d.SqlMem == nil || d.docSQL().Tier() != "sqlite" {
		return 0, nil
	}
	scopes, err := d.SqlMem.ListScopes(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, sk := range scopes {
		held, err := d.docSQL().AgentPlaneTables(ctx, sk, documentTableNames)
		if err != nil {
			slog.WarnContext(ctx, "document: could not check a scope for document tables to move", "tenant", sk.Tenant, "scope", sk.Scope, "scope_id", sk.ScopeID, "err", err)
			continue
		}
		if len(held) == 0 {
			continue
		}
		// The memo would skip a scope this process already provisioned, and a
		// scope holding document tables in its agent file needs the move whatever
		// the memo says.
		d.docSQL().ForgetEnsured(sk)
		if err := d.ensureSchema(ctx, sk); err != nil {
			slog.WarnContext(ctx, "document: could not move a scope's document tables into its document file; its first use retries", "tenant", sk.Tenant, "scope", sk.Scope, "scope_id", sk.ScopeID, "err", err)
			continue
		}
		moved++
	}
	return moved, nil
}
