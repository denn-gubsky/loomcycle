// Package workspace holds what the two ends of the workspace worker protocol
// (RFC GW, proto/workspace.proto) agree on: the protocol version, the op and
// error-class names, the chunk size and the rule a workspace id must meet.
// The core's dispatcher is internal/workspace/dispatch; the worker is
// internal/workspace/worker.
package workspace

import (
	"strings"
)

// ProtocolVersion is the workspace worker protocol this build speaks (RFC GW
// §8). 2 added the worker-to-core Egress stream (RFC GW P4): an older worker
// has no way out for its sessions, and an older core refuses the stream as
// unimplemented. 3 added git requests on that stream (RFC GW P11).
const ProtocolVersion = 3

// MinProtocolVersion is the oldest worker protocol the core still attaches:
// a v2 worker serves everything but git workspaces, so a fleet need not be
// upgraded at once. A worker of a version outside
// [MinProtocolVersion, ProtocolVersion] is refused at attach.
const MinProtocolVersion = 2

// GitProtocolVersion is the first protocol whose workers make git requests;
// a git stream from an older worker is refused (ClassProtocolTooOld).
const GitProtocolVersion = 3

// ClassProtocolTooOld names a refusal of something the worker's protocol
// predates.
const ClassProtocolTooOld = "worker_protocol_too_old"

// ChunkSize bounds one Chunk frame's data, either way (RFC GW §8).
const ChunkSize = 64 << 10

// MDAuthorization is the metadata key a worker's bearer travels in.
const MDAuthorization = "authorization"

// The ops the core's bookkeeping sends. The Workspace tool's ops are in
// ops.go; an op is a name and JSON arguments, so new ones need no protocol
// change.
const (
	// OpOpen makes the workspace's directory, refusing at capacity.
	OpOpen = "open"
	// OpRemove deletes the workspace's directory; absent is success.
	OpRemove = "remove"
	// OpStatus reports whether the workspace's directory exists.
	OpStatus = "status"
)

// The error classes a worker answers an op with, in OpDone.error_class.
const (
	// ClassAtCapacity: the worker holds as many workspaces as it may. The
	// core places the workspace on the next worker.
	ClassAtCapacity = "at_capacity"
	// ClassNotFound: the workspace is not on the worker's disk.
	ClassNotFound = "not_found"
	// ClassInvalid: the op's workspace id or arguments are malformed.
	ClassInvalid = "invalid"
	// ClassUnsupportedOp: the worker does not know the op.
	ClassUnsupportedOp = "unsupported_op"
	// ClassFailed: the op failed on the worker (a file system error).
	ClassFailed = "failed"
	// ClassRefused: the worker will not run the command, such as one whose
	// program the operator did not list.
	ClassRefused = "refused"
	// ClassOverQuota: the write would pass the workspace's disk quota, or a
	// command already took it past (the workspace is marked full until a
	// measurement finds it under again). Nothing was written.
	ClassOverQuota = "workspace_over_quota"
	// ClassPathNotFound: the file or folder the op names is not in the
	// workspace (the workspace itself is: that is ClassNotFound).
	ClassPathNotFound = "path_not_found"
	// ClassExists: the target is already there and the op did not ask to
	// overwrite it.
	ClassExists = "exists"
	// ClassSizeMismatch: a put's input carried other than its declared size.
	// Nothing was written.
	ClassSizeMismatch = "size_mismatch"
	// ClassCopyLimit: a folder copy passes the file count or byte total it
	// may copy; the detail names which. Nothing was written.
	ClassCopyLimit = "copy_limit"
	// ClassNotRegular: the op names a FIFO, a socket or a device, which the
	// worker never opens.
	ClassNotRegular = "not_regular_file"
)

// The error classes the core adds for an op that never reached a worker's
// answer.
const (
	// ClassWorkerLost: the worker went away before the op finished.
	ClassWorkerLost = "worker_lost"
	// ClassWorkerRevoked: the worker was detached because its token no
	// longer attaches it.
	ClassWorkerRevoked = "worker_revoked"
	// ClassNoWorker: no attached worker can take the op.
	ClassNoWorker = "no_worker"
	// ClassBusy: the worker kept no idle stream free long enough.
	ClassBusy = "busy"
	// ClassOutputTooLarge: the worker sent more output than the op's bound.
	ClassOutputTooLarge = "output_too_large"
	// ClassProtocol: the worker broke the protocol.
	ClassProtocol = "protocol"
)

// KnownWorkerClasses are the classes a worker may answer with that the core
// reports by name; any other is reported as ClassFailed, so a worker cannot
// grow a metric's label set or put arbitrary text where a class is read.
var KnownWorkerClasses = map[string]bool{
	ClassAtCapacity: true, ClassNotFound: true, ClassInvalid: true, ClassUnsupportedOp: true, ClassFailed: true, ClassRefused: true,
	ClassOverQuota: true, ClassPathNotFound: true, ClassExists: true, ClassSizeMismatch: true, ClassCopyLimit: true, ClassNotRegular: true,
	ClassHelperMissing: true, ClassConversionCrashed: true, ClassConversionUnsupported: true, ClassConversionFailed: true,
	ClassConversionBusy: true, ClassSourceChanged: true, ClassUnitTooLarge: true, ClassUnpackRefused: true,
	ClassKernelUnavailable: true, ClassCheckpointLimit: true, ClassCheckpointNotFound: true,
	ClassServerLimit: true, ClassServerNotFound: true, ClassPortNotServed: true, ClassPortUnreachable: true,
	ClassTemplateMissing: true, ClassNotOnRoot: true, ClassRootBusy: true,
	ClassBrowserUnavailable: true, ClassBrowserBusy: true, ClassBrowserTimedOut: true,
}

// The engines a worker may run commands under, weakest first.
const (
	EngineHost      = "host"
	EngineContainer = "container"
)

// EngineRank orders engines by isolation; 0 is an engine this build does not
// know, which satisfies no requirement.
func EngineRank(engine string) int {
	switch engine {
	case EngineHost:
		return 1
	case EngineContainer:
		return 2
	}
	return 0
}

// MaxIDLen bounds a workspace id.
const MaxIDLen = 64

// ValidID reports whether id may name a workspace's directory: 1 to MaxIDLen
// characters of ASCII letters, digits, '-' and '_', not starting with '-'. It
// is the whole of the check that keeps an op inside the worker's root, so it
// admits no separator, no dot and nothing a shell or a path reads specially.
func ValidID(id string) bool {
	if id == "" || len(id) > MaxIDLen || strings.HasPrefix(id, "-") {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Stats is the core's view of the workspace workers, for /metrics.
type Stats struct {
	// Workers is the workers attached to this replica.
	Workers int
	// Ops counts ops by "op/outcome"; outcome is ok or an error class.
	Ops map[string]uint64
	// Deaths counts workers lost to missed heartbeats or with ops in flight.
	Deaths uint64
	// OutputBytes counts the bytes workers sent the core, by op: what
	// crossed from a worker (RFC GW D34 keeps a converted file on it).
	OutputBytes map[string]uint64
}

// StatusAnswer is a status op's output.
type StatusAnswer struct {
	Exists bool `json:"exists"`
}
