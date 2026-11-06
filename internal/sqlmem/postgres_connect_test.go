package sqlmem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A scope role connects through the aux database's PUBLIC CONNECT. These tests
// cover what happens when a shared Postgres has revoked it: the boot probe sees
// it, and a scope op's error names the remedy instead of a bare FATAL.

func TestExplainConnectDenied_NamesGrantRemedyAndKeepsCause(t *testing.T) {
	cause := &pgconn.PgError{Severity: "FATAL", Code: "42501", Message: `permission denied for database "loomcycle_sqlmem"`}
	err := explainConnectDenied("loomcycle_sqlmem", fmt.Errorf("connect: %w", cause))

	msg := err.Error()
	for _, want := range []string{
		`GRANT CONNECT ON DATABASE "loomcycle_sqlmem" TO PUBLIC`,
		"per scope",                      // why the runtime does not grant it itself
		`permission denied for database`, // the original server message survives
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("the wrapped error lost the original *pgconn.PgError: %v", err)
	}
}

func TestExplainConnectDenied_LeavesOtherErrorsUnchanged(t *testing.T) {
	for _, cause := range []error{
		&pgconn.PgError{Code: "42501", Message: `permission denied for table notes`},
		&pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "x"`},
		errors.New("dial tcp: connection refused"),
		nil,
	} {
		if got := explainConnectDenied("db", cause); got != cause {
			t.Errorf("explainConnectDenied changed %v into %v", cause, got)
		}
	}
}

// pgScratchDB creates a database of its own for a test that must change a
// database-level ACL, so no other test's aux DB is ever altered. It returns the
// scratch DSN, its name, and an admin connection to it. Skips without the aux
// DSN, or when the aux admin may not create databases.
func pgScratchDB(t *testing.T) (dsn, name string, admin *sql.DB) {
	t.Helper()
	auxDSN := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	if auxDSN == "" {
		t.Skip("set LOOMCYCLE_TEST_SQLMEM_PG_DSN to run the postgres SQL Memory tests")
	}
	u, err := url.Parse(auxDSN)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Skip("the scratch-database tests need a URL-form LOOMCYCLE_TEST_SQLMEM_PG_DSN")
	}
	aux, err := sql.Open("pgx", auxDSN)
	if err != nil {
		t.Fatalf("open aux: %v", err)
	}
	defer func() { _ = aux.Close() }()

	name = fmt.Sprintf("sqlmem_connect_%d_%d", os.Getpid(), time.Now().UnixNano())
	ctx := context.Background()
	if _, err := aux.ExecContext(ctx, `CREATE DATABASE `+q(name)); err != nil {
		if pgCodeIn(err, "42501") {
			t.Skip("the aux admin lacks CREATEDB, so no scratch database can be made")
		}
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		aux, err := sql.Open("pgx", auxDSN)
		if err != nil {
			t.Errorf("reopen aux for cleanup: %v", err)
			return
		}
		defer func() { _ = aux.Close() }()
		if _, err := aux.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+q(name)); err != nil {
			t.Errorf("drop scratch database %s: %v", name, err)
		}
	})

	su := *u
	su.Path = "/" + name
	dsn = su.String()
	admin, err = sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	// Registered after the DROP cleanup, so it runs first: DROP DATABASE refuses
	// while this connection is open.
	t.Cleanup(func() { _ = admin.Close() })
	return dsn, name, admin
}

func TestPgPublicHasConnect_TracksThePublicGrant(t *testing.T) {
	_, name, admin := pgScratchDB(t)
	ctx := context.Background()

	if ok, err := pgPublicHasConnect(ctx, admin); err != nil || !ok {
		t.Fatalf("fresh database: pgPublicHasConnect = %v, %v; want true (the default ACL grants PUBLIC CONNECT)", ok, err)
	}
	if _, err := admin.ExecContext(ctx, `REVOKE CONNECT ON DATABASE `+q(name)+` FROM PUBLIC`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok, err := pgPublicHasConnect(ctx, admin); err != nil || ok {
		t.Fatalf("after REVOKE: pgPublicHasConnect = %v, %v; want false", ok, err)
	}
	if _, err := admin.ExecContext(ctx, `GRANT CONNECT ON DATABASE `+q(name)+` TO PUBLIC`); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if ok, err := pgPublicHasConnect(ctx, admin); err != nil || !ok {
		t.Fatalf("after GRANT: pgPublicHasConnect = %v, %v; want true", ok, err)
	}
}

func TestPostgres_ScopeConnectDeniedNamesTheGrantRemedy(t *testing.T) {
	// A unique scope keeps the cluster-global LOGIN role apart from every other
	// test's roles. Its cleanup is registered before the scratch database's so it
	// runs after the DROP DATABASE: the role holds privileges inside that
	// database, and DROP ROLE refuses until they are gone.
	key := agentKey("t-connect", fmt.Sprintf("probe-%d", time.Now().UnixNano()))
	_, role, err := pgScopeNames(key)
	if err != nil {
		t.Fatal(err)
	}
	if auxDSN := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN"); auxDSN != "" {
		t.Cleanup(func() {
			aux, err := sql.Open("pgx", auxDSN)
			if err != nil {
				return
			}
			defer func() { _ = aux.Close() }()
			if _, err := aux.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+q(role)); err != nil {
				t.Errorf("drop scope role: %v", err)
			}
		})
	}

	dsn, name, admin := pgScratchDB(t)
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `REVOKE CONNECT ON DATABASE `+q(name)+` FROM PUBLIC`); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Boot must still succeed: the admin owns the database and connects anyway,
	// and a deployment that never uses SQL Memory must not be taken down.
	m, err := NewPostgres(ctx, Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 100})
	if err != nil {
		t.Fatalf("NewPostgres with PUBLIC CONNECT revoked: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	_, err = m.Exec(ctx, key, "CREATE TABLE notes (id INT)", nil, 0)
	if err == nil {
		t.Fatal("Exec succeeded with PUBLIC CONNECT revoked; want a connect error")
	}
	if !strings.Contains(err.Error(), `GRANT CONNECT ON DATABASE `+q(name)+` TO PUBLIC`) {
		t.Fatalf("error does not name the remedy: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error lost the original 42501: %v", err)
	}
}
