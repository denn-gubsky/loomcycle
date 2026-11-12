package connector

import "github.com/denn-gubsky/loomcycle/internal/concurrency"

// NoteQueueBound copies a full-queue refusal's figures onto the result: the
// result is where a spawn's typed error is flattened to a string, so this is
// the last point they can be read.
func (r *SpawnRunResult) NoteQueueBound(err error) {
	if capacity, queuedNow, ok := concurrency.QueueBound(err); ok {
		r.QueueCapacity, r.QueuedNow = capacity, queuedNow
	}
}
