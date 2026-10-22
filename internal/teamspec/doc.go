// Package teamspec holds the rules for the names a team owns, which other
// definitions are checked against when they name a team or one of its parts:
// the reserved namespace its own channels live under.
//
// It is a leaf apart from internal/teamgraph and internal/store because the
// config layer checks these rules at load, and config sits below both.
package teamspec
