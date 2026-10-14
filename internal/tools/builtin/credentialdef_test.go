package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func credFixture(t *testing.T, userID string, kek string) (*CredentialDef, context.Context) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(kek, "")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	tool := &CredentialDef{Engine: credential.NewEngine(st, sealer)}
	ctx := tools.WithAgentName(context.Background(), "agent-1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: userID, TenantID: "tnt"})
	return tool, ctx
}

func credExec(t *testing.T, c *CredentialDef, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := c.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		_ = json.Unmarshal([]byte(res.Text), &out)
	}
	return out, res
}

func testKEK() string { return base64.StdEncoding.EncodeToString(make([]byte, 32)) }

func TestCredentialDefTool_CreateGetIsMetadataOnly(t *testing.T) {
	c, ctx := credFixture(t, "u1", testKEK())

	out, res := credExec(t, c, ctx, `{"op":"create","scope":"tenant","name":"serper","value":"sk-secret-xyz"}`)
	if res.IsError {
		t.Fatalf("create: %q", res.Text)
	}
	// The create result must never echo the plaintext value.
	if strings.Contains(res.Text, "sk-secret-xyz") {
		t.Errorf("create result leaked the plaintext value: %s", res.Text)
	}
	if out["name"] != "serper" || out["backend"] != "inline" {
		t.Errorf("create meta = %s", res.Text)
	}

	// get returns metadata, never the value.
	_, res = credExec(t, c, ctx, `{"op":"get","scope":"tenant","name":"serper"}`)
	if res.IsError || strings.Contains(res.Text, "sk-secret-xyz") {
		t.Errorf("get should return metadata only, no value: %s (err=%v)", res.Text, res.IsError)
	}
}

func TestCredentialDefTool_UserScopeStampsOwnSubject(t *testing.T) {
	// user u1 stores a user-scoped token.
	c1, ctx1 := credFixture(t, "u1", testKEK())
	if _, res := credExec(t, c1, ctx1, `{"op":"create","scope":"user","name":"telegram","value":"u1-token"}`); res.IsError {
		t.Fatalf("u1 create: %q", res.Text)
	}
	// A different user (u2) on the SAME engine/store can't see u1's user-scoped
	// credential — the tool stamps scope_id from the caller's own subject.
	c2 := &CredentialDef{Engine: c1.Engine}
	ctx2 := tools.WithRunIdentity(tools.WithAgentName(context.Background(), "agent-1"),
		tools.RunIdentityValue{AgentID: "a", UserID: "u2", TenantID: "tnt"})
	_, res := credExec(t, c2, ctx2, `{"op":"get","scope":"user","name":"telegram"}`)
	if !res.IsError {
		t.Errorf("user u2 resolved u1's user-scoped credential — isolation breach: %s", res.Text)
	}
	// u2 lists their own (empty) user bucket — must not include u1's.
	out, _ := credExec(t, c2, ctx2, `{"op":"list","scope":"user"}`)
	if creds, _ := out["credentials"].([]any); len(creds) != 0 {
		t.Errorf("u2's user-scope list leaked u1's credential: %v", creds)
	}
}

func TestCredentialDefTool_FailClosedNoKEK(t *testing.T) {
	c, ctx := credFixture(t, "u1", "") // no KEK
	_, res := credExec(t, c, ctx, `{"op":"create","scope":"tenant","name":"x","value":"v"}`)
	if !res.IsError || !strings.Contains(res.Text, "LOOMCYCLE_SECRET_KEY") {
		t.Errorf("create with no KEK should fail pointing at LOOMCYCLE_SECRET_KEY; got %q (err=%v)", res.Text, res.IsError)
	}
}

func TestCredentialDefTool_ScopeUserRequiresUserID(t *testing.T) {
	c, ctx := credFixture(t, "", testKEK()) // no user id on the run
	_, res := credExec(t, c, ctx, `{"op":"create","scope":"user","name":"x","value":"v"}`)
	if !res.IsError {
		t.Error("scope=user with no user_id on the run should error")
	}
}

func TestMaskCredentialCreateValue_ViaTool(t *testing.T) {
	// The redaction of the create `value` lives in internal/api/http
	// (maskCredentialCreateValue); here we just assert the tool's own result
	// never contains the value (covered above) and that a bad-name create is
	// refused before any store write.
	c, ctx := credFixture(t, "u1", testKEK())
	if _, res := credExec(t, c, ctx, `{"op":"create","scope":"tenant","name":"bad name!","value":"v"}`); !res.IsError {
		t.Error("create with an invalid name charset should be refused")
	}
}

