package snapshot

import (
	"encoding/json"
	"fmt"
)

// Export serialises a snapshot envelope to canonical JSON bytes
// suitable for writing to disk + restoring on a different host.
// Wrapper around json.Marshal that documents the wire-export contract.
//
// The canonical form is:
//   - Compact (no indentation; smallest wire payload)
//   - UTF-8 encoded
//   - Stable field ordering per the Envelope struct's field
//     declaration order (Go's encoding/json honours this)
//
// Operators wanting a pretty-printed export for human inspection
// can pipe through `jq` or set up their own ExportPretty wrapper —
// the canonical form is what the restore path consumes, so keep it
// minimal.
//
// Export does NOT include an envelope-level checksum or signature.
// That's left to the transport layer: HTTP responses include
// Content-Length; the snapshots table stores byte_size; operators
// transferring snapshot files between hosts use external tools
// (sha256sum, signed transfer) when integrity matters.
func Export(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("snapshot export: nil envelope")
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("snapshot export: marshal: %w", err)
	}
	return b, nil
}

// ExportPretty produces indented JSON for human inspection. Used by
// the CLI's `loomcycle snapshot get --pretty` flag. NOT used by
// Restore — restoration parses canonical JSON via Export's output
// shape.
func ExportPretty(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("snapshot export pretty: nil envelope")
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("snapshot export pretty: marshal: %w", err)
	}
	return b, nil
}
