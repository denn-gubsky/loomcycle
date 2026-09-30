package snapshot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// authoringValidators are the validators the production call sites wire for
// these two sections: the tools' own create/fork checks, judged against a
// host that lists the peers these tests name — and declares the operator
// layer's "ds" source in yaml, whose own url (docs.internal) it does not list.
func authoringValidators() map[string]func(json.RawMessage) error {
	cfg := &config.Config{
		Env: config.Env{HTTPHostAllowlist: []string{"peer.example", "docs.example", "from-source.example", "live.example"}},
		DocumentSources: map[string]config.DocumentSource{
			"ds": {Config: config.DocumentSourceConfig{BaseURL: "http://docs.internal:8080"}},
		},
	}
	v := passValidators()
	v[migrations.SectionMemoryBackendDefs] = builtin.MemoryBackendDefBodyValidator(cfg)
	v[migrations.SectionDocSourceDefs] = builtin.DocumentSourceDefBodyValidator(cfg)
	return v
}

func plantMemoryBackend(t *testing.T, s store.Store, row store.MemoryBackendDefRow, body map[string]any, active bool) {
	t.Helper()
	ctx := context.Background()
	row.Definition = mustJSON(t, body)
	if _, err := s.SnapshotRestoreMemoryBackendDef(ctx, row); err != nil {
		t.Fatalf("plant memory backend %s: %v", row.DefID, err)
	}
	if active {
		if _, err := s.SnapshotRestoreMemoryBackendDefActive(ctx, store.MemoryBackendDefActiveEntry{
			TenantID: row.TenantID, Name: row.Name, DefID: row.DefID, PromotedAt: row.CreatedAt, PromotedByAgentID: "planter",
		}); err != nil {
			t.Fatalf("plant memory backend active %s: %v", row.DefID, err)
		}
	}
}

func plantDocSource(t *testing.T, s store.Store, row store.DocumentSourceDefRow, body map[string]any, active bool) {
	t.Helper()
	ctx := context.Background()
	row.Definition = mustJSON(t, body)
	if _, err := s.SnapshotRestoreDocumentSourceDef(ctx, row); err != nil {
		t.Fatalf("plant document source %s: %v", row.DefID, err)
	}
	if active {
		if _, err := s.SnapshotRestoreDocumentSourceDefActive(ctx, store.DocumentSourceDefActiveEntry{
			TenantID: row.TenantID, Name: row.Name, DefID: row.DefID, PromotedAt: row.CreatedAt, PromotedByAgentID: "planter",
		}); err != nil {
			t.Fatalf("plant document source active %s: %v", row.DefID, err)
		}
	}
}

func remoteBackendBody(baseURL, keyEnv string) map[string]any {
	return map[string]any{"name": "mb", "kind": "remote", "fallback_on_error": "inprocess",
		"config": map[string]any{"base_url": baseURL, "api_version": "v1", "api_key_env": keyEnv}}
}

func docSourceBody(baseURL, keyEnv string) map[string]any {
	return map[string]any{"name": "ds", "config": map[string]any{"base_url": baseURL, "api_key_env": keyEnv}}
}

