package pause

import (
	"context"
	"testing"
	"time"
)

// TestRecordRunPauses_PanickingRecorderStillClosesDone pins the release half
// of the background-goroutine panic net: a recorder that panics is recovered,
// and the done channel the remote pause/resume handler waits on still closes,
// so that handler is not left blocked.
func TestRecordRunPauses_PanickingRecorderStillClosesDone(t *testing.T) {
	rec := func(context.Context, string, time.Time, time.Time) error { panic("recorder bug") }
	done := recordRunPauses(rec, []string{"run-1"}, time.Now(), time.Time{})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done never closed after the recorder panicked")
	}
}
