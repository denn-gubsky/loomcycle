package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// An operator taking a team's own agent's override lock off is audited as
// taking a stored agent's off is (allow_overrides_lock_removed), naming the
// agent <team>/<name> so it reads apart from a stored agent; the change is
// refused when the record cannot be written, and a version that keeps the
// lock records nothing.
func TestTeamDefFork_AnOperatorLiftingALocalAgentsOverrideLockIsAudited(t *testing.T) {
	const locked, open = `{"tier":"middle","allow_overrides":false}`, `{"tier":"middle"}`
	opCtx := func(t *testing.T) (*TeamDef, *noTraceAuditSink, func(string) string) {
		t.Helper()
		tool, ctx := localLockFixture(t)
		sink := tool.Agents.Audit.(*noTraceAuditSink)
		teamDefIDOf(t, teamOp(t, tool, ctx, "create", "sdlc", localTeam(locked)))
		op := operatorPlane(auth.WithPrincipal(ctx, auth.Principal{Subject: "ops", Scopes: []string{auth.ScopeTenant}}))
		fork := func(body string) string {
			res := teamOp(t, tool, op, "fork", "sdlc", localAgentsOverlay(body))
			if res.IsError {
				return res.Text
			}
			return ""
		}
		return tool, sink, fork
	}

	t.Run("lift is recorded", func(t *testing.T) {
		_, sink, fork := opCtx(t)
		if msg := fork(open); msg != "" {
			t.Fatalf("the operator lifting the lock: %s", msg)
		}
		if len(sink.evs) != 1 || sink.evs[0].Action != "allow_overrides_lock_removed" ||
			sink.evs[0].TargetName != "sdlc/reviewer" || sink.evs[0].ActorSubject != "ops" {
			t.Errorf("audit records = %+v, want one allow_overrides_lock_removed naming sdlc/reviewer by ops", sink.evs)
		}
	})
	t.Run("keeping the lock records nothing", func(t *testing.T) {
		_, sink, fork := opCtx(t)
		if msg := fork(`{"tier":"high","allow_overrides":false}`); msg != "" {
			t.Fatalf("fork keeping the lock: %s", msg)
		}
		if len(sink.evs) != 0 {
			t.Errorf("audit records = %+v, want none", sink.evs)
		}
	})
	t.Run("refused when the record cannot be written", func(t *testing.T) {
		_, sink, fork := opCtx(t)
		sink.fail = true
		msg := fork(open)
		if msg == "" {
			t.Fatal("the lift succeeded although its audit record could not be written")
		}
		if !strings.Contains(msg, "audit record") {
			t.Errorf("refusal %q does not say the audit record failed", msg)
		}
	})
}