// Every memory-backend and document-source def comes back under its own
// tenant with every column and its active pointer — two tenants and the
// operator layer, a tenant fork of an operator-layer parent, a retired
// version — through the real authoring validators; the restored active defs
// resolve at once through the lookups the Memory and Document tools use, so
// nothing needs a restart; and a re-restore writes nothing.
func TestMemorySourceDefs_RoundTripKeepsEveryColumnAndGoesLive(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	base := triggerBase()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_op", Name: "mb", Version: 1, CreatedAt: base,
		Description: "operator", CreatedByAgentID: "ag", CreatedByRunID: "run1", BootstrappedFromStatic: true},
		remoteBackendBody("https://peer.example", "LOOMCYCLE_DP4_PEER_KEY"), true)
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_acme", TenantID: "acme", Name: "mb", Version: 1,
		ParentDefID: "mbd_op", CreatedAt: base.Add(time.Minute)}, map[string]any{"name": "mb", "kind": "inprocess"}, true)
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_acme2", TenantID: "acme", Name: "mb", Version: 2,
		ParentDefID: "mbd_acme", CreatedAt: base.Add(2 * time.Minute), Retired: true}, map[string]any{"name": "mb", "kind": "inprocess"}, false)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_beta", TenantID: "beta", Name: "ds", Version: 1, CreatedAt: base,
		CreatedByAgentID: "ag2"}, docSourceBody("https://docs.example", "LOOMCYCLE_DP4_DOC_KEY"), true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_op", Name: "ds", Version: 1, CreatedAt: base, BootstrappedFromStatic: true},
		docSourceBody("http://docs.internal:8080", ""), true)

	raw := mustCapture(t, src)
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: authoringValidators()})
	c := res.Counts()
	if c["memory_backend_defs"] != 3 || c["memory_backend_def_active"] != 2 || c["document_source_defs"] != 2 || c["document_source_def_active"] != 2 {
		t.Fatalf("counts = %v, want 3/2/2/2 (warnings %v)", c, res.Warnings)
	}
	for _, read := range []func(store.Store) (any, error){
		func(s store.Store) (any, error) { return s.SnapshotReadMemoryBackendDefs(context.Background()) },
		func(s store.Store) (any, error) { return s.SnapshotReadDocumentSourceDefs(context.Background()) },
		func(s store.Store) (any, error) { return s.SnapshotReadMemoryBackendDefActive(context.Background()) },
		func(s store.Store) (any, error) { return s.SnapshotReadDocumentSourceDefActive(context.Background()) },
	} {
		g, _ := read(dst)
		w, _ := read(src)
		if !reflect.DeepEqual(normalizeRows(t, g), normalizeRows(t, w)) {
			t.Errorf("restored rows differ:\n got %+v\nwant %+v", g, w)
		}
	}

	// Live without a restart: the lookups read the store per call.
	ctx := context.Background()
	if mb, _, ok := lookup.MemoryBackend(ctx, dst, nil, "", "mb"); !ok || mb.Config.BaseURL != "https://peer.example" || mb.Config.APIKeyEnv != "LOOMCYCLE_DP4_PEER_KEY" {
		t.Errorf("operator-layer backend resolves (ok=%v) to %+v", ok, mb)
	}
	if mb, _, ok := lookup.MemoryBackend(ctx, dst, nil, "acme", "mb"); !ok || mb.Kind != "inprocess" {
		t.Errorf("acme's backend resolves (ok=%v) to %+v, want its own inprocess fork", ok, mb)
	}
	if ds, _, ok := lookup.DocumentSource(ctx, dst, nil, "beta", "ds"); !ok || ds.Config.BaseURL != "https://docs.example" {
		t.Errorf("beta's source resolves (ok=%v) to %+v", ok, ds)
	}

	again := mustRestore(t, dst, raw, RestoreOptions{Validators: authoringValidators()})
	for _, k := range []string{"memory_backend_defs", "memory_backend_def_active", "document_source_defs", "document_source_def_active"} {
		if again.Counts()[k] != 0 {
			t.Errorf("re-restore counted %s = %d", k, again.Counts()[k])
		}
	}
}

