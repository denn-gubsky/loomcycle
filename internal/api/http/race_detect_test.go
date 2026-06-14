//go:build race

package http

// raceEnabled reports whether the test binary was built with the race
// detector (`go test -race`). Load/scale tests use it to cap their scale: the
// race detector serialises memory access and slows execution ~10×, so a
// high-scale load test under -race saturates shared resources and flakes on a
// load artifact rather than catching a real data race. The companion
// !race build sets this to false.
const raceEnabled = true
