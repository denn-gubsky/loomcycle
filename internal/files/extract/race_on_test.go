//go:build race

package extract

// raceEnabled says the race detector is on: it allocates beside every heap
// object, so the heap ceilings in membounds_test.go are scaled by raceSlack.
const raceEnabled = true
