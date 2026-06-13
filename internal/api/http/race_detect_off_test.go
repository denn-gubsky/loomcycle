//go:build !race

package http

// raceEnabled is false in a non-race test build. See race_detect_test.go for
// why load/scale tests consult it. Kept in a separate file so exactly one of
// the two definitions compiles per build.
const raceEnabled = false
