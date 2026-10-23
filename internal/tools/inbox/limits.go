package inbox

import "context"

// MaxFileBytes is the per-file cap for a run: the operator's when the run
// has an inbox attached, else the default. File op=download holds a file
// written to a durable volume to the same cap as one kept here.
func MaxFileBytes(ctx context.Context) int64 {
	if h, ok := from(ctx); ok {
		return h.svc.maxFile()
	}
	return DefaultMaxFileBytes
}

// ClaimRegistration takes one of the run tree's limit slots for handing a
// secret to the server's redactor, and reports whether one was left. False
// on a run with no inbox state: nothing then bounds what it would register.
func ClaimRegistration(ctx context.Context, limit int) bool {
	h, ok := from(ctx)
	if !ok {
		return false
	}
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	if h.tree.registered >= limit {
		return false
	}
	h.tree.registered++
	return true
}
