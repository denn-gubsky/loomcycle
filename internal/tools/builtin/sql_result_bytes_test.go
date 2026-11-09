package builtin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The byte cap on the SQL an agent writes, at the two tool surfaces that run it:
// Memory sql_query (in and out of sql_begin) and Document query_chunks sql.

const sqlResultBudget = 64 << 10

// sqlResultManager builds a SQL Memory manager with the test byte budget on the
// named tier, or skips the postgres half without its DSN.
func sqlResultManager(t *testing.T, tier string) *sqlmem.Manager {
	t.Helper()
	cfg := sqlmem.Config{StatementTimeoutMS: 30000, MaxRows: 10000, MaxResultBytes: sqlResultBudget}
	if tier == "sqlite" {
		cfg.Root = t.TempDir()
		mgr, err := sqlmem.New(cfg)
		if err != nil {
			t.Fatalf("sqlmem.New: %v", err)
		}
		t.Cleanup(func() { _ = mgr.Close() })
		return mgr
	}
	dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if dsn == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres-tier half")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	dropAllSqlmemScopes(t, raw)
	cfg.PgDSN = dsn
	mgr, err := sqlmem.NewPostgres(context.Background(), cfg)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		dropAllSqlmemScopes(t, raw)
		_ = raw.Close()
	})
	return mgr
}

// sqlResultOut decodes a {columns, rows, truncated} tool result.
func sqlResultOut(t *testing.T, res tools.Result) (rows [][]any, truncated bool) {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Text)
	}
	var out struct {
		Rows      [][]any `json:"rows"`
		Truncated bool    `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v (%.200s)", err, res.Text)
	}
	return out.Rows, out.Truncated
}

// giantCellSQL is a one-row SELECT computing a value over the budget on the
// tier — nothing stored, so a scope quota does not bound it. It stays under
// twice the budget, the longest value an agent's statement may build, so the
// result cap is what refuses it (sqlmem's length_limit_test.go covers values
// past that limit).
func giantCellSQL(tier string) string {
	n := sqlResultBudget * 3 / 2
	if tier == "postgres" {
		return fmt.Sprintf("SELECT repeat('z', %d)", n)
	}
	return fmt.Sprintf("SELECT printf('%%.*c', %d, 'z')", n)
}

// TestMemorySQL_ResultBytesAreCapped: sql_query over rows totalling more than
// the byte budget comes back cut and truncated, inside a transaction too, and a
// computed first row larger than the budget is an error, on both tiers.
func TestMemorySQL_ResultBytesAreCapped(t *testing.T) {
	for _, tier := range []string{"sqlite", "postgres"} {
		t.Run(tier, func(t *testing.T) {
			mgr := sqlResultManager(t, tier)
			s, err := sqlite.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			tool := &Memory{Store: s, SqlMem: mgr}
			ctx := tools.WithAgentName(context.Background(), "bytes-agent")
			ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t", AgentID: "a", RootRunID: "run-bytes-" + tier})
			ctx = withSqlScopes(ctx, "agent")
			run := func(payload string) tools.Result {
				t.Helper()
				res, err := tool.Execute(ctx, json.RawMessage(payload))
				if err != nil {
					t.Fatalf("%.120s -> %v", payload, err)
				}
				return res
			}
			mustOK := func(payload string) {
				t.Helper()
				if res := run(payload); res.IsError {
					t.Fatalf("%.120s -> %s", payload, res.Text)
				}
			}

			mustOK(`{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE wide (id INTEGER, body TEXT)"}`)
			ins := "INSERT INTO wide (id, body) VALUES (?, ?)"
			if tier == "postgres" {
				ins = "INSERT INTO wide (id, body) VALUES ($1, $2)"
			}
			body := strings.Repeat("x", 8192)
			for i := 1; i <= 20; i++ {
				mustOK(`{"op":"sql_exec","scope":"agent","statement":"` + ins + `","args":[` + strconv.Itoa(i) + `,"` + body + `"]}`)
			}

			const query = `{"op":"sql_query","scope":"agent","statement":"SELECT id, body FROM wide ORDER BY id"}`
			check := func(where string) {
				t.Helper()
				rows, truncated := sqlResultOut(t, run(query))
				if !truncated || len(rows) == 0 || len(rows) >= 20 {
					t.Fatalf("%s: rows=%d truncated=%v; ~160 KiB of rows under a 64 KiB budget must come back cut and truncated",
						where, len(rows), truncated)
				}
			}
			check("auto-commit")

			mustOK(`{"op":"sql_begin","scope":"agent"}`)
			check("in sql_begin")
			mustOK(`{"op":"sql_rollback","scope":"agent"}`)

			res := run(`{"op":"sql_query","scope":"agent","statement":"` + giantCellSQL(tier) + `"}`)
			if !res.IsError || !strings.Contains(res.Text, "result limit") {
				t.Fatalf("a 96 KiB first row under a 64 KiB budget: is_error=%v %.200s; want an error naming the result limit",
					res.IsError, res.Text)
			}
		})
	}
}

