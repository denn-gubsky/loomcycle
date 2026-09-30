package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// passValidators accepts every body of the sections that must be validated.
// The real validators live with the tools (internal/tools/builtin) and are
// wired at the restore call sites; the HTTP package tests them end to end.
func passValidators() map[string]func(json.RawMessage) error {
	ok := func(json.RawMessage) error { return nil }
	return map[string]func(json.RawMessage) error{
		migrations.SectionWebhookDefs:       ok,
		migrations.SectionA2AAgentDefs:      ok,
		migrations.SectionA2AServerCardDefs: ok,
	}
}

func triggerBase() time.Time { return time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC) }

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func plantWebhook(t *testing.T, s store.Store, row store.WebhookDefRow, body map[string]any, active bool) {
	t.Helper()
	ctx := context.Background()
	row.Definition = mustJSON(t, body)
	if _, err := s.SnapshotRestoreWebhookDef(ctx, row); err != nil {
		t.Fatalf("plant webhook %s: %v", row.DefID, err)
	}
	if active {
		if _, err := s.SnapshotRestoreWebhookDefActive(ctx, store.WebhookDefActiveEntry{
			TenantID: row.TenantID, Name: row.Name, DefID: row.DefID, PromotedAt: row.CreatedAt, PromotedByAgentID: "planter",
		}); err != nil {
			t.Fatalf("plant webhook active %s: %v", row.DefID, err)
		}
	}
}

func webhookBody(t *testing.T, s store.Store, defID string) map[string]any {
	t.Helper()
	row, err := s.WebhookDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("WebhookDefGet(%s): %v", defID, err)
	}
	var m map[string]any
	if err := json.Unmarshal(row.Definition, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func hookSpawnBody(extra map[string]any) map[string]any {
	b := map[string]any{"delivery": "spawn", "agent": "intake", "enabled": true,
		"auth": map[string]any{"kind": "hmac", "signing_secret_env": "LOOMCYCLE_WH_SECRET"}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// Every webhook def comes back under its own tenant with every column and its
// active pointer — under two tenants and the operator layer, with a tenant
// fork of an operator-layer parent — and the confinement bits and execution
// tenant in the body travel verbatim.
func TestWebhookDefs_RoundTripKeepsEveryColumnAndPointer(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	base := triggerBase()
	defs := []store.WebhookDefRow{
		{DefID: "wh_op", Name: "gh", Version: 1, CreatedAt: base, Description: "operator", CreatedByAgentID: "ag", CreatedByRunID: "run1", BootstrappedFromStatic: true},
		{DefID: "wh_acme", TenantID: "acme", Name: "gh", Version: 1, ParentDefID: "wh_op", CreatedAt: base.Add(time.Minute)},
		{DefID: "wh_acme2", TenantID: "acme", Name: "gh", Version: 2, ParentDefID: "wh_acme", CreatedAt: base.Add(2 * time.Minute), Retired: true},
		{DefID: "wh_beta", TenantID: "beta", Name: "jira", Version: 1, CreatedAt: base.Add(3 * time.Minute)},
	}
	for i, d := range defs {
		plantWebhook(t, src, d, hookSpawnBody(map[string]any{
			"tenant_id": d.TenantID, "operator_key_restricted": true, "isolated": i%2 == 0,
		}), d.DefID != "wh_acme2")
	}

	raw := mustCapture(t, src)
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()})
	if res.WebhookDefsRestored != 4 || res.WebhookDefActiveRestored != 3 {
		t.Fatalf("restored %d defs, %d pointers; want 4 and 3 (warnings %v)", res.WebhookDefsRestored, res.WebhookDefActiveRestored, res.Warnings)
	}
	got, err := dst.SnapshotReadWebhookDefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := src.SnapshotReadWebhookDefs(context.Background())
	for i := range want {
		g, w := got[i], want[i]
		if g.DefID != w.DefID || g.TenantID != w.TenantID || g.Name != w.Name || g.Version != w.Version ||
			g.ParentDefID != w.ParentDefID || g.Description != w.Description || !g.CreatedAt.Equal(w.CreatedAt) ||
			g.CreatedByAgentID != w.CreatedByAgentID || g.CreatedByRunID != w.CreatedByRunID ||
			g.Retired != w.Retired || g.BootstrappedFromStatic != w.BootstrappedFromStatic {
			t.Errorf("def %d = %+v, want %+v", i, g, w)
		}
		if !jsonEqualish(t, g.Definition, w.Definition) {
			t.Errorf("def %s body = %s, want %s", g.DefID, g.Definition, w.Definition)
		}
	}
	ptrs, _ := dst.SnapshotReadWebhookDefActive(context.Background())
	wantPtrs, _ := src.SnapshotReadWebhookDefActive(context.Background())
	if len(ptrs) != len(wantPtrs) {
		t.Fatalf("pointers = %+v, want %+v", ptrs, wantPtrs)
	}
	for i := range ptrs {
		if ptrs[i].TenantID != wantPtrs[i].TenantID || ptrs[i].Name != wantPtrs[i].Name || ptrs[i].DefID != wantPtrs[i].DefID ||
			!ptrs[i].PromotedAt.Equal(wantPtrs[i].PromotedAt) || ptrs[i].PromotedByAgentID != "planter" {
			t.Errorf("pointer %d = %+v, want %+v", i, ptrs[i], wantPtrs[i])
		}
	}

	// A re-restore writes nothing and counts nothing.
	again := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()})
	if again.WebhookDefsRestored != 0 || again.WebhookDefActiveRestored != 0 || again.DefsDisabledForCredentials != 0 {
		t.Errorf("re-restore counted %+v, want nothing", again.Counts())
	}
}

