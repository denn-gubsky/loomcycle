package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/postgres"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These tests guard what a snapshot may carry (RFC DP P0). The postgres half
// of each backend-parameterised test needs LOOMCYCLE_TEST_PG_DSN; the CI job
// "Go (Postgres tiers)" sets it and runs them. Without it that half SKIPS,
// which is a vacuous pass — so it is a separate CI step, not a hope.

// schemaTabler is the table-listing read both store backends provide outside
// the Store interface.
type schemaTabler interface {
	SchemaTables(ctx context.Context) ([]string, error)
}

// coverageBackend is one store backend a guard runs against.
type coverageBackend struct {
	name string
	bit  backendSet
	// open returns a freshly migrated store, or skips the test.
	open func(t *testing.T) store.Store
}

func coverageBackends() []coverageBackend {
	return []coverageBackend{
		{name: "sqlite", bit: onSQLite, open: func(t *testing.T) store.Store {
			s, err := sqlite.Open(filepath.Join(t.TempDir(), "coverage.db"))
			if err != nil {
				t.Fatalf("sqlite.Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
		{name: "postgres", bit: onPostgres, open: openCoveragePostgres},
	}
}

var coverageSchemaCounter atomic.Uint64

// openCoveragePostgres migrates a per-test schema, the pattern the postgres
// store tests use, so this package's tests cannot collide with theirs (the
// prefix differs) or with each other (the counter).
func openCoveragePostgres(t *testing.T) store.Store {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LOOMCYCLE_TEST_PG_DSN not set; the postgres half of this guard runs in the CI job \"Go (Postgres tiers)\"")
	}
	schema := fmt.Sprintf("snapcov_%d_%d", time.Now().UnixNano()%1_000_000, coverageSchemaCounter.Add(1))
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		drop, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("cleanup dial: %v", err)
			return
		}
		defer drop.Close()
		if _, err := drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	// public stays on the path AFTER the test schema so a database-wide
	// pgvector extension resolves; every table still lands in the test schema.
	s, err := postgres.Open(ctx, postgres.Config{
		DSN:          dsn + sep + "search_path=" + schema + ",public",
		MaxOpenConns: 4,
		AutoMigrate:  true,
	})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func liveTables(t *testing.T, s store.Store) map[string]bool {
	t.Helper()
	st, ok := s.(schemaTabler)
	if !ok {
		t.Fatalf("%T has no SchemaTables; the guard cannot read its schema", s)
	}
	names, err := st.SchemaTables(context.Background())
	if err != nil {
		t.Fatalf("SchemaTables: %v", err)
	}
	// Non-vacuity: an empty or near-empty read means the query is wrong (a
	// wrong schema, a wrong table_type), and every check below would pass.
	if len(names) < 40 {
		t.Fatalf("SchemaTables returned %d tables (%v); a migrated schema has ~50", len(names), names)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// TestSnapshotCoverage_EveryTableIsClassified: every table in a freshly
// migrated schema has a classification, and every classification that applies
// to this backend names a table the schema has.
func TestSnapshotCoverage_EveryTableIsClassified(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			live := liveTables(t, b.open(t))

			var names []string
			for n := range live {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				c, ok := tableCoverageMap[n]
				switch {
				case !ok:
					t.Errorf("table %q is not classified in internal/snapshot/coverage.go. Decide whether a "+
						"restore needs its rows and whether they are safe in a portable file, then add it "+
						"as section, never, omitted or pending.", n)
				case c.Backends&b.bit == 0:
					t.Errorf("table %q exists on %s but its classification does not list that backend", n, b.name)
				}
			}

			var classified []string
			for n := range tableCoverageMap {
				classified = append(classified, n)
			}
			sort.Strings(classified)
			for _, n := range classified {
				c := tableCoverageMap[n]
				if c.Backends&b.bit == 0 || c.Conditional != "" {
					continue
				}
				if !live[n] {
					t.Errorf("coverage.go classifies %q on %s, but the migrated schema has no such table; "+
						"remove the entry (or fix its backends)", n, b.name)
				}
			}
		})
	}
}

var pendingPhase = regexp.MustCompile(`^DP-P[1-8][ab]?$`)

// TestSnapshotCoverage_ClassificationsAreWellFormed: an entry says enough to
// be reviewed — a section entry names a real envelope section and what secret
// material it may hold; every other entry says why, or which phase fixes it.
func TestSnapshotCoverage_ClassificationsAreWellFormed(t *testing.T) {
	sections := envelopeSectionKeys()
	for name, c := range tableCoverageMap {
		if c.Backends == 0 {
			t.Errorf("%s: no backends", name)
		}
		switch c.Kind {
		case coverSection:
			if !sections[c.Section] {
				t.Errorf("%s: section %q is not a key of the Sections struct", name, c.Section)
			}
			if !secretsNoteRE.MatchString(c.Secrets) {
				t.Errorf("%s: secrets note %q must lead with none, by-reference, stripped: or reported:", name, c.Secrets)
			}
		case coverNever, coverOmitted:
			if c.Reason == "" {
				t.Errorf("%s: a %s classification needs a reason", name, c.Kind)
			}
		case coverPending:
			if !pendingPhase.MatchString(c.Phase) {
				t.Errorf("%s: pending needs the phase that adds its section (DP-P<n>), got %q", name, c.Phase)
			}
		default:
			t.Errorf("%s: unknown kind %q", name, c.Kind)
		}
		if c.Kind != coverSection && (c.Section != "" || c.Secrets != "") {
			t.Errorf("%s: only a section classification names a section or a secrets note", name)
		}
		if c.Kind != coverPending && c.Phase != "" {
			t.Errorf("%s: only a pending classification names a phase", name)
		}
	}
}

var secretsNoteRE = regexp.MustCompile(`^(none|by-reference|stripped: \S|reported: \S)`)

// envelopeSectionKeys derives the section keys from the Sections struct's JSON
// tags — the shape Capture writes — rather than from a list.
func envelopeSectionKeys() map[string]bool {
	out := map[string]bool{}
	st := reflect.TypeOf(Sections{})
	for i := 0; i < st.NumField(); i++ {
		if k := jsonKey(st.Field(i)); k != "" {
			out[k] = true
		}
	}
	return out
}

// jsonKey is the key encoding/json writes for a field, or "" when it writes
// none.
func jsonKey(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name
	}
	return f.Name
}

