package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// noise is a reader of n pseudo-random bytes, made as they are read, so a
// test can send a large file without holding it.
type noise struct {
	r    *rand.ChaCha8
	left int64
}

func newNoise(seed byte, n int64) *noise {
	var s [32]byte
	s[0] = seed
	return &noise{r: rand.NewChaCha8(s), left: n}
}

func (z *noise) Read(p []byte) (int, error) {
	if z.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.left {
		p = p[:z.left]
	}
	_, _ = z.r.Read(p)
	z.left -= int64(len(p))
	return len(p), nil
}

func digest(r io.Reader) string {
	h := sha256.New()
	_, _ = io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil))
}

func classOf(err error) string { return dispatch.ClassOf(err) }

// workEntries is what the worker's work folder of a workspace holds: put's
// temporary files and stages, which must be gone after any put ends.
func workEntries(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(f.h.Dir(id), ".loomcycle-work"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// A 100 MiB file goes in with put and out with get, byte for byte, while the
// process's heap (the core and the worker both run in it) grows by a few
// MiB, not by the file: neither end holds it.
func TestPutGet_A100MiBFileDoesNotGrowTheHeapByItsSize(t *testing.T) {
	const size = 100 << 20
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "big")
	ctx := context.Background()

	// A low GC target keeps garbage (the chunks already sent) from reading
	// as growth; what is left is what the two ends hold at once.
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	ans, err := f.m.PutFile(ctx, o, "big", workspace.PutArgs{Path: "data/big.bin", Size: size}, newNoise(1, size))
	if err != nil {
		t.Fatal(err)
	}
	putPeak := peak.Swap(0)
	h := sha256.New()
	n, err := f.m.GetFile(ctx, o, "big", workspace.GetArgs{Path: "data/big.bin"}, h, 0)
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	getPeak := peak.Load()
	want := digest(newNoise(1, size))
	if n != size || hex.EncodeToString(h.Sum(nil)) != want || ans.SHA256 != want || ans.Size != size {
		t.Fatalf("get returned %d bytes, digest %x; put said %+v; want %d bytes, %s", n, h.Sum(nil), ans, size, want)
	}
	if st, err := os.Stat(filepath.Join(f.h.Dir(r.ID), "data", "big.bin")); err != nil || st.Size() != size {
		t.Fatalf("on the worker's disk: %v, %v", st, err)
	}
	growth := func(p uint64) float64 { return float64(int64(p)-int64(base.HeapAlloc)) / (1 << 20) }
	t.Logf("heap before %.1f MiB; growth at peak: put %.1f MiB, get %.1f MiB (file %d MiB)",
		float64(base.HeapAlloc)/(1<<20), growth(putPeak), growth(getPeak), size>>20)
	const bound = 16 // MiB: a few chunk windows and gRPC's buffers, far under the file
	if growth(putPeak) > bound || growth(getPeak) > bound {
		t.Errorf("the heap grew by %.1f MiB (put) and %.1f MiB (get), over %d MiB", growth(putPeak), growth(getPeak), bound)
	}
}

// Content of every byte value, of a size that is no multiple of the chunk,
// comes back exactly; a ranged get returns exactly its range; fstat's sha256
// is the content's.
func TestPutGet_BinaryContentRoundTripsAndRangesAreExact(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	f.open(t, o, "w")
	ctx := context.Background()
	body := make([]byte, 3*workspace.ChunkSize+12345)
	for i := range body {
		body[i] = byte(i*7 + i/256)
	}
	copy(body, "\x00\xff\r\n\"\\{}\xc3\x28") // NUL, invalid UTF-8, JSON's specials
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "b.bin", Size: int64(len(body))}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: "b.bin"}, &got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatal("the content did not round-trip byte for byte")
	}
	for _, c := range []struct{ off, n int64 }{{0, 1}, {1, 10}, {workspace.ChunkSize - 3, 7}, {int64(len(body)) - 5, 5}, {int64(len(body)) - 5, 100}, {100, 0}, {int64(len(body)), 0}} {
		var part bytes.Buffer
		n, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: "b.bin", Offset: c.off, Length: c.n}, &part, 0)
		end := int64(len(body))
		if c.n > 0 {
			end = min(c.off+c.n, end)
		}
		if err != nil || n != end-c.off || !bytes.Equal(part.Bytes(), body[c.off:end]) {
			t.Errorf("get offset %d length %d: %d bytes, %v; want body[%d:%d]", c.off, c.n, n, err, c.off, end)
		}
	}
	if _, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: "b.bin", Offset: int64(len(body)) + 1}, io.Discard, 0); classOf(err) != workspace.ClassInvalid {
		t.Errorf("an offset past the end = %v, want invalid", err)
	}
	if _, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: "b.bin"}, io.Discard, 1000); classOf(err) != workspace.ClassOutputTooLarge {
		t.Errorf("a get over the caller's bound = %v, want output_too_large", err)
	}
	st, err := f.m.Stat(ctx, o, "w", workspace.FStatArgs{Path: "b.bin", SHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA256 != digest(bytes.NewReader(body)) || st.Size != int64(len(body)) || st.Type != workspace.EntryFile {
		t.Errorf("fstat = %+v", st)
	}
	if _, err := f.m.Stat(ctx, o, "w", workspace.FStatArgs{Path: "nope"}); classOf(err) != workspace.ClassPathNotFound {
		t.Errorf("fstat of a missing file = %v, want path_not_found", err)
	}
}

// A put whose reader holds fewer or more bytes than declared is refused
// with size_mismatch and leaves nothing: neither the file nor a temporary
// one. The core checks (Stream) and so does the worker (a core that sent
// the wrong count anyway, through Do).
func TestPutFile_FewerOrMoreBytesThanDeclaredLeavesNothing(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		declared int64
		body     []byte
	}{
		{"fewer", 200000, make([]byte, 199999)},
		{"more", 200000, make([]byte, 200001)},
		{"none", 10, nil},
		{"more than zero", 0, []byte("x")},
	} {
		_, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "f-" + c.name, Size: c.declared}, bytes.NewReader(c.body))
		if classOf(err) != workspace.ClassSizeMismatch {
			t.Errorf("core, %s: err = %v, want size_mismatch", c.name, err)
		}
		// The worker's own check: Do sends whatever the reader holds, then
		// InputEnd.
		raw := []byte(`{"path":"w-` + c.name + `","size":` + itoa(c.declared) + `}`)
		_, err = f.h.D.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpPut, Args: raw, Input: bytes.NewReader(c.body)})
		if classOf(err) != workspace.ClassSizeMismatch {
			t.Errorf("worker, %s: err = %v, want size_mismatch", c.name, err)
		}
	}
	waitFor(t, "the temporary files to go", func() bool { return len(workEntries(t, f, r.ID)) == 0 })
	if ents, _ := os.ReadDir(f.h.Dir(r.ID)); len(ents) > 1 || (len(ents) == 1 && ents[0].Name() != ".loomcycle-work") {
		t.Errorf("the workspace holds %v after refused puts", ents)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A put that would pass the quota is refused before a byte is written,
// leaves no file, and holds nothing back: a put that fits goes through
// afterwards. Overwriting needs overwrite; the bytes it replaces are freed.
func TestPutFile_OverTheQuotaLeavesNoFileAndFreesItsReservation(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, DiskQuotaBytes: 1 << 20}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	put := func(path string, n int64, overwrite bool) (workspace.PutAnswer, error) {
		return f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: path, Size: n, Overwrite: overwrite}, newNoise(2, n))
	}
	if _, err := put("a", 600<<10, false); err != nil {
		t.Fatal(err)
	}
	if _, err := put("b", 600<<10, false); classOf(err) != workspace.ClassOverQuota {
		t.Fatalf("a put past the quota = %v, want workspace_over_quota", err)
	}
	if _, err := os.Lstat(filepath.Join(f.h.Dir(r.ID), "b")); err == nil {
		t.Error("the refused put left its file")
	}
	ans, err := put("c", 400<<10, false)
	if err != nil {
		t.Fatalf("a put that fits after a refused one: %v", err)
	}
	if ans.UsedBytes != 1000<<10 || ans.QuotaBytes != 1<<20 || ans.Estimate {
		t.Errorf("usage after two puts = %+v", ans.Usage)
	}
	if _, err := put("a", 10, false); classOf(err) != workspace.ClassExists {
		t.Errorf("a put onto a file without overwrite = %v, want exists", err)
	}
	ans, err = put("a", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if ans.UsedBytes != 400<<10+10 {
		t.Errorf("used after overwriting 600 KiB with 10 bytes = %d", ans.UsedBytes)
	}
	if got := workEntries(t, f, r.ID); len(got) != 0 {
		t.Errorf("the work folder holds %v", got)
	}
}

