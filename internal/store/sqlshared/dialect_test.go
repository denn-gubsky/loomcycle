package sqlshared

import (
	"database/sql"
	"testing"
	"time"
)

func TestRebind_NumbersPlaceholdersOutsideLiteralsAndComments(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"none", `SELECT 1`, `SELECT 1`},
		{"empty", ``, ``},
		{"in order", `a = ? AND b = ? OR c IN (?, ?)`, `a = $1 AND b = $2 OR c IN ($3, $4)`},
		{"adjacent", `??`, `$1$2`},
		{"ten and up", `?,?,?,?,?,?,?,?,?,?,?`, `$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11`},
		{"cast suffix", `VALUES (?::jsonb, CAST(? AS INTEGER))`, `VALUES ($1::jsonb, CAST($2 AS INTEGER))`},
		{"string literal", `a = '?' AND b = ?`, `a = '?' AND b = $1`},
		{"doubled quote stays inside", `a = 'it''s ?' AND b = ?`, `a = 'it''s ?' AND b = $1`},
		{"empty literal", `a = '' AND b = ?`, `a = '' AND b = $1`},
		{"backslash is not an escape", `p LIKE ? ESCAPE '\' AND q = ?`, `p LIKE $1 ESCAPE '\' AND q = $2`},
		{"json path literals", `jsonb_set(x, '{mode}', '"ro"') WHERE y = ?`, `jsonb_set(x, '{mode}', '"ro"') WHERE y = $1`},
		{"sqlite json path", `json_extract(x, '$.mode') = ?`, `json_extract(x, '$.mode') = $1`},
		{"quoted identifier", `SELECT "a?b" FROM t WHERE c = ?`, `SELECT "a?b" FROM t WHERE c = $1`},
		{"doubled identifier quote", `SELECT "a""?" WHERE c = ?`, `SELECT "a""?" WHERE c = $1`},
		{"line comment", "a = ? -- why? because\nAND b = ?", "a = $1 -- why? because\nAND b = $2"},
		{"line comment at end", `a = ? -- trailing ?`, `a = $1 -- trailing ?`},
		{"block comment", `a = ? /* is it? */ AND b = ?`, `a = $1 /* is it? */ AND b = $2`},
		{"unterminated block comment", `a = ? /* ? `, `a = $1 /* ? `},
		{"unterminated literal", `a = ? AND b = 'x?`, `a = $1 AND b = 'x?`},
		{"lone dash and slash", `a = ? - 1 / 2 AND b = ?`, `a = $1 - 1 / 2 AND b = $2`},
		{"multibyte", `name = 'é?' AND x = ? AND y = 'ü'`, `name = 'é?' AND x = $1 AND y = 'ü'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Postgres.Rebind(c.in); got != c.want {
				t.Errorf("Postgres.Rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
			if got := SQLite.Rebind(c.in); got != c.in {
				t.Errorf("SQLite.Rebind(%q) = %q, want it unchanged", c.in, got)
			}
		})
	}
}

func TestPlaceholders_IsNCommaSeparatedMarks(t *testing.T) {
	for n, want := range map[int]string{-1: "", 0: "", 1: "?", 3: "?, ?, ?"} {
		if got := Placeholders(n); got != want {
			t.Errorf("Placeholders(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTimeArg_IsUnixNanosForSQLiteAndTheTimeForPostgres(t *testing.T) {
	ts := time.Date(2026, 10, 20, 12, 30, 45, 123456789, time.UTC)
	if got, ok := SQLite.TimeArg(ts).(int64); !ok || got != ts.UnixNano() {
		t.Errorf("SQLite.TimeArg = %#v, want int64 %d", SQLite.TimeArg(ts), ts.UnixNano())
	}
	if got, ok := Postgres.TimeArg(ts).(time.Time); !ok || !got.Equal(ts) {
		t.Errorf("Postgres.TimeArg = %#v, want the time itself", Postgres.TimeArg(ts))
	}
}

func TestTimeDest_SQLiteRoundTripsTimeArgToTheNanosecond(t *testing.T) {
	ts := time.Date(2026, 10, 20, 12, 30, 45, 123456789, time.UTC)
	var got time.Time
	dest := SQLite.TimeDest(&got)
	sc, ok := dest.(sql.Scanner)
	if !ok {
		t.Fatalf("SQLite.TimeDest is %T, want a sql.Scanner", dest)
	}
	if err := sc.Scan(SQLite.TimeArg(ts)); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !got.Equal(ts) {
		t.Errorf("round trip = %v, want %v", got, ts)
	}
	// The stores have always decoded with time.Unix, so a read comes back in
	// the local zone; keep it that way.
	if got.Location() != time.Local {
		t.Errorf("location = %v, want time.Local", got.Location())
	}
}

func TestTimeDest_SQLiteRefusesANonIntegerColumn(t *testing.T) {
	var got time.Time
	sc := SQLite.TimeDest(&got).(sql.Scanner)
	for _, src := range []any{nil, "2026-10-20", 1.5, []byte("1")} {
		if err := sc.Scan(src); err == nil {
			t.Errorf("Scan(%#v) = nil, want an error", src)
		}
	}
}

func TestTimeDest_PostgresIsTheDestinationItself(t *testing.T) {
	var got time.Time
	if dest, ok := Postgres.TimeDest(&got).(*time.Time); !ok || dest != &got {
		t.Errorf("Postgres.TimeDest = %#v, want &got (pgx scans timestamptz natively)", Postgres.TimeDest(&got))
	}
}

func TestJSONFragments_CastOnlyOnPostgres(t *testing.T) {
	if got := SQLite.JSONArg(); got != "?" {
		t.Errorf("SQLite.JSONArg = %q", got)
	}
	if got := Postgres.JSONArg(); got != "?::jsonb" {
		t.Errorf("Postgres.JSONArg = %q", got)
	}
	if got := SQLite.JSONText("doc"); got != "doc" {
		t.Errorf("SQLite.JSONText = %q", got)
	}
	if got := Postgres.JSONText("doc"); got != "doc::text" {
		t.Errorf("Postgres.JSONText = %q", got)
	}
}

func TestEscapeLike_MakesMetacharactersLiteral(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"/docs/":       "/docs/",
		"/a_b/":        `/a\_b/`,
		"/100%/":       `/100\%/`,
		`/back\slash/`: `/back\\slash/`,
		`\_%`:          `\\\_\%`,
	}
	for in, want := range cases {
		if got := EscapeLike(in); got != want {
			t.Errorf("EscapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDialectString_NamesTheEngine(t *testing.T) {
	for d, want := range map[Dialect]string{SQLite: "sqlite", Postgres: "postgres", 0: "dialect(0)"} {
		if got := d.String(); got != want {
			t.Errorf("Dialect(%d).String() = %q, want %q", d, got, want)
		}
	}
}