// plantedRow is a row a guard seeds into the store: Present strings must
// appear in the envelope exactly when the row's table is carried by a
// section (so the check is not vacuous, and a pending table whose rows start
// travelling cannot stay classified pending); Absent strings must never
// appear.
type plantedRow struct {
	table   string
	present []string
	absent  []string
}

// Distinctive markers: no fixture elsewhere spells them, so a hit is this row.
const (
	markCredName    = "dp0mark-credential-name-unreferenced"
	markCredCipher  = "dp0mark-credential-ciphertext"
	markTokName     = "dp0mark-operator-token-name"
	markTokSubject  = "dp0mark-operator-token-subject"
	markTokHash     = "dp0mark-operator-token-hash"
	markSchedCred   = "dp0mark-schedule-literal-credential"
	markSchedRef    = "$cred:dp0mark-schedule-reference"
	markHookCred    = "dp0mark-webhook-literal-credential"
	markEnvResolved = "dp0mark-resolved-env-value"
	markProvider    = "dp0mark-usage-provider"
	markModel       = "dp0mark-usage-model"
	markUsageRun    = "dp0mark-usage-run"
	markUsageTenant = "dp0mark-usage-tenant"
	markUser        = "dp0mark-user-subject"
	markLimitUser   = "dp0mark-limit-subject"
	markCarryTenant = "dp0mark-carry-tenant"
	markVolumePath  = "/dp0mark-volume-host-path"
	plantedEnvName  = "LOOMCYCLE_DP0_PLANTED_KEY"
)