// A folder built in a stage is not in the workspace until commit, which
// places it in one rename; discard leaves nothing; commit onto a folder
// there needs overwrite, which merges.
func TestStage_AFolderAppearsOnlyAtCommitAndDiscardLeavesNothing(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	stage, err := f.m.Stage(ctx, o, "w")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"x/1.txt", "x/sub/2.txt"} {
		if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: p, Size: 3, Stage: stage}, strings.NewReader("abc")); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.m.Mkdir(ctx, o, "w", workspace.MkdirArgs{Path: "x/empty", Stage: stage}); err != nil {
		t.Fatal(err)
	}
	if l, err := f.m.ListFiles(ctx, o, "w", workspace.FListArgs{Recursive: true}); err != nil || len(l.Entries) != 0 {
		t.Fatalf("before commit the workspace lists %+v, %v", l, err)
	}
	if _, err := f.m.Commit(ctx, o, "w", workspace.CommitArgs{Stage: stage, Path: "out"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"out/x/1.txt", "out/x/sub/2.txt", "out/x/empty"} {
		if _, err := os.Stat(filepath.Join(f.h.Dir(r.ID), p)); err != nil {
			t.Errorf("after commit: %v", err)
		}
	}
	// Onto a folder already there: refused, then merged with overwrite.
	s2, _ := f.m.Stage(ctx, o, "w")
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "x/1.txt", Size: 1, Stage: s2}, strings.NewReader("Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Commit(ctx, o, "w", workspace.CommitArgs{Stage: s2, Path: "out"}); classOf(err) != workspace.ClassExists {
		t.Errorf("commit onto a folder without overwrite = %v, want exists", err)
	}
	s3, _ := f.m.Stage(ctx, o, "w")
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "x/1.txt", Size: 1, Stage: s3}, strings.NewReader("Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Commit(ctx, o, "w", workspace.CommitArgs{Stage: s3, Path: "out", Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.h.Dir(r.ID), "out/x/1.txt")); string(b) != "Z" {
		t.Errorf("the merged file = %q", b)
	}
	if _, err := os.Stat(filepath.Join(f.h.Dir(r.ID), "out/x/sub/2.txt")); err != nil {
		t.Errorf("the merge removed a file it did not replace: %v", err)
	}
	// Discard: the stage and what it held are gone.
	s4, _ := f.m.Stage(ctx, o, "w")
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "y", Size: 1, Stage: s4}, strings.NewReader("y")); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Discard(ctx, o, "w", s4); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Discard(ctx, o, "w", s4); err != nil {
		t.Errorf("discarding a stage already gone: %v", err)
	}
	if _, err := f.m.Commit(ctx, o, "w", workspace.CommitArgs{Stage: s4, Path: "y"}); classOf(err) != workspace.ClassPathNotFound {
		t.Errorf("committing a discarded stage = %v, want path_not_found", err)
	}
	if got := workEntries(t, f, r.ID); len(got) != 0 {
		t.Errorf("the work folder still holds %v", got)
	}
	if err := f.m.Discard(ctx, o, "w", "../../x"); classOf(err) != workspace.ClassInvalid {
		t.Errorf("a malformed stage id = %v, want invalid", err)
	}
}