// normalizeRows compares rows by value across two stores: instants in UTC,
// bodies decoded.
func normalizeRows(t *testing.T, rows any) []map[string]any {
	t.Helper()
	out := normalizeDefs(t, rows)
	for _, m := range out {
		if ts, ok := m["promoted_at"].(string); ok {
			if p, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				m["promoted_at"] = p.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	return out
}

// The exfiltration pair: a hand-edited envelope points a def at a URL the
// authoring tool refuses, or names one of loomcycle's own infrastructure
// secrets as the key to send. Each such def is skipped with a per-row
// warning, its pointer is refused with it (so the target keeps no pointer at
// a def it does not hold), the valid rows land, and no warning carries the
// refused URL's credential.
func TestMemorySourceDefs_RefusedBodyIsSkippedWithItsPointer(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	base := triggerBase()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_ok", Name: "mb-ok", Version: 1, CreatedAt: base},
		remoteBackendBody("https://peer.example", "LOOMCYCLE_DP4_PEER_KEY"), true)
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_exfil", TenantID: "acme", Name: "mb-exfil", Version: 1, CreatedAt: base},
		remoteBackendBody("https://peer.example", "LOOMCYCLE_DP4_PEER_KEY"), true)
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_file", Name: "mb-file", Version: 1, CreatedAt: base},
		remoteBackendBody("https://peer.example", ""), true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_ok", Name: "ds-ok", Version: 1, CreatedAt: base},
		docSourceBody("https://docs.example", ""), true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_leak", Name: "ds-leak", Version: 1, CreatedAt: base},
		docSourceBody("https://docs.example", ""), true)

	const pass, tok = "dp4-base-url-password", "dp4-base-url-token"
	raw := editSection(t, mustCapture(t, src), "memory_backend_defs", func(sec map[string]any) {
		for _, x := range sec["entries"].([]any) {
			e := x.(map[string]any)
			cfg := e["definition"].(map[string]any)["config"].(map[string]any)
			switch e["def_id"] {
			case "mbd_exfil":
				cfg["api_key_env"] = "LOOMCYCLE_AUTH_TOKEN"
			case "mbd_file":
				cfg["base_url"] = "file:///etc/passwd"
			}
		}
	})
	raw = editSection(t, raw, "document_source_defs", func(sec map[string]any) {
		for _, x := range sec["entries"].([]any) {
			e := x.(map[string]any)
			if e["def_id"] == "dsd_leak" {
				// Fails to parse, so the authoring error quotes it whole.
				e["definition"].(map[string]any)["config"].(map[string]any)["base_url"] =
					"https://dp4:" + pass + "@docs.example/%zz?token=" + tok
			}
		}
	})

	dst, dstClose := newTestStore(t)
	defer dstClose()
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: authoringValidators()})
	if res.MemoryBackendDefsRestored != 1 || res.MemoryBackendDefActiveRestored != 1 ||
		res.DocSourceDefsRestored != 1 || res.DocSourceDefActiveRestored != 1 {
		t.Errorf("counts = %v; want only the valid backend and source", res.Counts())
	}
	for _, refused := range []struct{ where, why string }{
		{"memory_backend_def acme/mb-exfil v1 (def mbd_exfil)", "not an allowed credential env var"},
		{"memory_backend_def mb-file v1 (def mbd_file)", "http or https"},
		{"document_source_def ds-leak v1 (def dsd_leak)", "not a valid URL"},
	} {
		if !hasWarning(res, refused.where, "not restored", refused.why) {
			t.Errorf("no warning refusing %s (%s): %v", refused.where, refused.why, res.Warnings)
		}
	}
	ctx := context.Background()
	for _, id := range []string{"mbd_exfil", "mbd_file"} {
		if _, err := dst.MemoryBackendDefGet(ctx, id); err == nil {
			t.Errorf("the refused def %s was written", id)
		}
	}
	if _, err := dst.DocumentSourceDefGet(ctx, "dsd_leak"); err == nil {
		t.Error("the refused def dsd_leak was written")
	}
	if _, err := dst.MemoryBackendDefGetActive(ctx, "acme", "mb-exfil"); err == nil {
		t.Error("a pointer at the refused exfiltration def was written")
	}
	if _, _, ok := lookup.MemoryBackend(ctx, dst, nil, "acme", "mb-exfil"); ok {
		t.Error("acme's runs would route memory through the refused def")
	}
	if res.ActivePointersRefused != 3 {
		t.Errorf("active_pointers_refused = %d, want 3 (one per refused def)", res.ActivePointersRefused)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, pass) || strings.Contains(w, tok) {
			t.Errorf("a warning carries the refused URL's credential: %s", w)
		}
	}
}

// A new section is never restored unvalidated: with no validator wired for
// it, every row and every pointer is skipped with a warning.
func TestMemorySourceDefs_UnwiredValidatorSkipsEveryRow(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_1", Name: "mb", Version: 1, CreatedAt: triggerBase()},
		remoteBackendBody("https://peer.example", ""), true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_1", Name: "ds", Version: 1, CreatedAt: triggerBase()},
		docSourceBody("https://docs.example", ""), true)

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: map[string]func(json.RawMessage) error{
		// The other new sections are wired, so only these two are unwired.
		migrations.SectionWebhookDefs: func(json.RawMessage) error { return nil },
	}})
	c := res.Counts()
	for _, k := range []string{"memory_backend_defs", "memory_backend_def_active", "document_source_defs", "document_source_def_active"} {
		if c[k] != 0 {
			t.Errorf("an unwired validator still restored %s = %d", k, c[k])
		}
	}
	if !hasWarning(res, "memory_backend_def mb v1", "no memory_backend_defs validator is wired") ||
		!hasWarning(res, "document_source_def ds v1", "no document_source_defs validator is wired") {
		t.Errorf("no per-row warning for the unwired validators: %v", res.Warnings)
	}
	if rows, _ := dst.SnapshotReadMemoryBackendDefs(context.Background()); len(rows) != 0 {
		t.Errorf("an unvalidated def was written: %+v", rows)
	}
}

