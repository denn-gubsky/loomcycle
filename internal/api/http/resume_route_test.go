package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A queue-aware run resumed is resolved as a run start, as RunOnce resolves a
// continuation (owner ruling, 2026-11-11): the lanes as they are now decide,
// so a busy provider is passed over. The row records no started model, so no
// started model is preferred over the pick.
func TestResume_QueueAwareRunIsRoutedAsARunStart(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	run := pausedRunRow(t, f.s, "worker", "a_queue_resume", false, oneUserSeg("go"))
	f.fillBusy(t)
	resumeOne(t, f.s)
	f.assertResolvedAfterTheWait(t, run.ID)
}

// ...and, resolved as a run start with the image its history carries marked,
// it is not passed to a provider that cannot take the image.
func TestResume_QueueAwareImageRunIsNotPassedToATextOnlyProvider(t *testing.T) {
	f := newQueueRouteFixture(t, false)
	run := pausedRunRow(t, f.s, "worker", "a_queue_image_resume", false, imageUserSeg())
	f.fillBusy(t)
	resumeOne(t, f.s)
	waitRunStatus(t, f.s.store, run.ID, store.RunCompleted)
	f.assertRanOnBusy(t, nil)
}

// A queue-aware resumed run that waited for its run slot is resolved again
// when it gets one, as RunOnce resolves a run that queued: busy was free when
// it was routed and full when it started.
func TestResume_QueueAwareRunThatWaitedIsResolvedAgainWhenItStarts(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	free := f.queueBehindHeldSlot(t)
	run := pausedRunRow(t, f.s, "worker", "a_queue_wait_resume", false, oneUserSeg("go"))
	resumeOne(t, f.s)
	waitFor(t, "the resumed run to wait for a slot", func() bool { return f.s.sem.Stats().Queued == 1 })
	f.fillBusy(t)
	free()
	f.assertResolvedAfterTheWait(t, run.ID)
}
