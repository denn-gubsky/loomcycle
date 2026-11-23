package lifecycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func unsized(path string, maxBytes int64) workspace.PutArgs {
	return workspace.PutArgs{Path: path, Size: workspace.PutSizeUnknown, MaxBytes: maxBytes}
}

// A put of unknown length lands byte for byte, answers its real size, and
// the quota counts that size, not the MaxBytes it reserved: a known-size put
// that fits only once the unused reservation is released goes through.
func TestPutFileUnsized_LandsAndTheQuotaCountsTheRealSize(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, DiskQuotaBytes: 1 << 20}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	const n = 300 << 10
	ans, err := f.m.PutFile(ctx, o, "w", unsized("in/a.bin", 1<<20), newNoise(7, n))
	if err != nil {
		t.Fatal(err)
	}
	if ans.Size != n || ans.UsedBytes != n || ans.SHA256 != digest(newNoise(7, n)) {
		t.Errorf("answer = %+v, want size and used %d", ans, n)
	}
	b, err := os.ReadFile(filepath.Join(f.h.Dir(r.ID), "in", "a.bin"))
	if err != nil || int64(len(b)) != n || digest(bytes.NewReader(b)) != ans.SHA256 {
		t.Fatalf("the workspace holds %d bytes (%v)", len(b), err)
	}
	ans, err = f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "b", Size: 1<<20 - n}, newNoise(8, 1<<20-n))
	if err != nil {
		t.Fatalf("a put of the rest of the quota after an unsized put: %v", err)
	}
	if ans.UsedBytes != 1<<20 {
		t.Errorf("used = %d, want the whole quota", ans.UsedBytes)
	}
	if got := workEntries(t, f, r.ID); len(got) != 0 {
		t.Errorf("the work folder holds %v", got)
	}
}

// An input past MaxBytes is refused and leaves nothing, on the core's check
// (Stream) and on the worker's own (Do sends whatever the reader holds), and
// the reservation is released: a put of the whole quota goes through after.
func TestPutFileUnsized_PastMaxBytesLeavesNothingAndFreesTheReservation(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, DiskQuotaBytes: 1 << 20}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	const max = 200 << 10
	if _, err := f.m.PutFile(ctx, o, "w", unsized("core", max), newNoise(1, max+1)); classOf(err) != workspace.ClassSizeMismatch {
		t.Errorf("core: err = %v, want size_mismatch", err)
	}
	raw := []byte(`{"path":"worker","size":-1,"max_bytes":` + itoa(max) + `}`)
	_, err := f.h.D.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpPut, Args: raw, Input: newNoise(1, max+1)})
	if classOf(err) != workspace.ClassSizeMismatch {
		t.Errorf("worker: err = %v, want size_mismatch", err)
	}
	waitFor(t, "the temporary files to go", func() bool { return len(workEntries(t, f, r.ID)) == 0 })
	if ents, _ := os.ReadDir(f.h.Dir(r.ID)); len(ents) > 1 || (len(ents) == 1 && ents[0].Name() != ".loomcycle-work") {
		t.Errorf("the workspace holds %v after refused puts", ents)
	}
	ans, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "all", Size: 1 << 20}, newNoise(2, 1<<20))
	if err != nil || ans.UsedBytes != 1<<20 {
		t.Fatalf("a put of the whole quota after refused unsized puts: %+v %v", ans, err)
	}
}

// A put whose MaxBytes the quota cannot hold is refused before a byte of
// its input is read: the worker admits (or refuses) it on OpStart and the
// core reads nothing until it has been admitted. A refused target (a file
// already there) reads nothing either.
func TestPutFileUnsized_RefusedBeforeReadingAByte(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, DiskQuotaBytes: 1 << 20}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	in := &countingReader{r: newNoise(3, 10)}
	if _, err := f.m.PutFile(ctx, o, "w", unsized("big", 1<<20+1), in); classOf(err) != workspace.ClassOverQuota {
		t.Fatalf("err = %v, want workspace_over_quota", err)
	}
	if in.n != 0 {
		t.Errorf("the core read %d bytes of a refused put's input", in.n)
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "x", Size: 1}, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	in = &countingReader{r: newNoise(3, 10)}
	if _, err := f.m.PutFile(ctx, o, "w", unsized("x", 100), in); classOf(err) != workspace.ClassExists || in.n != 0 {
		t.Errorf("onto a file: err = %v, %d bytes read", err, in.n)
	}
	if got := workEntries(t, f, r.ID); len(got) != 0 {
		t.Errorf("the work folder holds %v", got)
	}
}

// The worker refuses a malformed unknown-size put: no MaxBytes, MaxBytes
// with a known size, or another negative size.
func TestPutFileUnsized_MalformedArgumentsAreRefused(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	for _, raw := range []string{
		`{"path":"a","size":-1}`,
		`{"path":"a","size":3,"max_bytes":10}`,
		`{"path":"a","size":-2,"max_bytes":10}`,
	} {
		_, err := f.h.D.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpPut, Args: []byte(raw), Input: bytes.NewReader([]byte("abc"))})
		if classOf(err) != workspace.ClassInvalid {
			t.Errorf("%s: err = %v, want invalid", raw, err)
		}
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "a", Size: workspace.PutSizeUnknown}, bytes.NewReader([]byte("abc"))); classOf(err) != workspace.ClassInvalid {
		t.Errorf("core without max_bytes: err = %v, want invalid", err)
	}
}
