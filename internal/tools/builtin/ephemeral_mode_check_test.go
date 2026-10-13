package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// modeCheck is a stand-in for the stored record: it answers what it is told
// to, and counts how often it was asked.
type modeCheck struct {
	readOnly bool
	err      error
	asked    int
}

func (m *modeCheck) check(_ context.Context, _ string) (bool, error) {
	m.asked++
	return m.readOnly, m.err
}

// ephemeralWithCheck builds a ctx whose in-memory set holds "scratch" as
// WRITABLE, a default binding beside it, and the given record check.
func ephemeralWithCheck(t *testing.T, m *modeCheck) (context.Context, *tools.EphemeralVolumeSet, string) {
	t.Helper()
	eph, bound := realDir(t), realDir(t)
	set := tools.NewEphemeralVolumeSet()
	set.Add("scratch", tools.EphemeralVolumeRef{Root: eph})
	ctx := tools.WithEphemeralVolumes(ctxWith(tools.VolumeBinding{Name: "default", Root: bound, Default: true}), set)
	return tools.WithEphemeralModeCheck(ctx, m.check), set, eph
}

func writeTo(t *testing.T, ctx context.Context, volume string) tools.Result {
	t.Helper()
	return fileCall(t, ctx, &Write{}, map[string]any{"volume": volume, "path": "a.txt", "content": "x"})
}

// The set in this process says writable; the record says read-only, as it
// does after a set_mode made by a run of the tree in another process. The
// write is refused, and the set learns it.
func TestEphemeralWrite_RecordSaysReadOnly_IsRefusedAndRemembered(t *testing.T) {
	m := &modeCheck{readOnly: true}
	ctx, set, dir := ephemeralWithCheck(t, m)

	res := writeTo(t, ctx, "scratch")
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Fatalf("the write was not refused as read-only: error=%v %q", res.IsError, res.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Error("the file was written")
	}
	if ref, _ := set.Get("scratch"); !ref.ReadOnly {
		t.Error("the set still holds the volume as writable")
	}
	// Now the set knows, and the record is not asked again.
	asked := m.asked
	if res := writeTo(t, ctx, "scratch"); !res.IsError {
		t.Error("a second write went through")
	}
	if m.asked != asked {
		t.Errorf("the record was read again for a volume already known read-only (%d reads)", m.asked-asked)
	}
}

// A record that cannot be read is not a yes.
func TestEphemeralWrite_RecordUnreadable_IsRefused(t *testing.T) {
	m := &modeCheck{err: errors.New("store is down")}
	ctx, set, dir := ephemeralWithCheck(t, m)

	res := writeTo(t, ctx, "scratch")
	if !res.IsError || !strings.Contains(res.Text, "could not confirm") || !strings.Contains(res.Text, "store is down") {
		t.Fatalf("error=%v %q, want a refusal that says the mode could not be confirmed", res.IsError, res.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Error("the file was written")
	}
	// The volume is not marked read-only by a failed read: the next call asks again.
	if ref, _ := set.Get("scratch"); ref.ReadOnly {
		t.Error("an unreadable record made the volume read-only for good")
	}
	m.err = nil
	if res := writeTo(t, ctx, "scratch"); res.IsError {
		t.Errorf("once the record can be read and says writable, the write should work: %s", res.Text)
	}
}

// The record is asked only by a write to a writable ephemeral volume.
func TestEphemeralModeCheck_IsAskedOnlyWhenAWriteDependsOnIt(t *testing.T) {
	m := &modeCheck{}
	ctx, _, _ := ephemeralWithCheck(t, m)

	if res := writeTo(t, ctx, "scratch"); res.IsError {
		t.Fatalf("a write the record allows: %s", res.Text)
	}
	if m.asked != 1 {
		t.Fatalf("a write to a writable ephemeral volume read the record %d times, want 1", m.asked)
	}
	m.asked = 0
	for what, res := range map[string]tools.Result{
		"Read":  fileCall(t, ctx, &Read{}, map[string]any{"volume": "scratch", "path": "a.txt"}),
		"Glob":  fileCall(t, ctx, &Glob{}, map[string]any{"volume": "scratch", "pattern": "*"}),
		"Grep":  fileCall(t, ctx, &Grep{}, map[string]any{"volume": "scratch", "pattern": "x"}),
		"Write": writeTo(t, ctx, "default"), // the agent's own binding, not an ephemeral volume
	} {
		if res.IsError {
			t.Errorf("%s: %s", what, res.Text)
		}
	}
	if m.asked != 0 {
		t.Errorf("reads, or a write to a bound volume, read the record %d times", m.asked)
	}
}

// Bashbox mounts a writable volume read-write, so it asks the record too. On
// read-only, or on a record it cannot read, it mounts the overlay: the
// command runs and nothing it writes reaches the volume.
func TestBashbox_EphemeralVolume_RecordDecidesTheMount(t *testing.T) {
	for name, m := range map[string]*modeCheck{
		"record says read-only": {readOnly: true},
		"record unreadable":     {err: errors.New("store is down")},
	} {
		ctx, _, dir := ephemeralWithCheck(t, m)
		res := fileCall(t, ctx, &Bashbox{Enabled: true}, map[string]any{"command": "echo leaked > out.txt && cat out.txt", "volume": "scratch"})
		if res.IsError {
			t.Errorf("%s: the command should still run: %s", name, res.Text)
		}
		if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
			t.Errorf("%s: the write reached the volume", name)
		}
	}
	// Control: when the record says writable, the write does reach it.
	ctx, _, dir := ephemeralWithCheck(t, &modeCheck{})
	if res := fileCall(t, ctx, &Bashbox{Enabled: true}, map[string]any{"command": "echo kept > out.txt", "volume": "scratch"}); res.IsError {
		t.Fatalf("a writable volume: %s", res.Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("a writable volume did not get the write: %v", err)
	}
}