// api_key_env is an env var NAME: it travels as written and is never
// resolved, so the value the source process holds is in neither the envelope
// nor the restored body.
func TestMemorySourceDefs_KeyEnvTravelsAsANameNeverAValue(t *testing.T) {
	const name, value = "LOOMCYCLE_DP4_RESOLVED_KEY", "dp4-resolved-env-value"
	t.Setenv(name, value)
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_1", Name: "mb", Version: 1, CreatedAt: triggerBase()},
		remoteBackendBody("https://peer.example", name), true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_1", Name: "ds", Version: 1, CreatedAt: triggerBase()},
		docSourceBody("https://docs.example", name), true)

	raw := mustCapture(t, src)
	if strings.Contains(string(raw), value) {
		t.Fatal("the envelope carries the resolved env value")
	}
	if strings.Count(string(raw), `"api_key_env":"`+name+`"`) != 2 {
		t.Fatalf("the envelope does not carry the env NAME on both defs")
	}
	mustRestore(t, dst, raw, RestoreOptions{Validators: authoringValidators()})
	mb, _ := dst.MemoryBackendDefGet(context.Background(), "mbd_1")
	ds, _ := dst.DocumentSourceDefGet(context.Background(), "dsd_1")
	for _, body := range []json.RawMessage{mb.Definition, ds.Definition} {
		if strings.Contains(string(body), value) || !strings.Contains(string(body), name) {
			t.Errorf("restored body = %s; want the env name and not its value", body)
		}
	}
}

// The missing-credential scan names, for each restored def that dials a
// peer, the env var a call would send when this host does not set it — the
// tenant's own var for a key_per_tenant pattern — and nothing else: not the
// value, and not a var an in-process backend never reads.
func TestMemorySourceDefs_MissingKeyScanNamesTheReference(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	base := triggerBase()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_remote", Name: "mb", Version: 1, CreatedAt: base},
		remoteBackendBody("https://peer.example", "LOOMCYCLE_DP4_UNSET_PEER"), true)
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_local", Name: "mb-local", Version: 1, CreatedAt: base},
		map[string]any{"kind": "inprocess", "config": map[string]any{"api_key_env": "LOOMCYCLE_DP4_NEVER_READ"}}, true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_tenant", TenantID: "acme", Name: "ds", Version: 1, CreatedAt: base},
		map[string]any{"config": map[string]any{"base_url": "https://docs.example", "api_key_env": "LOOMCYCLE_DP4_FALLBACK"},
			"tenancy_strategy": map[string]any{"kind": "key_per_tenant", "env_pattern": "LOOMCYCLE_DP4_DOC_{tenant_id}"}}, true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_set", Name: "ds-set", Version: 1, CreatedAt: base},
		docSourceBody("https://docs.example", "LOOMCYCLE_DP4_IS_SET"), true)

	var asked []string
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{
		Validators: authoringValidators(),
		EnvSet: func(n string) bool {
			asked = append(asked, n)
			return n == "LOOMCYCLE_DP4_IS_SET"
		},
	})
	if !hasWarning(res, "missing credential", "memory_backend_def mb:", "config.api_key_env", "LOOMCYCLE_DP4_UNSET_PEER") {
		t.Errorf("no warning naming the remote backend's unset key: %v", res.Warnings)
	}
	if !hasWarning(res, "missing credential", "document_source_def acme/ds:", "tenancy_strategy.env_pattern", "LOOMCYCLE_DP4_DOC_acme") {
		t.Errorf("no warning naming acme's own per-tenant key: %v", res.Warnings)
	}
	for _, n := range []string{"LOOMCYCLE_DP4_NEVER_READ", "LOOMCYCLE_DP4_FALLBACK", "LOOMCYCLE_DP4_IS_SET"} {
		if hasWarning(res, n) {
			t.Errorf("warned about %s, which this host either sets or never reads: %v", n, res.Warnings)
		}
	}
	if !reflect.DeepEqual(asked, []string{"LOOMCYCLE_DP4_UNSET_PEER", "LOOMCYCLE_DP4_IS_SET", "LOOMCYCLE_DP4_DOC_acme"}) {
		t.Errorf("env checks asked = %v", asked)
	}
}

