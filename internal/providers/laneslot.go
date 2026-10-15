package providers

import "context"

// laneSlotKey carries the call lane's slot number on a call's ctx.
type laneSlotKey struct{}

// WithLaneSlot attaches the number of the lane slot a call was granted (RFC
// EI P4c). The call lanes set it on the ctx they hand a driver; a driver whose
// backend can pin a request to one of its own slots (llama.cpp's id_slot)
// reads it, so a run's next call — which a reservation keeps on the same lane
// slot — lands on the backend slot holding its prompt cache.
func WithLaneSlot(ctx context.Context, slot int) context.Context {
	return context.WithValue(ctx, laneSlotKey{}, slot)
}

// LaneSlot is the call's lane slot number; ok is false for a call no lane
// granted (an uncapped provider).
func LaneSlot(ctx context.Context) (slot int, ok bool) {
	slot, ok = ctx.Value(laneSlotKey{}).(int)
	return slot, ok
}
