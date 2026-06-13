package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
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
// Export stamps an additive "sha256:<hex>" Checksum over the canonical
// Sections bytes (exp7 I4) so Restore can detect a truncated/tampered body
// before touching the store. It remains backward-compatible — Restore only
// verifies the digest when present, so older readers and older snapshots are
// unaffected. Transport-level integrity (Content-Length, the snapshots
// table's byte_size, signed transfer) still applies on top.
func Export(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("snapshot export: nil envelope")
	}
	// Hash the canonical Sections bytes — identical to the "sections" value
	// that ends up in the marshalled document, since encoding/json marshals
	// a field value the same standalone as embedded. Stamp on a copy so the
	// caller's envelope is not mutated.
	sectionBytes, err := json.Marshal(env.Sections)
	if err != nil {
		return nil, fmt.Errorf("snapshot export: marshal sections: %w", err)
	}
	out := *env
	out.Checksum = sectionChecksum(sectionBytes)
	b, err := json.Marshal(&out)
	if err != nil {
		return nil, fmt.Errorf("snapshot export: marshal: %w", err)
	}
	return b, nil
}

// sectionChecksum returns the canonical "sha256:<hex>" digest of the given
// section bytes. Shared by Export (stamp) and Restore (verify).
func sectionChecksum(sectionBytes []byte) string {
	sum := sha256.Sum256(sectionBytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ExportPretty produces indented JSON for human inspection. Used by
// the CLI's `loomcycle snapshot get --pretty` flag. NOT the canonical
// restore artifact — restoration consumes Export's compact output.
//
// It clears the integrity Checksum on its copy. Restore (exp7 I4) verifies
// the digest against the "sections" bytes exactly as they appear in the
// document, but json.MarshalIndent re-indents the nested "sections" object —
// so a checksum stamped over the compact bytes (which every Capture/Export
// envelope carries) no longer matches the indented bytes, and Restore would
// reject the pretty document as "truncated or tampered". A pretty doc
// therefore carries no checksum: if one is ever fed to Restore it skips
// verification (additive) instead of failing on a digest that cannot survive
// re-indentation.
func ExportPretty(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("snapshot export pretty: nil envelope")
	}
	out := *env
	out.Checksum = ""
	b, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("snapshot export pretty: marshal: %w", err)
	}
	return b, nil
}
