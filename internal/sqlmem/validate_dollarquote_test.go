package sqlmem

import "testing"

// TestValidate_DollarQuotesArePostgresData covers the two scanners that decide
// whether a statement is single — stripComments and indexOfStatementSeparator —
// which were dialect-BLIND while the validator around them is dialect-aware.
//
// Two consequences, one user-visible and one latent.
//
// VISIBLE: a legitimate `VALUES ($$x; y$$)` was refused as multi-statement, so an
// agent could not store any text containing a semicolon via dollar quoting.
//
// LATENT: `$$--$$` made the stripper delete to end-of-line, hiding a REAL
// separator from the guard the file itself calls load-bearing. That was safe only
// because pgx runs the extended protocol, which rejects multi-statement strings —
// an implicit dependency on a driver detail nobody had written down, and one a
// QueryExecMode change would have spent silently.
func TestValidate_DollarQuotesArePostgresData(t *testing.T) {
	t.Run("valid postgres is accepted", func(t *testing.T) {
		for _, sql := range []string{
			`INSERT INTO t (a) VALUES ($$x; y$$)`,
			`UPDATE t SET a = $$; DROP$$ WHERE a = 1`,
			`INSERT INTO t (a) VALUES ($tag$x; y$tag$)`,
			`INSERT INTO t (a) VALUES ($$a--b$$)`,
		} {
			if err := validateStatementForDialect(sql, false, dialectPostgres); err != nil {
				t.Errorf("refused valid postgres %q: %v", sql, err)
			}
		}
	})

	t.Run("a real separator is still refused", func(t *testing.T) {
		for _, sql := range []string{
			`SELECT 1 $$--$$ ; DROP TABLE t`, // the latent bypass
			`SELECT $$a ; DROP TABLE t`,      // unterminated tag must not swallow it
			`SELECT $tag$a ; DROP TABLE t`,
			`SELECT 1; DROP TABLE t`,
			`SELECT * FROM t WHERE a = $1; DROP TABLE t`,
		} {
			if err := validateStatementForDialect(sql, true, dialectPostgres); err == nil {
				t.Errorf("accepted a second statement in %q", sql)
			}
		}
	})

	t.Run("positional parameters are not quotes", func(t *testing.T) {
		// $1 is a BIND PARAMETER, used by every scope-tier call. Treating it as an
		// opening delimiter would swallow the rest of the statement, so the tag rule
		// must reject a leading digit.
		for _, sql := range []string{
			`SELECT * FROM t WHERE a = $1`,
			`SELECT $$x$$, $1 FROM t`,
			`SELECT $a$ $b$ $a$`,
		} {
			if err := validateStatementForDialect(sql, true, dialectPostgres); err != nil {
				t.Errorf("refused %q: %v", sql, err)
			}
		}
		// The cases above pass even WITHOUT the leading-digit rule, because each $N
		// lacks a closing $ and the unterminated path falls through to keep scanning.
		// This one is what actually exercises the rule: with a digit-led tag accepted,
		// $1$ … $1$ becomes a quoted span and the ';' inside it disappears.
		if err := validateStatementForDialect(
			`SELECT $1$ ; DROP TABLE t $1$`, true, dialectPostgres); err == nil {
			t.Error("a digit-led $1$ was treated as a quote delimiter, hiding a real " +
				"separator — the tag rule must reject a leading digit")
		}
	})

	t.Run("sqlite is unchanged", func(t *testing.T) {
		// sqlite has no $tag$ string form — $name is a bind parameter — so scanning
		// for dollar quotes there would mis-tokenize valid sqlite. It must keep
		// treating the ';' as a separator.
		if err := validateStatementForDialect(`SELECT $$a;b$$`, true, dialectSQLite); err == nil {
			t.Error("the sqlite tier applied postgres dollar-quote rules")
		}
		if err := validateStatementForDialect(`SELECT 1; DROP TABLE t`, true, dialectSQLite); err == nil {
			t.Error("sqlite accepted a second statement")
		}
	})

	t.Run("stripComments leaves dollar bodies intact", func(t *testing.T) {
		got := stripComments(`SELECT $$a--b; DROP TABLE t$$`, true)
		if got != `SELECT $$a--b; DROP TABLE t$$` {
			t.Errorf("stripped inside a dollar-quoted body: %q", got)
		}
		// With the dialect flag off, the old behaviour stands — the flag is what
		// makes this postgres-only.
		if off := stripComments(`SELECT $$a--b$$`, false); off == `SELECT $$a--b$$` {
			t.Error("dollar handling applied even with dollarQuotes=false")
		}
	})
}