func jsonEqualish(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// V2: a literal user_credentials value never enters the envelope; its key is
// listed, the carried body says enabled:false, a reference stays exactly as
// written, and the def restores disabled with the marker, a warning naming
// the keys, and defs_disabled_for_credentials counting it.
func TestWebhookDefs_LiteralCredentialsStrippedAndDefRestoredDisabled(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	const literal = "p3-literal-webhook-bearer"
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_cred", TenantID: "acme", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		hookSpawnBody(map[string]any{"user_credentials": map[string]string{"slack": literal, "jobs": "$cred:jobs-token"}}), true)

	raw := mustCapture(t, src)
	if strings.Contains(string(raw), literal) {
		t.Fatal("the envelope carries the literal credential value")
	}
	var env struct {
		Sections Sections `json:"sections"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	e := env.Sections.WebhookDefs.Entries[0]
	if !reflect.DeepEqual(e.StrippedCredentials, []string{"slack"}) {
		t.Errorf("stripped_credentials = %v, want [slack]", e.StrippedCredentials)
	}
	var carried map[string]any
	_ = json.Unmarshal(e.Definition, &carried)
	if carried["enabled"] != false {
		t.Errorf("carried body enabled = %v, want false", carried["enabled"])
	}
	if uc, _ := carried["user_credentials"].(map[string]any); uc["jobs"] != "$cred:jobs-token" || uc["slack"] != nil {
		t.Errorf("carried user_credentials = %v, want the reference kept and the literal gone", carried["user_credentials"])
	}
	if carried["operator_key_restricted"] != nil || carried["tenant_id"] != nil {
		// the planted body has neither; make sure the strip adds nothing
		t.Errorf("the strip added fields: %v", carried)
	}

	res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()})
	stored := webhookBody(t, dst, "wh_cred")
	marker, _ := stored["capture_disabled"].(map[string]any)
	if stored["enabled"] != false || !reflect.DeepEqual(marker["stripped_credentials"], []any{"slack"}) {
		t.Errorf("stored body = %v; want enabled:false and capture_disabled listing [slack]", stored)
	}
	if res.DefsDisabledForCredentials != 1 {
		t.Errorf("defs_disabled_for_credentials = %d, want 1", res.DefsDisabledForCredentials)
	}
	if !hasWarning(res, "webhook_def acme/gh", "restored DISABLED", "slack") {
		t.Errorf("no warning naming the def and its stripped key: %v", res.Warnings)
	}
}

// V2: a hand-edited envelope that flips enabled back to true and drops the
// marker, while keeping stripped_credentials, still restores disabled.
func TestWebhookDefs_RestoreForcesDisabledOverAHandEditedEnvelope(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_cred", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		hookSpawnBody(map[string]any{"user_credentials": map[string]string{"slack": "literal"}}), true)
	raw := editSection(t, mustCapture(t, src), "webhook_defs", func(sec map[string]any) {
		e := sec["entries"].([]any)[0].(map[string]any)
		body := e["definition"].(map[string]any)
		body["enabled"] = true
		delete(body, "capture_disabled")
	})
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()})
	stored := webhookBody(t, dst, "wh_cred")
	if stored["enabled"] != false || stored["capture_disabled"] == nil {
		t.Errorf("stored body = %v; want enabled:false and the marker despite the edit", stored)
	}
	if res.DefsDisabledForCredentials != 1 {
		t.Errorf("defs_disabled_for_credentials = %d, want 1", res.DefsDisabledForCredentials)
	}
}

// A second hop (A→B→C) of a webhook never re-enabled lists its keys again, so
// it restores disabled on C too.
func TestWebhookDefs_SecondHopKeepsTheDefDisabled(t *testing.T) {
	a, aClose := newTestStore(t)
	defer aClose()
	b, bClose := newTestStore(t)
	defer bClose()
	c, cClose := newTestStore(t)
	defer cClose()
	plantWebhook(t, a, store.WebhookDefRow{DefID: "wh_cred", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		hookSpawnBody(map[string]any{"user_credentials": map[string]string{"slack": "literal"}}), true)
	mustRestore(t, b, mustCapture(t, a), RestoreOptions{Validators: passValidators()})
	res := mustRestore(t, c, mustCapture(t, b), RestoreOptions{Validators: passValidators()})
	stored := webhookBody(t, c, "wh_cred")
	if stored["enabled"] != false || stored["capture_disabled"] == nil || res.DefsDisabledForCredentials != 1 {
		t.Errorf("third host: body %v, disabled count %d; want disabled with the marker, counted", stored, res.DefsDisabledForCredentials)
	}
}

// The live row stands: a different def already on the target's (tenant,
// name, version) — its own yaml bootstrap — is a per-row warning, not an
// overwrite, and a pointer at the refused def is not written.
func TestWebhookDefs_LiveRowStandsAndItsPointerToo(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_src", Name: "gh", Version: 1, CreatedAt: triggerBase(), BootstrappedFromStatic: true},
		hookSpawnBody(map[string]any{"agent": "from-source"}), true)
	plantWebhook(t, dst, store.WebhookDefRow{DefID: "wh_live", Name: "gh", Version: 1, CreatedAt: triggerBase(), BootstrappedFromStatic: true},
		hookSpawnBody(map[string]any{"agent": "live"}), false)

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: passValidators()})
	if res.WebhookDefsRestored != 0 || res.WebhookDefActiveRestored != 0 {
		t.Errorf("counted %+v, want nothing restored", res.Counts())
	}
	if !hasWarning(res, "webhook_def gh v1 (def wh_src)", "live definition stands") {
		t.Errorf("no per-row warning for the collision: %v", res.Warnings)
	}
	if !hasWarning(res, "webhook_def_active gh", "wh_src") {
		t.Errorf("no warning for the pointer at the refused def: %v", res.Warnings)
	}
	if b := webhookBody(t, dst, "wh_live"); b["agent"] != "live" {
		t.Errorf("the live def was overwritten: %v", b)
	}
}

// New sections are never restored unvalidated: a row whose validator is not
// wired is skipped with a warning, and so is a row the validator refuses —
// along with any pointer at it. The rest of the section lands.
func TestTriggerDefs_UnwiredOrFailingValidatorSkipsTheRow(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_1", Name: "gh", Version: 1, CreatedAt: triggerBase()}, hookSpawnBody(nil), true)
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_ok", Name: "peer-ok", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"endpoint": "https://peer.example/a2a", "binding": "jsonrpc"}, true)
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_bad", Name: "peer-bad", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"endpoint": "https://peer.example/a2a", "binding": "jsonrpc"}, true)
	raw := editSection(t, mustCapture(t, src), "a2a_agent_defs", func(sec map[string]any) {
		for _, x := range sec["entries"].([]any) {
			e := x.(map[string]any)
			if e["def_id"] == "aad_bad" {
				e["definition"].(map[string]any)["endpoint"] = "file:///etc/passwd"
			}
		}
	})

	t.Run("unwired", func(t *testing.T) {
		dst, dstClose := newTestStore(t)
		defer dstClose()
		res := mustRestore(t, dst, raw, RestoreOptions{})
		if res.WebhookDefsRestored != 0 || res.A2AAgentDefsRestored != 0 || res.WebhookDefActiveRestored != 0 || res.A2AAgentDefActiveRestored != 0 {
			t.Errorf("an unwired validator still restored rows: %v", res.Counts())
		}
		if !hasWarning(res, "webhook_def gh v1", "no webhook_defs validator is wired") ||
			!hasWarning(res, "a2a_agent_def peer-ok v1", "no a2a_agent_defs validator is wired") {
			t.Errorf("no per-row warning for the unwired validators: %v", res.Warnings)
		}
		if defs, _ := dst.SnapshotReadWebhookDefs(context.Background()); len(defs) != 0 {
			t.Errorf("an unvalidated webhook def was written: %+v", defs)
		}
	})
	t.Run("failing", func(t *testing.T) {
		dst, dstClose := newTestStore(t)
		defer dstClose()
		v := passValidators()
		v[migrations.SectionA2AAgentDefs] = func(body json.RawMessage) error {
			if strings.Contains(string(body), "file://") {
				return errors.New("endpoint must be an http or https URL")
			}
			return nil
		}
		res := mustRestore(t, dst, raw, RestoreOptions{Validators: v})
		if res.A2AAgentDefsRestored != 1 || res.A2AAgentDefActiveRestored != 1 {
			t.Errorf("restored %d peers, %d pointers; want the valid one only", res.A2AAgentDefsRestored, res.A2AAgentDefActiveRestored)
		}
		if !hasWarning(res, "a2a_agent_def peer-bad v1 (def aad_bad)", "http or https") {
			t.Errorf("no warning naming the refused peer and why: %v", res.Warnings)
		}
		if _, err := dst.A2AAgentDefGet(context.Background(), "aad_bad"); err == nil {
			t.Error("the refused peer def was written")
		}
		if _, err := dst.A2AAgentDefGetActive(context.Background(), "", "peer-bad"); err == nil {
			t.Error("a pointer at the refused peer def was written")
		}
	})
}

func plantA2AAgent(t *testing.T, s store.Store, row store.A2AAgentDefRow, body map[string]any, active bool) {
	t.Helper()
	ctx := context.Background()
	row.Definition = mustJSON(t, body)
	if _, err := s.SnapshotRestoreA2AAgentDef(ctx, row); err != nil {
		t.Fatalf("plant a2a agent %s: %v", row.DefID, err)
	}
	if active {
		if _, err := s.SnapshotRestoreA2AAgentDefActive(ctx, store.A2AAgentDefActiveEntry{
			TenantID: row.TenantID, Name: row.Name, DefID: row.DefID, PromotedAt: row.CreatedAt,
		}); err != nil {
			t.Fatalf("plant a2a agent active %s: %v", row.DefID, err)
		}
	}
}

func plantA2ACard(t *testing.T, s store.Store, row store.A2AServerCardDefRow, body map[string]any, active bool) {
	t.Helper()
	ctx := context.Background()
	row.Definition = mustJSON(t, body)
	if _, err := s.SnapshotRestoreA2AServerCardDef(ctx, row); err != nil {
		t.Fatalf("plant a2a card %s: %v", row.DefID, err)
	}
	if active {
		if _, err := s.SnapshotRestoreA2AServerCardDefActive(ctx, store.A2AServerCardDefActiveEntry{
			TenantID: row.TenantID, Name: row.Name, DefID: row.DefID, PromotedAt: row.CreatedAt,
		}); err != nil {
			t.Fatalf("plant a2a card active %s: %v", row.DefID, err)
		}
	}
}

// A2A peer and card defs come back verbatim under their tenants with their
// pointers; a restored card warns that it is live.
func TestA2ADefs_RoundTripKeepsEveryColumnAndPointer(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	base := triggerBase()
	peerBody := map[string]any{"endpoint": "https://peer.example/a2a", "binding": "jsonrpc",
		"auth": map[string]any{"scheme": "http", "bearer_credential_ref": "peer_token"}, "expected_skills": []any{map[string]any{"id": "s1"}}}
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_op", Name: "peer", Version: 1, CreatedAt: base, BootstrappedFromStatic: true, Description: "d"}, peerBody, true)
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_acme", TenantID: "acme", Name: "peer", Version: 1, ParentDefID: "aad_op",
		CreatedAt: base.Add(time.Minute), CreatedByAgentID: "ag", CreatedByRunID: "r"}, peerBody, true)
	cardBody := map[string]any{"name": "card", "exposed_agents": []any{map[string]any{"agent_name": "helper"}},
		"security_schemes": []any{map[string]any{"kind": "http", "scheme": "bearer"}}}
	plantA2ACard(t, src, store.A2AServerCardDefRow{DefID: "ascd_beta", TenantID: "beta", Name: "card", Version: 1, CreatedAt: base, Retired: false}, cardBody, true)
	plantA2ACard(t, src, store.A2AServerCardDefRow{DefID: "ascd_beta2", TenantID: "beta", Name: "card", Version: 2, ParentDefID: "ascd_beta",
		CreatedAt: base.Add(time.Minute), Retired: true}, cardBody, false)

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: passValidators()})
	c := res.Counts()
	if c["a2a_agent_defs"] != 2 || c["a2a_agent_def_active"] != 2 || c["a2a_server_card_defs"] != 2 || c["a2a_server_card_def_active"] != 1 {
		t.Fatalf("counts = %v, want 2/2/2/1 (warnings %v)", c, res.Warnings)
	}
	for _, pair := range []struct{ got, want func() (any, error) }{
		{func() (any, error) { return dst.SnapshotReadA2AAgentDefs(context.Background()) }, func() (any, error) { return src.SnapshotReadA2AAgentDefs(context.Background()) }},
		{func() (any, error) { return dst.SnapshotReadA2AServerCardDefs(context.Background()) }, func() (any, error) { return src.SnapshotReadA2AServerCardDefs(context.Background()) }},
	} {
		g, _ := pair.got()
		w, _ := pair.want()
		if !reflect.DeepEqual(normalizeDefs(t, g), normalizeDefs(t, w)) {
			t.Errorf("restored defs differ:\n got %+v\nwant %+v", g, w)
		}
	}
	if !hasWarning(res, "a2a_server_card beta/card", "restored ACTIVE") {
		t.Errorf("no warning that the restored card is live: %v", res.Warnings)
	}
}

// normalizeDefs round-trips rows through JSON with instants in UTC and bodies
// decoded, so two backends' readings compare by value.
func normalizeDefs(t *testing.T, rows any) []map[string]any {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if ts, ok := m["created_at"].(string); ok {
			if p, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				m["created_at"] = p.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	return out
}

// An old snapshot without the webhook and A2A sections restores exactly as
// before: nothing written, no warning.
func TestTriggerDefs_WithoutSectionsRestoreAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_1", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		hookSpawnBody(map[string]any{"user_credentials": map[string]string{"slack": "literal"}}), true)
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_1", Name: "peer", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"agent_card_url": "https://peer.example/card"}, true)
	plantA2ACard(t, src, store.A2AServerCardDefRow{DefID: "ascd_1", Name: "card", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"name": "card", "exposed_agents": []any{map[string]any{"agent_name": "a"}}}, true)
	raw := mustCapture(t, src)
	for _, sec := range []string{"webhook_defs", "webhook_def_active", "a2a_agent_defs", "a2a_agent_def_active",
		"a2a_server_card_defs", "a2a_server_card_def_active"} {
		raw = withoutSection(t, raw, sec)
	}
	var env struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	for k := range env.Sections {
		if strings.HasPrefix(k, "webhook") || strings.HasPrefix(k, "a2a") {
			t.Fatalf("the old-format fixture still has section %s", k)
		}
	}
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators()})
	c := res.Counts()
	for _, k := range []string{"webhook_defs", "webhook_def_active", "a2a_agent_defs", "a2a_agent_def_active",
		"a2a_server_card_defs", "a2a_server_card_def_active", "defs_disabled_for_credentials"} {
		if c[k] != 0 {
			t.Errorf("old-format restore counted %s = %d", k, c[k])
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("old-format restore warned: %v", res.Warnings)
	}
}

// The counters reach the extensible restored map under their section names,
// and defs_disabled_for_credentials counts webhooks and schedules alike.
func TestTriggerDefs_CountersReachTheRestoredMap(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_1", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		hookSpawnBody(map[string]any{"user_credentials": map[string]string{"slack": "literal"}}), true)
	plantSchedules(t, src, plantedSchedule{
		row:    store.ScheduleDefRow{DefID: "sd_1", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body:   map[string]any{"agent": "a", "schedule": "0 6 * * *", "user_credentials": map[string]string{"jobs": "literal"}},
		active: true, next: schedBase(),
	})
	plantA2AAgent(t, src, store.A2AAgentDefRow{DefID: "aad_1", Name: "peer", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"agent_card_url": "https://peer.example/card"}, true)
	plantA2ACard(t, src, store.A2AServerCardDefRow{DefID: "ascd_1", Name: "card", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"name": "card", "exposed_agents": []any{map[string]any{"agent_name": "a"}}}, true)
	c := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: passValidators()}).Counts()
	want := map[string]int{"webhook_defs": 1, "webhook_def_active": 1, "a2a_agent_defs": 1, "a2a_agent_def_active": 1,
		"a2a_server_card_defs": 1, "a2a_server_card_def_active": 1, "schedule_defs": 1, "defs_disabled_for_credentials": 2}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("restored[%q] = %d, want %d (map %v)", k, c[k], v, c)
		}
	}
}
