package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const httpHookOverlay = `{"description":"Blocks internal hosts.","event":"pre","match":{"tools":["WebFetch"]},"body":{"kind":"http","url":"https://hooks.example/gate"},"fail_mode":"closed","timeout_ms":800}`

func hookDefFixture(t *testing.T) (*HookDef, *sqlite.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &HookDef{Store: s, MaxDescriptionBytes: 8192}, s
}

func hookTenantCtx(tenant string) context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_http-admin", TenantID: tenant})
}

func runHookDef(t *testing.T, tool *HookDef, ctx context.Context, input string) (map[string]any, string, bool) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		return nil, res.Text, true
	}
	return decodeResult(t, res.Text), res.Text, false
}

func mustHookDef(t *testing.T, tool *HookDef, ctx context.Context, input string) map[string]any {
	t.Helper()
	out, text, isErr := runHookDef(t, tool, ctx, input)
	if isErr {
		t.Fatalf("%s: %s", input, text)
	}
	return out
}

func TestHookDef_CreateStoresTheNormalizedDefinitionAndPromotes(t *testing.T) {
	tool, s := hookDefFixture(t)
	ctx := hookTenantCtx("acme")
	out := mustHookDef(t, tool, ctx, `{"op":"create","name":"net/deny-internal","overlay":`+httpHookOverlay+`}`)
	if out["version"].(float64) != 1 || out["promoted"] != true {
		t.Fatalf("create = %v; want version 1, promoted", out)
	}
	active, err := s.HookDefGetActive(ctx, "acme", "net/deny-internal")
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	var def hooks.Def
	if err := json.Unmarshal(active.Definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Event != hooks.PhasePre || def.Body.URL != "https://hooks.example/gate" || def.FailMode != hooks.FailClosed || def.Match.Tools[0] != "WebFetch" {
		t.Fatalf("stored def = %+v", def)
	}
	if active.ContentSHA256 != hooks.SignDef("net/deny-internal", def) || !strings.HasPrefix(active.ContentSHA256, "sha256:") {
		t.Fatalf("content_sha256 = %q", active.ContentSHA256)
	}
	if active.TenantID != "acme" {
		t.Fatalf("tenant = %q, want the caller's", active.TenantID)
	}
}

func TestHookDef_CreateRefusesADefinitionTheDispatcherCouldNotRun(t *testing.T) {
	tool, _ := hookDefFixture(t)
	ctx := hookTenantCtx("acme")
	cases := map[string]string{
		"unknown event":       `{"event":"pre_tool_use","body":{"kind":"http","url":"https://h.example"}}`,
		"no event":            `{"body":{"kind":"http","url":"https://h.example"}}`,
		"tools on run event":  `{"event":"agent_stop","match":{"tools":["Read"]},"body":{"kind":"http","url":"https://h.example"}}`,
		"no body kind":        `{"event":"pre","body":{"url":"https://h.example"}}`,
		"url not http":        `{"event":"pre","body":{"kind":"http","url":"ftp://h.example"}}`,
		"http with code":      `{"event":"pre","body":{"kind":"http","url":"https://h.example","code":"x"}}`,
		"code without code":   `{"event":"pre","body":{"kind":"code-js"}}`,
		"bad fail_mode":       `{"event":"pre","fail_mode":"maybe","body":{"kind":"http","url":"https://h.example"}}`,
		"negative timeout":    `{"event":"pre","timeout_ms":-1,"body":{"kind":"http","url":"https://h.example"}}`,
		"unknown field":       `{"event":"pre","owner":"app","body":{"kind":"http","url":"https://h.example"}}`,
		"description too big": `{"event":"pre","description":"` + strings.Repeat("x", hooks.MaxDescriptionBytes+1) + `","body":{"kind":"http","url":"https://h.example"}}`,
	}
	for name, overlay := range cases {
		t.Run(name, func(t *testing.T) {
			_, text, isErr := runHookDef(t, tool, ctx, `{"op":"create","name":"h","overlay":`+overlay+`}`)
			if !isErr {
				t.Fatalf("created; want refused")
			}
			if !strings.HasPrefix(text, "create: ") {
				t.Fatalf("error = %q", text)
			}
		})
	}
}

func TestHookDef_NameRefusesTheCharactersReferencesAndPermitsUse(t *testing.T) {
	tool, _ := hookDefFixture(t)
	ctx := hookTenantCtx("acme")
	for _, name := range []string{"gate@2", "acme:gate", "/gate", "a//b", "", "sp ace"} {
		if _, _, isErr := runHookDef(t, tool, ctx, `{"op":"create","name":"`+name+`","overlay":`+httpHookOverlay+`}`); !isErr {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestHookDef_CodeBodyIsRefusedWhenCodeHooksAreOff(t *testing.T) {
	tool, _ := hookDefFixture(t)
	ctx := hookTenantCtx("acme")
	overlay := `{"event":"pre","body":{"kind":"code-js","code":"function hook(ev){return {}}"}}`
	_, text, isErr := runHookDef(t, tool, ctx, `{"op":"create","name":"c","overlay":`+overlay+`}`)
	if !isErr || !strings.Contains(text, "LOOMCYCLE_CODE_HOOKS_ENABLED") {
		t.Fatalf("got %q (err=%v); want refused naming the gate", text, isErr)
	}
}

func TestHookDef_CodeBodyIsCompiledOnWrite(t *testing.T) {
	tool, _ := hookDefFixture(t)
	var compiled []string
	tool.CompileCode = func(src string) error {
		compiled = append(compiled, src)
		if strings.Contains(src, "broken") {
			return errors.New("SyntaxError: unexpected token")
		}
		return nil
	}
	ctx := hookTenantCtx("acme")
	good := `{"event":"agent_stop","body":{"kind":"code-js","code":"function hook(ev){return {}}"}}`
	mustHookDef(t, tool, ctx, `{"op":"create","name":"c","overlay":`+good+`}`)
	bad := `{"body":{"kind":"code-js","code":"broken("}}`
	_, text, isErr := runHookDef(t, tool, ctx, `{"op":"fork","name":"c","overlay":`+bad+`}`)
	if !isErr || !strings.Contains(text, "SyntaxError") {
		t.Fatalf("fork with a broken body = %q; want the compile error", text)
	}
	if len(compiled) != 2 {
		t.Fatalf("compiled %d bodies; want 2", len(compiled))
	}
}

func TestHookDef_ForkReplacesTopLevelFieldsAndLeavesThePointer(t *testing.T) {
	tool, s := hookDefFixture(t)
	ctx := hookTenantCtx("acme")
	v1 := mustHookDef(t, tool, ctx, `{"op":"create","name":"gate","overlay":`+httpHookOverlay+`}`)
	v2 := mustHookDef(t, tool, ctx, `{"op":"fork","name":"gate","overlay":{"fail_mode":"open","match":null}}`)
	if v2["version"].(float64) != 2 || v2["parent_def_id"] != v1["def_id"] || v2["promoted"] != false {
		t.Fatalf("fork = %v", v2)
	}
	row, err := s.HookDefGet(ctx, v2["def_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var def hooks.Def
	_ = json.Unmarshal(row.Definition, &def)
	if def.FailMode != hooks.FailOpen || def.Match != nil || def.Body.URL != "https://hooks.example/gate" || def.Description != "Blocks internal hosts." {
		t.Fatalf("forked def = %+v; want fail_mode replaced, match cleared, the rest inherited", def)
	}
	if row.ContentSHA256 == "" || row.ContentSHA256 == v1["content_sha256"] {
		t.Fatalf("fork hash %q must differ from the parent's", row.ContentSHA256)
	}
	active, _ := s.HookDefGetActive(ctx, "acme", "gate")
	if active.DefID != v1["def_id"] {
		t.Fatalf("active = %s; a fork does not promote by default", active.DefID)
	}
	mustHookDef(t, tool, ctx, `{"op":"promote","def_id":"`+v2["def_id"].(string)+`"}`)
	if active, _ = s.HookDefGetActive(ctx, "acme", "gate"); active.DefID != v2["def_id"] {
		t.Fatalf("after promote active = %s", active.DefID)
	}
}

// A shared hook can be named by a tenant's definitions but not forked into the
// tenant: the fork would copy a body that stays with the hook's author.
func TestHookDef_ATenantCannotForkOrReadAnotherTenantsHook(t *testing.T) {
	tool, _ := hookDefFixture(t)
	shared := hookTenantCtx("")
	acme := hookTenantCtx("acme")
	other := hookTenantCtx("other")
	sv := mustHookDef(t, tool, shared, `{"op":"create","name":"gate","overlay":`+httpHookOverlay+`}`)
	ov := mustHookDef(t, tool, other, `{"op":"create","name":"gate","overlay":`+httpHookOverlay+`}`)

	for _, id := range []string{sv["def_id"].(string), ov["def_id"].(string)} {
		for _, op := range []string{
			`{"op":"fork","name":"gate","parent_def_id":"` + id + `"}`,
			`{"op":"get","def_id":"` + id + `"}`,
			`{"op":"promote","def_id":"` + id + `"}`,
			`{"op":"retire","def_id":"` + id + `","retired":true}`,
		} {
			_, text, isErr := runHookDef(t, tool, acme, op)
			if !isErr || !strings.Contains(text, "not found") {
				t.Errorf("%s from acme = %q; want an opaque not found", op, text)
			}
		}
	}
	if _, text, isErr := runHookDef(t, tool, acme, `{"op":"fork","name":"gate"}`); !isErr || !strings.Contains(text, "no active version in this tenant") {
		t.Errorf("fork by name from acme = %q; want no parent (the shared hook is not a fork parent)", text)
	}
	if out := mustHookDef(t, tool, acme, `{"op":"list","name":"gate"}`); out["versions"] != nil && len(out["versions"].([]any)) != 0 {
		t.Errorf("acme lists %v; want none of the other tenants' versions", out["versions"])
	}
	if _, _, isErr := runHookDef(t, tool, acme, `{"op":"delete","name":"gate"}`); !isErr {
		t.Errorf("acme deleted a hook it has no version of")
	}
	if out := mustHookDef(t, tool, other, `{"op":"get","name":"gate"}`); out["def_id"] != ov["def_id"] {
		t.Errorf("other's hook changed: %v", out)
	}
}

func TestHookDef_AnAdminReachesAnotherTenantsHook(t *testing.T) {
	tool, s := hookDefFixture(t)
	acme := hookTenantCtx("acme")
	v1 := mustHookDef(t, tool, acme, `{"op":"create","name":"gate","overlay":`+httpHookOverlay+`}`)
	v2 := mustHookDef(t, tool, acme, `{"op":"fork","name":"gate","overlay":{"timeout_ms":900}}`)
	admin := auth.WithPrincipal(hookTenantCtx("ops"), auth.Principal{TenantID: "ops", Subject: "op", Scopes: []string{auth.ScopeAdmin}})
	mustHookDef(t, tool, admin, `{"op":"get","def_id":"`+v1["def_id"].(string)+`"}`)
	// Not even an admin forks across tenants: the lineage would cross them.
	if _, _, isErr := runHookDef(t, tool, admin, `{"op":"fork","name":"gate","parent_def_id":"`+v1["def_id"].(string)+`"}`); !isErr {
		t.Fatalf("admin forked acme's hook into its own tenant")
	}
	mustHookDef(t, tool, admin, `{"op":"promote","def_id":"`+v2["def_id"].(string)+`"}`)
	if active, _ := s.HookDefGetActive(context.Background(), "acme", "gate"); active.DefID != v2["def_id"] {
		t.Fatalf("admin promote moved acme's pointer to %s; want %s", active.DefID, v2["def_id"])
	}
	if _, err := s.HookDefGetActive(context.Background(), "ops", "gate"); err == nil {
		t.Fatalf("admin promote created a pointer in the admin's own tenant")
	}
}

func TestHookDef_RefusesACallFromInsideARun(t *testing.T) {
	tool, s := hookDefFixture(t)
	ctx := tools.WithRunID(hookTenantCtx("acme"), "run_1")
	_, text, isErr := runHookDef(t, tool, ctx, `{"op":"create","name":"gate","overlay":`+httpHookOverlay+`}`)
	if !isErr || !strings.Contains(text, "not available inside a run") {
		t.Fatalf("in-run create = %q; want refused", text)
	}
	if rows, _ := s.HookDefListByName(context.Background(), "gate"); len(rows) != 0 {
		t.Fatalf("an in-run call wrote %d rows", len(rows))
	}
}

func TestHookDef_VerifyGetByNameRetireAndDelete(t *testing.T) {
	tool, s := hookDefFixture(t)
	ctx := hookTenantCtx("acme")
	v1 := mustHookDef(t, tool, ctx, `{"op":"create","name":"gate","overlay":`+httpHookOverlay+`}`)
	sha := v1["content_sha256"].(string)

	if out := mustHookDef(t, tool, ctx, `{"op":"verify","name":"gate","content_sha256":"`+sha+`"}`); out["matches"] != true || out["deployed"] != true {
		t.Fatalf("verify = %v", out)
	}
	if out := mustHookDef(t, tool, ctx, `{"op":"verify","name":"gate","content_sha256":"sha256:00"}`); out["matches"] != false {
		t.Fatalf("verify with a wrong hash = %v", out)
	}
	if out := mustHookDef(t, tool, ctx, `{"op":"get","name":"gate"}`); out["def_id"] != v1["def_id"] {
		t.Fatalf("get by name = %v", out)
	}
	mustHookDef(t, tool, ctx, `{"op":"retire","def_id":"`+v1["def_id"].(string)+`","retired":true}`)
	if row, _ := s.HookDefGet(ctx, v1["def_id"].(string)); !row.Retired {
		t.Fatalf("not retired")
	}
	mustHookDef(t, tool, ctx, `{"op":"fork","name":"gate","parent_def_id":"`+v1["def_id"].(string)+`","promote":true}`)
	if out := mustHookDef(t, tool, ctx, `{"op":"delete","name":"gate"}`); out["deleted"] != true {
		t.Fatalf("delete = %v", out)
	}
	if rows, _ := s.HookDefListByName(ctx, "gate"); len(rows) != 0 {
		t.Fatalf("after delete %d versions remain", len(rows))
	}
	if out := mustHookDef(t, tool, ctx, `{"op":"verify","name":"gate"}`); out["deployed"] != false {
		t.Fatalf("verify after delete = %v", out)
	}
}