// TestDocumentQueryChunksSQL_ResultBytesAreCapped: query_chunks' raw sql is
// held to the same budget on both tiers.
func TestDocumentQueryChunksSQL_ResultBytesAreCapped(t *testing.T) {
	for _, tier := range []string{"sqlite", "postgres"} {
		t.Run(tier, func(t *testing.T) {
			d, ctx := sqlResultDocument(t, tier)
			many := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 20) SELECT i, printf('%.*c', 8192, 'x') FROM n"
			if tier == "postgres" {
				many = "SELECT i, repeat('x', 8192) FROM generate_series(1, 20) AS i"
			}
			_, res := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"`+many+`"}`)
			rows, truncated := sqlResultOut(t, res)
			if !truncated || len(rows) == 0 || len(rows) >= 20 {
				t.Fatalf("rows=%d truncated=%v; want the result cut at the byte budget and truncated", len(rows), truncated)
			}

			_, res = docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"`+giantCellSQL(tier)+`"}`)
			if !res.IsError || !strings.Contains(res.Text, "result limit") {
				t.Fatalf("a 96 KiB first row: is_error=%v %.200s; want an error naming the result limit", res.IsError, res.Text)
			}
		})
	}
}

// TestDocument_ImageOverResultByteCapStillServes: the runtime's own read of an
// image's bytes is not an agent's result and is not held to the budget — an
// image larger than it (but within the asset cap) is still served whole, while
// the same bytes asked for through query_chunks sql are refused.
func TestDocument_ImageOverResultByteCapStillServes(t *testing.T) {
	d, ctx := sqlResultDocument(t, "sqlite")
	doc, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Pictures"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	chunk, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+asStr(doc["document_id"])+`","title":"Big"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	img := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0xab}, 2*sqlResultBudget)...)
	if _, r := docExec(t, d, ctx, `{"op":"set_asset","scope":"user","id":"`+asStr(chunk["id"])+`","media_type":"image/png","data":"`+
		base64.StdEncoding.EncodeToString(img)+`"}`); r.IsError {
		t.Fatalf("set_asset: %s", r.Text)
	}
	mt, data, ok, err := d.ReadAsset(ctx, "user", asStr(chunk["id"]))
	if err != nil || !ok || mt != "image/png" || !bytes.Equal(data, img) {
		t.Fatalf("ReadAsset: ok=%v err=%v media=%q len=%d, want the whole %d-byte image", ok, err, mt, len(data), len(img))
	}
	if _, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user","sql":"SELECT bytes FROM chunk_assets"}`); !r.IsError {
		t.Fatalf("query_chunks sql returned an image over the result budget: %.200s", r.Text)
	}
}

// sqlResultDocument is a Document over a byte-capped manager on the tier.
func sqlResultDocument(t *testing.T, tier string) (*Document, context.Context) {
	t.Helper()
	mgr := sqlResultManager(t, tier)
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := tools.WithAgentName(context.Background(), "bytes-doc")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	return &Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}, ctx
}
