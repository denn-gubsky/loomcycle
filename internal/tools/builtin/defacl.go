package builtin

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// defOwnerFunc reads a definition by def_id: its name and owning tenant.
type defOwnerFunc func(ctx context.Context, defID string) (name, tenant string, err error)

// defOpNames is every definition a def-tool call names, for the op's ACL
// question (tools.AuthorizeDefOp): the name it gives, and the name each id it
// gives resolves to. An id is resolved rather than trusted beside the name
// because the ops that take one act on the id, not the name — asking only of
// a name the caller chose would let it name an allowed definition and act on
// a denied one.
//
// An id that does not resolve in the caller's tenant or the shared one (not
// found, a store fault, another tenant's row) contributes "", the
// family-wide question: the op is refused or fails as not found anyway, and
// the refusal must not name another tenant's definition.
func defOpNames(ctx context.Context, name string, owner defOwnerFunc, ids ...string) []string {
	var out []string
	if name != "" {
		out = append(out, name)
	}
	tenant := tools.RunIdentity(ctx).TenantID
	for _, id := range ids {
		if id == "" {
			continue
		}
		n, t, err := owner(ctx, id)
		if err != nil || n == "" || (t != tenant && t != "") {
			out = append(out, "")
			continue
		}
		out = append(out, n)
	}
	return out
}
