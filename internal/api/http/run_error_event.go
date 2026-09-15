package http

import (
	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// runErrorEvent builds the terminal EventError for a failed run, classified.
//
// WHY A HELPER RATHER THAN FIVE CALL SITES: a census found five writers of a
// run's terminal error event — the streaming run path, the detached path, the
// continue-a-session path, the resume path and the resident sub-agent path.
// Teaching each one to classify means the sixth forgets, and the failure is
// silent: the event still goes out, just without the half a caller needs.
//
// WHY IT MATTERS ON THIS SURFACE: once the SSE stream is open the HTTP status
// is already 200, so a consumer's only signal is the event type plus an English
// string. "Retry in five seconds" and "your budget is exhausted" arrive looking
// identical, and they want opposite responses. The ADMISSION path never had
// this problem — it answers before the stream opens and has always emitted a
// typed code plus Retry-After. This closes the mid-run half.
//
// An unclassified failure yields the event exactly as before, so nothing that
// the runtime cannot categorise acquires an invented category.
func runErrorEvent(runErr error) providers.Event {
	ev := providers.Event{
		Type:  providers.EventError,
		Error: runErr.Error(),
	}
	if info, ok := errclassify.CategoryOf(runErr); ok {
		ev.ErrorInfo = &info
	}
	return ev
}
