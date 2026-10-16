package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// refuseNoTraceTriggerAuthor refuses writing a run-triggering definition
// (a schedule, a webhook) for a no-trace caller: a token whose runs are
// no-trace, or a no-trace run.
//
// The definition stores what its author wrote (a schedule's prompt), and
// the runs it fires are stored: a fired run has no principal on it whose
// mode could keep it unstored, and none of the bits a trigger captures from
// its author says otherwise. Refused, not captured — the same answer a draft
// gets (a draft would store its prompt) and for the same reason; a
// definition that quietly fired no-trace runs could not be told apart from
// one that stored them until something had been kept.
func refuseNoTraceTriggerAuthor(ctx context.Context, kind, op string) (tools.Result, bool) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !(ok && p.Restrictions.Retention == store.RetentionNone) && tools.RunIdentity(ctx).Retention != store.RetentionNone {
		return tools.Result{}, false
	}
	return errValidation(
		fmt.Sprintf("%s: a no-trace caller cannot write a %s: the definition and the runs it starts would be stored", op, kind),
		"start the runs directly, or have a caller whose runs are stored write the definition"), true
}
