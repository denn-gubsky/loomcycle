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
