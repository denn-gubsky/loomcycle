package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// add_hook and remove_hook mint a new version the scheduler fires under the
// bits it carries. It keeps the parent's and adds the editor's, the most
// restrictive winning: before, it kept only the parent's, so a confined editor
// could mint a version that fired unconfined.

// unconfinedScheduleParent creates a schedule in acme with one hook, authored
// with no restriction, and returns its def id.
func unconfinedScheduleParent(t *testing.T, tool *ScheduleDef, base context.Context) string {
	t.Helper()
	author := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_author", TenantID: "acme"})
	res, _ := tool.Execute(author, json.RawMessage(`{"op":"create","name":"edited-sched","overlay":{"agent":"job-search-batch","schedule":"0 9 * * 1","user_id":"alice",`+
		`"on_complete":[{"kind":"memory.set","scope":"user","key":"k"}]}}`))
	if r, i := capturedBits(t, res); r || i {
		t.Fatalf("fixture drifted: the parent was authored restricted=%v isolated=%v", r, i)
	}
	return decodeResult(t, res.Text)["def_id"].(string)
}

func TestScheduleDef_HookEditCarriesTheEditorsRestriction(t *testing.T) {
	for _, c := range authorCtxCases() {
		t.Run(c.name, func(t *testing.T) {
			tool, base, cleanup := scheduleDefFixture(t)
			defer cleanup()
			tool.Cfg.Env.OperatorKeyRestriction = true
			parent := unconfinedScheduleParent(t, tool, base)
			ctx := c.apply(base)

			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"add_hook","def_id":"`+parent+`","hook":{"kind":"memory.set","scope":"user","key":"k2"}}`))
			checkBits(t, "add_hook", c, res)
			res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"remove_hook","def_id":"`+parent+`","hook_index":0}`))
			checkBits(t, "remove_hook", c, res)
		})
	}
}

// The parent's bits survive an unconfined editor: a hook edit adds
// restriction, it never lifts it.
func TestScheduleDef_HookEditKeepsTheParentsRestriction(t *testing.T) {
	tool, base, cleanup := scheduleDefFixture(t)
	defer cleanup()
	tool.Cfg.Env.OperatorKeyRestriction = true
	var confined, loose authorCtx
	for _, c := range authorCtxCases() {
		switch c.name {
		case "resumed confined run, no principal":
			confined = c
		case "loose principal, unconfined run":
			loose = c
		}
	}
	res, _ := tool.Execute(confined.apply(base), json.RawMessage(`{"op":"create","name":"edited-sched","overlay":{"agent":"job-search-batch","schedule":"0 9 * * 1","user_id":"alice"}}`))
	checkBits(t, "create", confined, res)
	parent := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(loose.apply(base), json.RawMessage(`{"op":"add_hook","def_id":"`+parent+`","hook":{"kind":"memory.set","scope":"user","key":"k"}}`))
	checkBits(t, "add_hook by a loose editor", confined, res)
}
