package sqlite

import "context"

// widenedLegacyKeys lists tables whose key gained a column other than
// tenant_id after the table first shipped, with the key they had before.
// rebuildTenantBlindKeys covers the tenant axis; this covers the rest, by the
// same reasoning: SQLite cannot alter a PRIMARY KEY, so on a database created
// before the change the old key still fires and refuses rows the new key
// admits.
//
// usage_archive gained purpose: an archived day holds one row per purpose
// (chat, ocr, stt), which the six-column key would refuse as a conflict, and
// the rollup's ON CONFLICT names the seven-column key, which an old table does
// not have at all. No row collides under the new key — it is the old one plus
// a column every pre-existing row holds as 'chat'.
//
// It then gained tier and user_tier (postgres 0118) for the same reason, so a
// database is on one of two old keys; both are listed, and a rebuild from
// either lands on the current one. Every pre-existing row holds an empty
// string in both.
var widenedLegacyKeys = []struct {
	table string
	key   []string
}{
	{"usage_archive", []string{"period_start", "tenant_id", "user_id", "provider", "model", "credential_source"}},
	{"usage_archive", []string{"period_start", "tenant_id", "user_id", "provider", "model", "credential_source", "purpose"}},
}

// rebuildWidenedKeys rebuilds every table still on a key in widenedLegacyKeys
// from its current CREATE TABLE. Like rebuildTenantBlindKeys it inspects the
// live schema, so a fresh or already-rebuilt database is a no-op, and it must
// run after the ADD COLUMN block and before addIndexes.
func (s *Store) rebuildWidenedKeys(ctx context.Context, schema []string) error {
	var tables []string
	for _, lk := range widenedLegacyKeys {
		has, err := hasConstraintKey(ctx, s.db, lk.table, lk.key)
		if err != nil {
			return err
		}
		if has {
			tables = append(tables, lk.table)
		}
	}
	return s.rebuildWithForeignKeysOff(ctx, tables, schema)
}