func plantNeverSnapshotRows(t *testing.T, s store.Store) []plantedRow {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("plant %s: %v", what, err)
		}
	}

	// A credential whose name no definition references: the name itself must
	// not travel (there is no credential manifest), nor its ciphertext.
	_, err := s.CredentialDefPut(ctx, store.CredentialDefRow{
		TenantID: "acme", Scope: "tenant", Name: markCredName, Backend: "inline",
		Definition: json.RawMessage(`{"value":{"key_id":"k1","nonce":"bm9uY2U=","ciphertext":"` + markCredCipher + `"}}`),
		CreatedAt:  now, UpdatedAt: now,
	})
	must("credential_defs", err)

	_, err = s.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "otd_dp0mark", Name: markTokName, TenantID: "acme", Subject: markTokSubject,
		TokenHash: markTokHash, AllowedScopes: []string{"runs:create"}, CreatedAt: now,
	})
	must("operator_token_defs", err)

	// Trigger definitions holding a LITERAL per-user credential, the form the
	// tools accept with only a warning. The schedule also holds a REFERENCE,
	// which is authored text and must travel exactly as written.
	_, err = s.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID: "sched_dp0mark", Name: "dp0mark-schedule", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"agent":"a","cron":"@hourly","user_credentials":{"GITHUB_TOKEN":"` + markSchedCred +
			`","JOBS":"` + markSchedRef + `"}}`),
	})
	must("schedule_defs", err)
	_, err = s.WebhookDefCreate(ctx, store.WebhookDefRow{
		DefID: "wh_dp0mark", Name: "dp0mark-webhook", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"agent":"a","user_credentials":{"SLACK_TOKEN":"` + markHookCred + `"}}`),
	})
	must("webhook_defs", err)

	// An MCP server def that REFERENCES an env var holding a secret. The
	// reference is authored text and travels; the resolved value must not.
	t.Setenv(plantedEnvName, markEnvResolved)
	ref := "${" + plantedEnvName + "}"
	_, err = s.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID: "mcpdef_dp0mark", Name: "dp0mark-mcp", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"transport":"streamable-http","url":"https://example.test/mcp","headers":{"X-Api-Key":"` + ref + `"}}`),
	})
	must("mcp_server_defs", err)

	// A memory backend and a document source whose api_key_env NAMES that
	// same env var. The name travels as written; the value is never resolved.
	_, err = s.MemoryBackendDefCreate(ctx, store.MemoryBackendDefRow{
		DefID: "mbd_dp0mark", Name: "dp0mark-memory-backend", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"kind":"remote","config":{"base_url":"https://example.test","api_key_env":"` + plantedEnvName + `"}}`),
	})
	must("memory_backend_defs", err)
	_, err = s.DocumentSourceDefCreate(ctx, store.DocumentSourceDefRow{
		DefID: "dsd_dp0mark", Name: "dp0mark-document-source", Version: 1, CreatedAt: now,
		Definition: json.RawMessage(`{"config":{"base_url":"https://example.test","api_key_env":"` + plantedEnvName + `"}}`),
	})
	must("document_source_defs", err)

	// A dynamic volume whose stored path is a marker. Its name and mode
	// travel; the host path never does — the target derives its own.
	_, err = s.VolumeDefCreate(ctx, store.VolumeDefRow{TenantID: "acme", Name: "dp0mark-volume",
		Definition: json.RawMessage(`{"path":"` + markVolumePath + `","mode":"ro"}`)})
	must("volume_defs", err)

	// A billing-ledger row. Only a month-to-date aggregate may ever travel,
	// so none of the row's own fields may — but its tenant does, as the key
	// of that aggregate. The check for it is in the test, since the ledger
	// table itself is classified never.
	must("token_usage", s.RecordCallUsage(ctx, store.TokenUsageRow{
		RunID: markUsageRun, TenantID: markUsageTenant, UserID: "u1", Provider: markProvider, Model: markModel,
		CredentialSource: "operator", InputTokens: 10, OutputTokens: 5, TS: now,
	}))

	must("users", s.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: markUser, AccessMode: "isolated", Status: "active", CreatedAt: now}))
	must("token_limits", s.TokenLimitPut(ctx, store.TokenLimitRow{TenantID: "acme", Scope: "user", ScopeID: markLimitUser, HardLimit: i64p(10), UpdatedAt: now}))
	_, err = s.UsageCarryRaise(ctx, store.UsageCarryRow{TenantID: markCarryTenant, Month: limits.MonthStart(now), Tokens: 42})
	must("usage_carry", err)

	return []plantedRow{
		{table: "credential_defs", absent: []string{markCredName, markCredCipher}},
		{table: "operator_token_defs", absent: []string{markTokName, markTokSubject, markTokHash, "otd_dp0mark"}},
		{table: "schedule_defs", present: []string{"dp0mark-schedule", markSchedRef, `"stripped_credentials":["GITHUB_TOKEN"]`},
			absent: []string{markSchedCred}},
		{table: "webhook_defs", present: []string{"dp0mark-webhook", `"stripped_credentials":["SLACK_TOKEN"]`},
			absent: []string{markHookCred}},
		{table: "mcp_server_defs", present: []string{"dp0mark-mcp", ref}, absent: []string{markEnvResolved}},
		{table: "memory_backend_defs", present: []string{"dp0mark-memory-backend", `"api_key_env":"` + plantedEnvName + `"`},
			absent: []string{markEnvResolved}},
		{table: "document_source_defs", present: []string{"dp0mark-document-source", `"api_key_env":"` + plantedEnvName + `"`},
			absent: []string{markEnvResolved}},
		{table: "volume_defs", present: []string{"dp0mark-volume", `"mode":"ro"`}, absent: []string{markVolumePath, `"path"`}},
		{table: "token_usage", absent: []string{markProvider, markModel, markUsageRun}},
		{table: "users", present: []string{markUser}},
		{table: "token_limits", present: []string{markLimitUser}},
		{table: "usage_carry", present: []string{markCarryTenant}},
	}
}

// TestSnapshotCapture_NeverSnapshotValuesAreAbsent: plant a value in every
// never-snapshot field there is a row for, capture, and search the whole
// envelope for each one.
func TestSnapshotCapture_NeverSnapshotValuesAreAbsent(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			planted := plantNeverSnapshotRows(t, s)

			_, raw, err := Capture(context.Background(), s, CaptureOptions{})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			env := string(raw)

			carried := 0
			for _, p := range planted {
				c, ok := tableCoverageMap[p.table]
				if !ok {
					t.Fatalf("planted table %q is not classified", p.table)
				}
				for _, m := range p.absent {
					if strings.Contains(env, m) {
						t.Errorf("%s: the envelope carries %q, a value that must never be in a snapshot", p.table, m)
					}
				}
				for _, m := range p.present {
					got := strings.Contains(env, m)
					if c.Kind == coverSection && !got {
						t.Errorf("%s is classified section:%s but the envelope lacks %q; the absences "+
							"above prove nothing for it", p.table, c.Section, m)
					}
					if c.Kind != coverSection && got {
						t.Errorf("%s is classified %s but its row reached the envelope (%q); "+
							"reclassify it and review what it carries", p.table, c.Kind, m)
					}
				}
				if c.Kind == coverSection {
					carried++
				}
			}
			// Non-vacuity for the whole search: at least one planted row is in
			// a section today, so the envelope provably holds planted content.
			if carried == 0 {
				t.Fatal("no planted row is carried by a section; the search could be over an empty envelope")
			}
			// The ledger row's absences above prove something only if the
			// ledger was read: its aggregate must be in the month-to-date block.
			if !strings.Contains(env, `{"tenant_id":"`+markUsageTenant+`","user_id":"u1","tokens":15}`) {
				t.Error("the ledger row's month-to-date aggregate is not in the envelope; the ledger absences prove nothing")
			}
		})
	}
}
