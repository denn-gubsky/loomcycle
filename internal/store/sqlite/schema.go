package sqlite

// schemaPlan is the statements migrate executes, grouped by pass. A section of
// the registry is a schemaPlan too, holding one area's statements.
type schemaPlan struct {
	tables  []string
	columns []string
	indexes []string
	fixups  []string
}

// schemaSections is the registry migrate executes. Each pass (all tables,
// then all column adds, then all indexes, then all data fixups — see migrate)
// runs every section's statements for that pass in this order, so the order
// below is the execution order and must not change: an existing database
// replays it on every boot.
//
// The area sections hold the tables pass (each table with the indexes that
// need only its CREATE-time columns) and, for channels, the data fixups. The
// column adds and the indexes that may name an added column are the upgrade
// history and run across areas, so they are their own sections, last.
func schemaSections() []schemaPlan {
	return []schemaPlan{
		runsSection(),            // schema_runs.go
		memorySection(),          // schema_memory.go
		channelsSection(),        // schema_channels.go
		authSection(),            // schema_auth.go
		oauthSection(),           // schema_auth.go
		agentDefsSection(),       // schema_defs.go
		serviceDefsSection(),     // schema_defs.go
		integrationDefsSection(), // schema_defs.go
		volumesSection(),         // schema_volumes.go
		runtimeSection(),         // schema_runtime.go
		columnsSection1(),        // schema_upgrade.go
		columnsSection2(),        // schema_upgrade.go
		indexesSection(),         // schema_upgrade.go
	}
}

// migrationPlan flattens the registry into the four passes migrate runs. The
// backfills and table rebuilds between the column adds and the indexes stay in
// migrate.
func migrationPlan() schemaPlan {
	var p schemaPlan
	for _, s := range schemaSections() {
		p.tables = append(p.tables, s.tables...)
		p.columns = append(p.columns, s.columns...)
		p.indexes = append(p.indexes, s.indexes...)
		p.fixups = append(p.fixups, s.fixups...)
	}
	return p
}