// allowed_urls is stored with the credential, shown by get and list in its
// canonical form, and never loosened by a rotation that leaves it out.
func TestCredentialDefTool_AllowedURLsAreStoredShownAndKeptAcrossARotation(t *testing.T) {
	c, ctx := credFixture(t, "u1", testKEK())

	out, res := credExec(t, c, ctx, `{"op":"create","name":"repo","value":"ghs_x","allowed_urls":["https://GitHub.com/acme/app.git"]}`)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	shown := func(m map[string]any) []any { l, _ := m["allowed_urls"].([]any); return l }
	if got := shown(out); len(got) != 1 || got[0] != "https://github.com/acme/app" {
		t.Fatalf("create answered allowed_urls = %v, want the canonical entry", out["allowed_urls"])
	}
	if strings.Contains(res.Text, "ghs_x") {
		t.Error("the create answer carries the value")
	}
	if out, _ := credExec(t, c, ctx, `{"op":"get","name":"repo"}`); len(shown(out)) != 1 {
		t.Errorf("get does not show the list: %v", out)
	}
	listed, _ := credExec(t, c, ctx, `{"op":"list"}`)
	creds, _ := listed["credentials"].([]any)
	if len(creds) != 1 || len(shown(creds[0].(map[string]any))) != 1 {
		t.Errorf("list does not show the list: %v", listed)
	}

	// A rotation that says nothing about the list keeps it.
	if out, res := credExec(t, c, ctx, `{"op":"create","name":"repo","value":"ghs_y"}`); res.IsError || len(shown(out)) != 1 {
		t.Errorf("a rotation without allowed_urls: %s %v, want the list kept", res.Text, out)
	}
	// The restricted value is not handed to a caller that names no destination.
	if _, _, err := c.Engine.Resolve(ctx, "tnt", "", "", "repo"); err == nil {
		t.Error("the restricted credential resolved with no destination")
	}
	if got, _, err := c.Engine.ResolveFor(netguard.WithPinnable(ctx), "tnt", "", "", "repo", "https://github.com/acme/app"); err != nil || got.Value != "ghs_y" {
		t.Errorf("for its own URL: %q err=%v", got.Value, err)
	}
}

func TestCredentialDefTool_AllowedURLs_EmptyMeansNowhereAndBadEntriesAreRefused(t *testing.T) {
	c, ctx := credFixture(t, "u1", testKEK())

	out, res := credExec(t, c, ctx, `{"op":"create","name":"hook","value":"whsec","allowed_urls":[]}`)
	if res.IsError {
		t.Fatalf("create with an empty list: %s", res.Text)
	}
	if l, ok := out["allowed_urls"].([]any); !ok || len(l) != 0 {
		t.Errorf("an empty list must be shown as an empty list, not left out: %v", out)
	}
	if _, _, err := c.Engine.ResolveFor(ctx, "tnt", "", "", "hook", "https://github.com/acme/app"); err == nil {
		t.Error("a credential that is sent nowhere resolved for a URL")
	}
	// An unrestricted credential shows no list at all.
	if out, _ := credExec(t, c, ctx, `{"op":"create","name":"plain","value":"v"}`); out["allowed_urls"] != nil {
		t.Errorf("an unrestricted credential shows allowed_urls: %v", out["allowed_urls"])
	}
	for _, bad := range []string{
		`["http://github.com/acme/app"]`, `["https://tok@github.com/acme/app"]`, `["https://github.com/a?b=1"]`, `["github.com/acme/app"]`, `[""]`,
	} {
		if _, res := credExec(t, c, ctx, `{"op":"create","name":"bad","value":"v","allowed_urls":`+bad+`}`); !res.IsError || !strings.Contains(res.Text, "allowed_urls") {
			t.Errorf("allowed_urls %s: error=%v %q, want a refusal naming the field", bad, res.IsError, res.Text)
		}
	}
	if _, res := credExec(t, c, ctx, `{"op":"get","name":"bad"}`); !res.IsError {
		t.Error("a refused create stored the credential")
	}
}

// username is stored with the credential, shown by get and list, kept by a
// rotation that leaves it out, removed by "", and refused when it would break
// basic auth.
func TestCredentialDefTool_UsernameIsStoredShownKeptAndCleared(t *testing.T) {
	c, ctx := credFixture(t, "u1", testKEK())

	out, res := credExec(t, c, ctx, `{"op":"create","name":"repo","value":"glpat_x","username":"bob"}`)
	if res.IsError || out["username"] != "bob" {
		t.Fatalf("create: %s %v", res.Text, out)
	}
	if out, _ := credExec(t, c, ctx, `{"op":"get","name":"repo"}`); out["username"] != "bob" {
		t.Errorf("get does not show the user name: %v", out)
	}
	listed, _ := credExec(t, c, ctx, `{"op":"list"}`)
	if creds, _ := listed["credentials"].([]any); len(creds) != 1 || creds[0].(map[string]any)["username"] != "bob" {
		t.Errorf("list does not show the user name: %v", listed)
	}
	if out, res := credExec(t, c, ctx, `{"op":"create","name":"repo","value":"glpat_y"}`); res.IsError || out["username"] != "bob" {
		t.Errorf("a rotation without username: %s %v, want it kept", res.Text, out)
	}
	if out, res := credExec(t, c, ctx, `{"op":"create","name":"repo","value":"glpat_z","username":""}`); res.IsError || out["username"] != nil {
		t.Errorf("username \"\": %s %v, want it removed", res.Text, out)
	}
	for _, bad := range []string{`"a:b"`, `"a\nb"`} {
		if _, res := credExec(t, c, ctx, `{"op":"create","name":"repo","value":"v","username":`+bad+`}`); !res.IsError {
			t.Errorf("username %s accepted", bad)
		}
	}
}
