package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// noTraceCtxs are the two ways a caller is no-trace: its token says so, or it
// is a no-trace run. A trigger it wrote would start runs that are stored — no
// principal is on a fired run to keep them unstored — so, as a draft is, the
// definition is refused.
func noTraceCtxs(base context.Context) map[string]context.Context {
	run := tools.RunIdentity(base)
	run.Retention = store.RetentionNone
	return map[string]context.Context{
		"a no-trace token": auth.WithPrincipal(base, auth.Principal{Subject: "dev", Scopes: []string{auth.ScopeAdmin},
			Restrictions: auth.Restrictions{Retention: store.RetentionNone}}),
		"a no-trace run": tools.WithRunIdentity(base, run),
	}
}

func TestScheduleDefTool_ANoTraceCallerCannotWriteASchedule(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	overlay := `{"agent":"job-search-batch","schedule":"0 6 * * *"}`
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"base","overlay":`+overlay+`}`)); res.IsError {
		t.Fatalf("create by a stored caller: %s", res.Text)
	}
	row, err := tool.Store.ScheduleDefGetActive(context.Background(), "", "base")
	if err != nil {
		t.Fatal(err)
	}
	// A hook edit writes a new version too: the same authoring, the same refusal.
	defID := `"def_id":"` + row.DefID + `"`
	for who, nctx := range noTraceCtxs(ctx) {
		for op, in := range map[string]string{
			"create":      `{"op":"create","name":"quiet","overlay":` + overlay + `}`,
			"fork":        `{"op":"fork","name":"base","overlay":{"schedule":"0 7 * * *"}}`,
			"add_hook":    `{"op":"add_hook",` + defID + `,"hook":{"kind":"memory.set","scope":"user","key":"k"}}`,
			"remove_hook": `{"op":"remove_hook",` + defID + `,"hook_index":0}`,
		} {
			res, err := tool.Execute(nctx, json.RawMessage(in))
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(res.Text, "no-trace") {
				t.Errorf("%s %s: %s, want it refused as no-trace", who, op, res.Text)
			}
		}
	}
}

func TestWebhookDefTool_ANoTraceCallerCannotWriteAWebhook(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	if text, isErr := webhookCreate(t, tool, ctx, "base", `{`+webhookBase+`}`); isErr {
		t.Fatalf("create by a stored caller: %s", text)
	}
	for who, nctx := range noTraceCtxs(ctx) {
		for op, in := range map[string]string{
			"create": `{"op":"create","name":"quiet","overlay":{` + webhookBase + `}}`,
			"fork":   `{"op":"fork","name":"base","overlay":{"metadata":{"x":"y"}}}`,
		} {
			res, err := tool.Execute(nctx, json.RawMessage(in))
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(res.Text, "no-trace") {
				t.Errorf("%s %s: %s, want it refused as no-trace", who, op, res.Text)
			}
		}
	}
}
