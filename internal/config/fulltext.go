package config

import (
	"fmt"
	"regexp"
)

// FullTextConfig is the memory.fulltext block: how the Postgres word leg of a
// memory or document search reads text. The leg was hard-wired to the
// `english` text search configuration, so on a corpus in another language an
// inflected term never met its own other forms and the leg contributed nothing
// — the vector leg alone carried the search.
//
// It is server-wide, not per scope: the words are indexed in ONE generated
// column on memory_embeddings, shared by every tenant. Changing it rebuilds
// that column at the next boot (see the postgres store). Postgres-only; the
// sqlite tier has no word leg and ignores the block.
type FullTextConfig struct {
	// Config names a Postgres text search configuration (`english`, `simple`,
	// `russian`, or one an operator created — e.g. a Ukrainian hunspell
	// dictionary, which Postgres does not ship). Empty = english, which is
	// what every deployment had before the block existed.
	Config string `yaml:"config"`
	// Unaccent strips diacritics before indexing and before querying, through
	// the `unaccent` extension (installed on demand).
	Unaccent bool `yaml:"unaccent"`
}

// ftsConfigName is what memory.fulltext.config may hold: an unquoted
// identifier, optionally schema-qualified. The name is written into DDL (a
// generated column's expression cannot take a bind parameter), so the shape
// check is what keeps it from being anything but a name.
var ftsConfigName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// EffectiveConfig is Config with the default applied.
func (f FullTextConfig) EffectiveConfig() string {
	if f.Config == "" {
		return "english"
	}
	return f.Config
}

func (f FullTextConfig) validate() error {
	if f.Config != "" && !ftsConfigName.MatchString(f.Config) {
		return fmt.Errorf("memory.fulltext.config %q is not a text search configuration name (letters, digits and _, optionally schema.name)", f.Config)
	}
	return nil
}
