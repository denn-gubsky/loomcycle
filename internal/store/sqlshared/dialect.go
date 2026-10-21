// Package sqlshared is the small layer the SQLite and Postgres stores share,
// so a domain's query text and row scanning are written once instead of
// mirrored in internal/store/sqlite and internal/store/postgres.
//
// The SQL stays hand-written. A shared query is written with bare `?`
// placeholders in the subset of SQL both engines accept; the Postgres
// adapter rebinds it to `$n`. Where the engines genuinely differ (how a time
// or a JSON document is stored, a JSON function), the Dialect supplies the
// fragment or the bound value. Anything the subset can't express — Postgres
// FOR UPDATE / advisory locks, SQLite BEGIN IMMEDIATE — stays in the backend.
//
// Domains move here one at a time; each backend's method becomes a one-line
// call. Unconverted domains are untouched.
package sqlshared

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Dialect names the SQL engine a statement runs on.
type Dialect uint8

const (
	// SQLite stores times as INTEGER unix nanoseconds and JSON as TEXT.
	SQLite Dialect = iota + 1
	// Postgres stores times as timestamptz and JSON as jsonb, and numbers
	// its placeholders $1, $2, ...
	Postgres
)

func (d Dialect) String() string {
	switch d {
	case SQLite:
		return "sqlite"
	case Postgres:
		return "postgres"
	default:
		return "dialect(" + strconv.Itoa(int(d)) + ")"
	}
}

// Rebind rewrites the bare `?` placeholders in query into this dialect's
// form: unchanged for SQLite, `$1`, `$2`, ... in order for Postgres.
//
// A `?` is left alone inside a single-quoted string literal (two quotes in a
// row are an escaped quote, not its end), a double-quoted identifier, a `--`
// line comment or a `/* */` block comment. Postgres-only lexical forms —
// dollar-quoted strings, E-prefixed escape strings, nested block comments,
// the jsonb `?` operators — are not recognised; a shared query can't use
// them anyway, since SQLite has none of them. Numbered SQLite placeholders
// (`?2`) are not supported either.
func (d Dialect) Rebind(query string) string {
	if d != Postgres || strings.IndexByte(query, '?') < 0 {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 0
	// Byte-wise is safe on UTF-8: every byte this switch looks for is ASCII,
	// and no byte of a multi-byte sequence is.
	for i := 0; i < len(query); {
		c := query[i]
		var end int // one past the end of a span copied verbatim
		switch {
		case c == '\'' || c == '"':
			end = quotedEnd(query, i)
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end = len(query)
			if j := strings.IndexByte(query[i:], '\n'); j >= 0 {
				end = i + j
			}
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end = len(query)
			if j := strings.Index(query[i+2:], "*/"); j >= 0 {
				end = i + 2 + j + 2
			}
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
			continue
		default:
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteString(query[i:end])
		i = end
	}
	return b.String()
}

// quotedEnd returns one past the closing quote of the literal or identifier
// opening at query[start]. A doubled quote is an escaped quote, not the end.
// An unterminated literal runs to the end of the query.
func quotedEnd(query string, start int) int {
	q := query[start]
	for i := start + 1; i < len(query); i++ {
		if query[i] != q {
			continue
		}
		if i+1 < len(query) && query[i+1] == q {
			i++ // the escaped quote
			continue
		}
		return i + 1
	}
	return len(query)
}

// Placeholders is n comma-separated `?`, for an IN list. Rebind numbers them.
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// TimeArg is t as this dialect binds it: unix nanoseconds for SQLite's
// INTEGER columns, the time itself for Postgres's timestamptz.
func (d Dialect) TimeArg(t time.Time) any {
	if d == Postgres {
		return t
	}
	return t.UnixNano()
}

// TimeDest is a Scan destination that fills *dst from a NOT NULL time
// column written by TimeArg. Postgres scans timestamptz natively; SQLite's
// INTEGER nanoseconds are converted with time.Unix(0, n), as the stores
// always have.
func (d Dialect) TimeDest(dst *time.Time) any {
	if d == Postgres {
		return dst
	}
	return &unixNanoDest{dst: dst}
}

type unixNanoDest struct{ dst *time.Time }

// Scan implements sql.Scanner.
func (u *unixNanoDest) Scan(src any) error {
	n, ok := src.(int64)
	if !ok {
		return fmt.Errorf("sqlshared: time column holds %T, want int64 unix nanoseconds", src)
	}
	*u.dst = time.Unix(0, n)
	return nil
}

// JSONArg is the placeholder for a JSON document bound as a string. Postgres
// casts it to jsonb (an explicit cast, so it holds under the simple query
// protocol too); SQLite stores the text as given.
func (d Dialect) JSONArg() string {
	if d == Postgres {
		return "?::jsonb"
	}
	return "?"
}

// JSONText selects the JSON column col as text, so it scans into a string.
func (d Dialect) JSONText(col string) string {
	if d == Postgres {
		return col + "::text"
	}
	return col
}

// EscapeLike escapes the LIKE metacharacters (%, _, \) in s so it matches
// literally. Pair it with `LIKE ? ESCAPE '\'`, which both engines accept.
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
