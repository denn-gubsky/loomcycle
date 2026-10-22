// Package teamspec holds the rules a team's names and variables are held to,
// which other definitions are checked against when they name a team or one of
// its parts: a team's own name, its variable names and values, and the
// reserved namespace its own channels live under.
//
// It is a leaf apart from internal/teamgraph and internal/store because the
// config layer checks these rules at load (a schedule's or webhook's team
// delivery, an operator channel name), and config sits below both.
package teamspec