// copy_local copies a file and a folder between two workspaces of the run
// on one worker; links in a folder are skipped and named; a copy over the
// target's quota is refused with nothing written.
func TestCopyLocal_CopiesFilesAndFoldersWithinTheTargetsQuota(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, DiskQuotaBytes: 64 << 10}, worker.Options{})
	o := f.run(t, "alice")
	src := f.open(t, o, "src")
	dst := f.open(t, o, "dst")
	ctx := context.Background()
	sd := f.h.Dir(src.ID)
	for p, body := range map[string]string{"one.txt": "1", "dir/a.txt": "aa", "dir/deep/b.txt": "bbb"} {
		_ = os.MkdirAll(filepath.Join(sd, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(sd, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(sd, "dir", "pw")); err != nil {
		t.Fatal(err)
	}
	ans, err := f.m.CopyLocal(ctx, o, CopyLocalRequest{From: "src", FromPath: "one.txt", To: "dst", ToPath: "copied/one.txt"})
	if err != nil || ans.Files != 1 || ans.Bytes != 1 {
		t.Fatalf("file copy = %+v, %v", ans, err)
	}
	ans, err = f.m.CopyLocal(ctx, o, CopyLocalRequest{From: "src", FromPath: "dir", To: "dst", ToPath: "copied/dir"})
	if err != nil || ans.Files != 2 || ans.Bytes != 5 || len(ans.Skipped) != 1 || ans.Skipped[0] != "pw" {
		t.Fatalf("folder copy = %+v, %v", ans, err)
	}
	dd := f.h.Dir(dst.ID)
	for p, want := range map[string]string{"copied/one.txt": "1", "copied/dir/a.txt": "aa", "copied/dir/deep/b.txt": "bbb"} {
		if b, err := os.ReadFile(filepath.Join(dd, p)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", p, b, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dd, "copied/dir/pw")); err == nil {
		t.Error("a link was copied")
	}
	// Within one workspace too.
	if _, err := f.m.CopyLocal(ctx, o, CopyLocalRequest{From: "src", FromPath: "dir", To: "src", ToPath: "dir2"}); err != nil {
		t.Fatal(err)
	}
	// Over the target's quota: refused, nothing written.
	if err := os.WriteFile(filepath.Join(sd, "big"), make([]byte, 70<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.CopyLocal(ctx, o, CopyLocalRequest{From: "src", FromPath: "big", To: "dst", ToPath: "big"}); classOf(err) != workspace.ClassOverQuota {
		t.Errorf("a copy over the quota = %v, want workspace_over_quota", err)
	}
	if _, err := os.Lstat(filepath.Join(dd, "big")); err == nil {
		t.Error("the refused copy left its file")
	}
	if got := workEntries(t, f, dst.ID); len(got) != 0 {
		t.Errorf("the target's work folder holds %v", got)
	}
	// Onto a file there: refused without overwrite.
	if _, err := f.m.CopyLocal(ctx, o, CopyLocalRequest{From: "src", FromPath: "one.txt", To: "dst", ToPath: "copied/one.txt"}); classOf(err) != workspace.ClassExists {
		t.Errorf("a copy onto a file = %v, want exists", err)
	}
	// Another run cannot name these workspaces.
	other := f.run(t, "bob")
	if _, err := f.m.CopyLocal(ctx, other, CopyLocalRequest{From: "src", FromPath: "one.txt", To: "src", ToPath: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another run's copy = %v, want not found", err)
	}
}

// Every new op through the Manager reaches only the run's own workspace,
// and get and put, which the worker serves on the streaming path, refuse
// `..`, absolute paths and links there too.
func TestFileOps_OtherRunsGetNotFoundAndStreamOpsRefuseEscapingPaths(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	ctx := context.Background()
	dir := f.h.Dir(r.ID)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "lf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "ld")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../x", "a/../../x", outside, filepath.Join(dir, "f"), "lf", "ld/secret", "ld/new", ".loomcycle-work/tmp/x"} {
		var buf bytes.Buffer
		if _, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: p}, &buf, 0); err == nil || strings.Contains(buf.String(), "secret") {
			t.Errorf("get %q = %q, %v; want a refusal", p, buf.String(), err)
		}
		// A put onto the link lf replaces the link itself, inside the
		// workspace, as the Write tool does; every other one is refused.
		if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: p, Size: 1, Overwrite: true}, strings.NewReader("X")); err == nil && p != "lf" {
			t.Errorf("put %q was not refused", p)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "lf")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("a put onto a link did not replace the link: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "secret" {
		t.Errorf("the file outside changed: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(outside), "new")); err == nil {
		t.Error("a put through a link wrote outside")
	}
	other := f.run(t, "bob")
	if _, err := f.m.Stat(ctx, other, "w", workspace.FStatArgs{Path: "."}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another run's fstat = %v, want not found", err)
	}
	if _, err := f.m.PutFile(ctx, other, "w", workspace.PutArgs{Path: "x", Size: 1}, strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("another run's put = %v, want not found", err)
	}
	if _, err := f.m.RemoveFile(ctx, o, "w", "."); classOf(err) != workspace.ClassInvalid {
		t.Errorf("removing the root = %v, want invalid", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the workspace is gone: %v", err)
	}
}

// A worker that stops in the middle of a put leaves no file: the put's
// temporary file is removed when the stream ends, and whatever a killed
// worker left in the work folder is removed when it starts again.
func TestPutFile_AWorkerStoppedMidPutLeavesNoPartialFileAfterRestart(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	dir := f.h.Dir(r.ID)
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, err := f.m.PutFile(context.Background(), o, "w", workspace.PutArgs{Path: "half", Size: 10 << 20}, pr)
		errc <- err
	}()
	if _, err := pw.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the temporary file", func() bool { return len(workEntries(t, f, r.ID)) == 1 })
	// What a killed worker leaves: a temporary file and a stage.
	for _, p := range []string{".loomcycle-work/tmp/put-killed", ".loomcycle-work/stage/0123456789abcdef0123456789abcdef/f"} {
		_ = os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.h.StopWorker()
	_ = pw.CloseWithError(errors.New("the test stopped sending"))
	if err := <-errc; err == nil {
		t.Fatal("a put whose worker stopped succeeded")
	}
	f.h.StartWorker()
	if _, err := os.Lstat(filepath.Join(dir, "half")); err == nil {
		t.Error("the stopped put left its file")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".loomcycle-work")); err == nil {
		t.Errorf("the work folder survived the restart: %v", workEntries(t, f, r.ID))
	}
}

// Ruling Q2: a command may write past the quota and succeed; the workspace
// is then marked full, so the next put and the next command are refused
// with workspace_over_quota while reads still work, until a remove brings
// it back under and both work again.
func TestQuota_ACommandPastTheQuotaMarksTheWorkspaceFullUntilARemove(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, DiskQuotaBytes: 1 << 20}, worker.Options{})
	o := f.run(t, "alice")
	f.open(t, o, "w")
	ctx := context.Background()
	exec := func(cmd string) (string, error) {
		r, err := f.m.Get(ctx, o, "w")
		if err != nil {
			return "", err
		}
		res, err := f.m.Do(ctx, r, dispatch.Request{Op: workspace.OpExec, Args: []byte(`{"command":"` + cmd + `","timeout_ms":10000}`)})
		return string(res.Output), err
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "small", Size: 5}, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec("head -c 2000000 /dev/zero > big"); err != nil {
		t.Fatalf("the command that writes past the quota failed: %s %v", out, err)
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "more", Size: 1}, strings.NewReader("x")); classOf(err) != workspace.ClassOverQuota {
		t.Errorf("a put into a full workspace = %v, want workspace_over_quota", err)
	}
	if _, err := exec("echo hi"); classOf(err) != workspace.ClassOverQuota {
		t.Errorf("a command in a full workspace = %v, want workspace_over_quota", err)
	}
	if err := f.m.Mkdir(ctx, o, "w", workspace.MkdirArgs{Path: "d"}); classOf(err) != workspace.ClassOverQuota {
		t.Errorf("a mkdir in a full workspace = %v, want workspace_over_quota", err)
	}
	var got bytes.Buffer
	if _, err := f.m.GetFile(ctx, o, "w", workspace.GetArgs{Path: "small"}, &got, 0); err != nil || got.String() != "hello" {
		t.Errorf("a get from a full workspace = %q, %v", got.String(), err)
	}
	if _, err := f.m.Stat(ctx, o, "w", workspace.FStatArgs{Path: "big"}); err != nil {
		t.Errorf("an fstat in a full workspace: %v", err)
	}
	rm, err := f.m.RemoveFile(ctx, o, "w", "big")
	if err != nil {
		t.Fatal(err)
	}
	if rm.Bytes != 2000000 || rm.UsedBytes != 5 {
		t.Errorf("remove = %+v", rm)
	}
	if _, err := f.m.PutFile(ctx, o, "w", workspace.PutArgs{Path: "more", Size: 1}, strings.NewReader("x")); err != nil {
		t.Errorf("a put after the remove: %v", err)
	}
	if out, err := exec("echo hi"); err != nil || !strings.Contains(out, "hi") {
		t.Errorf("a command after the remove = %s, %v", out, err)
	}
}

// A count whose walk ran out of time is an estimate, and says so.
func TestQuota_AWalkOutOfTimeIsAnEstimate(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{MeasureBudget: time.Nanosecond})
	o := f.run(t, "alice")
	r := f.open(t, o, "w")
	for i := range 50 {
		if err := os.WriteFile(filepath.Join(f.h.Dir(r.ID), "f"+itoa(int64(i))), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ans, err := f.m.PutFile(context.Background(), o, "w", workspace.PutArgs{Path: "x", Size: 1}, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Estimate {
		t.Errorf("usage = %+v, want an estimate", ans.Usage)
	}
}
