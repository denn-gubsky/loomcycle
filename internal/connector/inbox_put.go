package connector

import "io"

// InboxPutRequest is a run owner's file put into the run's inbox (RFC HE F1):
// PUT /v1/runs/{run_id}/inbox/{name} and the gRPC PutInboxFile RPC.
type InboxPutRequest struct {
	RunID string
	// Name is the caller's name for the file. The runtime keeps only a
	// sanitised base name of it; it is never logged or audited.
	Name string
	// MediaType is the type the caller declared (the Content-Type); a generic
	// or empty one is replaced by a sniff of the bytes, as for any kept file.
	MediaType string
	// Body is the file's bytes, read once. Size is its length when known, -1
	// when not: an unknown length is counted as it is read.
	Body io.Reader
	Size int64
}

// InboxPutResult is what was kept.
type InboxPutResult struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

// InboxPutErrorKind classifies a refused put; each transport maps it to its
// own status.
type InboxPutErrorKind int

const (
	// InboxPutNotFound: no such run, or one the caller may not put into —
	// the same answer, so the route is no oracle for another user's runs.
	InboxPutNotFound InboxPutErrorKind = iota + 1
	// InboxPutBadRequest: the request itself is malformed.
	InboxPutBadRequest
	// InboxPutNotLive: the run has ended; its inbox went with it.
	InboxPutNotLive
	// InboxPutElsewhere: the run is live, but not in this process.
	InboxPutElsewhere
	// InboxPutTooBig: over the client put cap (LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES).
	InboxPutTooBig
	// InboxPutInboxFull: over the run tree's inbox quota.
	InboxPutInboxFull
	// InboxPutUnavailable: the put could not be recorded (no dynamic root,
	// the audit sink down); nothing was kept.
	InboxPutUnavailable
)

// InboxPutError is a refused put: Code is the wire code, Msg the reason.
type InboxPutError struct {
	Kind InboxPutErrorKind
	Code string
	Msg  string
}

func (e *InboxPutError) Error() string { return e.Msg }
