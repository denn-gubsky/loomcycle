package http

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The restore counter map and the capture warnings are declared on every
// surface a client reads them from. Each surface is a hand-written mirror, and
// a field added to one and not another is how five def counters went
// unreported everywhere — so each mirror is checked by name.

func declares(t *testing.T, fields []string, key string) bool {
	t.Helper()
	for _, f := range fields {
		if f == key {
			return true
		}
	}
	return false
}

func TestSnapshotWire_RestoredMapOnEverySurface(t *testing.T) {
	const key = "restored"
	if !declares(t, jsonFields(t, snapshotRestoreResponse{}), key) {
		t.Fatalf("HTTP snapshotRestoreResponse does not send %q", key)
	}
	if !declares(t, jsonFields(t, connector.RestoreSnapshotResult{}), key) {
		t.Errorf("connector.RestoreSnapshotResult (and so MCP) does not carry %q", key)
	}
	for _, src := range []struct{ path, start string }{
		{"../../../adapters/ts/src/types.ts", "export interface SnapshotRestoreResponse {"},
		{"../../../web/src/api.ts", "export interface SnapshotRestoreResponse {"},
	} {
		if !declaredNames(tsFieldRe, sourceBlock(t, src.path, src.start, "\n}"))[key] {
			t.Errorf("%s %q does not declare %q", src.path, src.start, key)
		}
	}
	proto := sourceBlock(t, "../../../proto/loomcycle.proto", "message RestoreSnapshotResponse {", "\n}")
	if !strings.Contains(proto, "map<string, int32> "+key+" =") {
		t.Errorf("proto RestoreSnapshotResponse does not declare %q", key)
	}
	py := sourceBlock(t, "../../../adapters/python/loomcycle/client.py", "async def restore_snapshot(", "\n    async def ")
	if !strings.Contains(py, `"`+key+`":`) {
		t.Errorf("the Python restore_snapshot does not map %q", key)
	}
}

func TestSnapshotWire_CaptureWarningsOnEverySurface(t *testing.T) {
	const key = "warnings"
	if !declares(t, jsonFields(t, snapshotCreateResponse{}), key) {
		t.Fatalf("HTTP snapshotCreateResponse does not send %q", key)
	}
	if !declares(t, jsonFields(t, connector.SnapshotDescriptor{}), key) {
		t.Errorf("connector.SnapshotDescriptor (and so MCP) does not carry %q", key)
	}
	for _, src := range []struct{ path, start string }{
		{"../../../adapters/ts/src/types.ts", "export interface SnapshotCreateResponse {"},
		{"../../../web/src/api.ts", "export interface SnapshotCreateResponse {"},
	} {
		if !declaredNames(tsFieldRe, sourceBlock(t, src.path, src.start, "\n}"))[key] {
			t.Errorf("%s %q does not declare %q", src.path, src.start, key)
		}
	}
	if !declaredNames(protoFieldRe, sourceBlock(t, "../../../proto/loomcycle.proto", "message SnapshotDescriptor {", "\n}"))[key] {
		t.Errorf("proto SnapshotDescriptor does not declare %q", key)
	}
	py := sourceBlock(t, "../../../adapters/python/loomcycle/client.py", "def _snapshot_descriptor_to_dict(", "\ndef ")
	if !strings.Contains(py, `"`+key+`":`) {
		t.Errorf("the Python _snapshot_descriptor_to_dict does not map %q", key)
	}
}
