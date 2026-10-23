package connector

import (
	"bytes"
	"encoding/json"
)

// internalSpecKey is the run-config key that is storage-only: whose
// access-control rules bound the run (the caller's subject, token id and
// token class), which a resume and a restarted queued run read to narrow
// the run again. It is the server's to read, not a reader's.
const internalSpecKey = "acl_origin"

// IsStorageOnlySpecKey reports whether a run-config key is kept out of every
// served form of the record (ServedRunSpec), so a client type need not — and
// should not — declare it.
func IsStorageOnlySpecKey(key string) bool { return key == internalSpecKey }

// ServedRunSpec is a stored run-config record as a reader is served it — the
// record with every storage-only key removed, at the top and inside the
// override offer a sub-run's record carries from its parent
// (spawn.offered_overrides.record, itself a record). Every surface that
// serves a run's spec or config calls it: HTTP, gRPC and the connector the
// MCP tools use. A record without the key is returned byte for byte; one
// that is not a JSON object is returned as is.
func ServedRunSpec(raw json.RawMessage) json.RawMessage {
	if !bytes.Contains(raw, []byte(`"`+internalSpecKey+`"`)) {
		return raw
	}
	out, ok := stripSpec(raw)
	if !ok {
		return raw
	}
	return out
}

func stripSpec(raw json.RawMessage) (json.RawMessage, bool) {
	var rec map[string]json.RawMessage
	if json.Unmarshal(raw, &rec) != nil || rec == nil {
		return nil, false
	}
	delete(rec, internalSpecKey)
	if spawn, ok := nested(rec["spawn"]); ok {
		if offer, ok := nested(spawn["offered_overrides"]); ok {
			if inner, ok := stripSpec(offer["record"]); ok {
				offer["record"] = inner
				spawn["offered_overrides"] = mustJSON(offer)
				rec["spawn"] = mustJSON(spawn)
			}
		}
	}
	return mustJSON(rec), true
}

func nested(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// mustJSON marshals a map of already-valid JSON values, which cannot fail.
func mustJSON(m map[string]json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}
