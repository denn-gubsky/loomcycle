package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/postgres"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

var goldenSchemaCounter atomic.Int64

// pgVectorStoreOrSkip opens the Postgres store with pgvector in a schema of its
// own: the tier where Document op=search runs both its legs for real.
func pgVectorStoreOrSkip(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" || os.Getenv("LOOMCYCLE_TEST_PG_VECTOR") != "1" {
		t.Skip("set LOOMCYCLE_TEST_PG_DSN and LOOMCYCLE_TEST_PG_VECTOR=1 to run the Postgres + pgvector search half")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("fjgold_%d_%d", time.Now().UnixNano()%1_000_000, goldenSchemaCounter.Add(1))
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "search_path=" + schema + ",public"
	st, err := postgres.Open(ctx, postgres.Config{DSN: scoped, MaxOpenConns: 8, AutoMigrate: true, PgvectorEnabled: true})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	raw, err := pgxpool.New(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	return st, raw
}

type goldenHit struct {
	ChunkID string
	Score   float64
}

// TestDocumentSearch_TenantResultsUnchangedByTheMigration — the results
// Document op=search gives for a tenant corpus are identical before and after
// its rows move keyspace. "Before" is a corpus laid out as an older binary left
// it: the rows are pushed to the SQL Memory scope id, where the new reader
// finds nothing (asserted, so the move is real), and the migration brings them
// back. Run for a named tenant and for the default one ("" → "default").
func TestDocumentSearch_TenantResultsUnchangedByTheMigration(t *testing.T) {
	for _, tenant := range []string{"tnt", ""} {
		t.Run(fmt.Sprintf("tenant %q", tenant), func(t *testing.T) {
			st, raw := pgVectorStoreOrSkip(t)
			mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mgr.Close() })
			vocab := []string{"quorum", "board", "budget", "audit", "supplier", "vote", "minutes", "chair"}
			d := &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus(), Embedder: newFakeEmbedder("fake", "m1", vocab...)}
			ctx := tools.WithAgentName(context.Background(), "doc-agent")
			ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: tenant})
			ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
				rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
				rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
			})
			for path, bodies := range map[string][]string{
				"/norms/board": {"quorum of the board is two thirds", "the chair casts the deciding vote", "minutes are kept by the secretary"},
				"/norms/audit": {"supplier audit every year", "budget audit before the vote", "audit minutes go to the board"},
			} {
				doc := docRun(t, d, ctx, map[string]any{"op": "create_document", "scope": "tenant", "title": "Norms", "path": path})
				for _, b := range bodies {
					docRun(t, d, ctx, map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"], "title": "Clause", "body": b})
				}
			}
			queries := []map[string]any{
				{"op": "search", "scope": "tenant", "query": "board quorum vote", "top_k": 10},
				{"op": "search", "scope": "tenant", "query": "audit supplier budget", "top_k": 10},
				{"op": "search", "scope": "tenant", "query": "minutes", "top_k": 10, "under_paths": []string{"/norms/audit"}},
			}
			search := func() [][]goldenHit {
				t.Helper()
				out := make([][]goldenHit, len(queries))
				for i, q := range queries {
					res := docRun(t, d, ctx, q)
					rows, _ := res["chunks"].([]any)
					for _, r := range rows {
						h := r.(map[string]any)
						score, _ := h["score"].(float64)
						out[i] = append(out[i], goldenHit{ChunkID: h["chunk_id"].(string), Score: math.Round(score*1e9) / 1e9})
					}
				}
				return out
			}
			golden := search()
			for i, hits := range golden {
				if len(hits) == 0 {
					t.Fatalf("query %d found nothing; the golden would compare empty to empty", i)
				}
			}

			// The old layout: every tenant document row and embedding at the SQL
			// Memory scope id, as an older binary wrote them.
			legacy := sqlmem.ScopeTenant(tenant)
			var pushed int
			if err := raw.QueryRow(context.Background(), `WITH m AS (
				  UPDATE memory SET scope_id = $1 WHERE scope = 'tenant' AND scope_id = '' AND tenant_id = $2
				     AND (key LIKE 'doc.chunk:%' OR key LIKE 'doc.unit:%') RETURNING 1),
				e AS (
				  UPDATE memory_embeddings SET scope_id = $1 WHERE scope = 'tenant' AND scope_id = '' AND tenant_id = $2
				     AND (key LIKE 'doc.chunk:%' OR key LIKE 'doc.unit:%') RETURNING 1)
				SELECT COUNT(*) FROM m`, legacy, tenant).Scan(&pushed); err != nil {
				t.Fatalf("lay the corpus out the old way: %v", err)
			}
			if pushed == 0 {
				t.Fatal("no tenant document rows to push back")
			}
			for i, hits := range search() {
				if len(hits) != 0 {
					t.Fatalf("query %d still finds %d chunks in the old layout — the move did not take", i, len(hits))
				}
			}

			rep, err := st.MigrateTenantDocumentRows(context.Background())
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if rep.Moved != pushed || rep.Conflicts != 0 || rep.Strays != 0 {
				t.Errorf("migration report = %+v, want %d moved and nothing else", rep, pushed)
			}
			if after := search(); !reflect.DeepEqual(after, golden) {
				a, _ := json.Marshal(after)
				g, _ := json.Marshal(golden)
				t.Errorf("results changed by the migration:\nbefore %s\nafter  %s", g, a)
			}
			var leftover int
			if err := raw.QueryRow(context.Background(), `SELECT COUNT(*) FROM memory WHERE scope = $1 AND scope_id <> ''`,
				string(store.MemoryScopeTenant)).Scan(&leftover); err != nil || leftover != 0 {
				t.Errorf("rows left at a non-empty tenant scope id: %d (%v)", leftover, err)
			}
		})
	}
}