// The live row stands: a different def already on the target's (tenant,
// name, version) — its own yaml bootstrap — is a per-row warning, not an
// overwrite, and the pointer at the refused def is not written.
func TestMemorySourceDefs_LiveRowStands(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_src", Name: "mb", Version: 1, CreatedAt: triggerBase(), BootstrappedFromStatic: true},
		remoteBackendBody("https://from-source.example", ""), true)
	plantMemoryBackend(t, dst, store.MemoryBackendDefRow{DefID: "mbd_live", Name: "mb", Version: 1, CreatedAt: triggerBase(), BootstrappedFromStatic: true},
		remoteBackendBody("https://live.example", ""), true)

	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: authoringValidators()})
	if res.MemoryBackendDefsRestored != 0 || res.MemoryBackendDefActiveRestored != 0 {
		t.Errorf("counted %v, want nothing restored", res.Counts())
	}
	if !hasWarning(res, "memory_backend_def mb v1 (def mbd_src)", "live definition stands") {
		t.Errorf("no per-row warning for the collision: %v", res.Warnings)
	}
	if mb, _, _ := lookup.MemoryBackend(context.Background(), dst, nil, "", "mb"); mb.Config.BaseURL != "https://live.example" {
		t.Errorf("the live def was replaced: %+v", mb)
	}
}

// These bodies hold no header map today, but the findings scan walks them,
// so one added later is reported by location like every other literal header.
func TestMemorySourceDefs_HeaderLiteralIsAFinding(t *testing.T) {
	s, closeFn := newTestStore(t)
	defer closeFn()
	const literal = "dp4-literal-header-bearer-0123456789abcdef"
	body := docSourceBody("https://docs.example", "")
	body["headers"] = map[string]any{"Authorization": "Bearer " + literal}
	plantDocSource(t, s, store.DocumentSourceDefRow{DefID: "dsd_hdr", TenantID: "acme", Name: "ds", Version: 1, CreatedAt: triggerBase()}, body, true)

	var env struct {
		Sections Sections `json:"sections"`
	}
	if err := json.Unmarshal(mustCapture(t, s), &env); err != nil {
		t.Fatal(err)
	}
	f := env.Sections.CaptureFindings
	if f == nil || len(f.Entries) != 1 || f.Entries[0].Section != "document_source_defs" ||
		f.Entries[0].DefID != "dsd_hdr" || f.Entries[0].Field != "headers.Authorization" {
		t.Fatalf("capture_findings = %+v, want one finding at document_source_defs dsd_hdr headers.Authorization", f)
	}
	if strings.Contains(f.Entries[0].Warning(), literal) {
		t.Error("the finding carries the literal")
	}
}

// An old snapshot without these sections restores exactly as before.
func TestMemorySourceDefs_WithoutSectionsRestoreAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantMemoryBackend(t, src, store.MemoryBackendDefRow{DefID: "mbd_1", Name: "mb", Version: 1, CreatedAt: triggerBase()},
		remoteBackendBody("https://peer.example", "LOOMCYCLE_DP4_UNSET_PEER"), true)
	plantDocSource(t, src, store.DocumentSourceDefRow{DefID: "dsd_1", Name: "ds", Version: 1, CreatedAt: triggerBase()},
		docSourceBody("https://docs.example", ""), true)
	raw := mustCapture(t, src)
	for _, sec := range []string{"memory_backend_defs", "memory_backend_def_active", "document_source_defs", "document_source_def_active"} {
		raw = withoutSection(t, raw, sec)
	}
	var env struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	for k := range env.Sections {
		if strings.HasPrefix(k, "memory_backend") || strings.HasPrefix(k, "document_source") {
			t.Fatalf("the old-format fixture still has section %s", k)
		}
	}
	res := mustRestore(t, dst, raw, RestoreOptions{Validators: authoringValidators()})
	for _, k := range []string{"memory_backend_defs", "memory_backend_def_active", "document_source_defs", "document_source_def_active"} {
		if res.Counts()[k] != 0 {
			t.Errorf("old-format restore counted %s = %d", k, res.Counts()[k])
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("old-format restore warned: %v", res.Warnings)
	}
}
