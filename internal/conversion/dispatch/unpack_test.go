package dispatch_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/unpack"
)

func unpackZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// replayed is what an unpack tar holds once replayed: each file's bytes by
// path, and the result.
func replayed(t *testing.T, tarBytes []byte) (map[string]string, *unpack.Result) {
	t.Helper()
	files := map[string]string{}
	p := &unpack.Replayer{PerFile: 1 << 20, MaxWritten: unpack.DefaultLimits.Written,
		Planned: func(segs []string) string { return path.Join(segs...) },
		Write: func(e unpack.Entry, head []byte, body io.Reader) (unpack.Written, string, error) {
			rest, err := io.ReadAll(body)
			files[path.Join(e.Segs...)] = string(head) + string(rest)
			return unpack.Written{Ref: path.Join(e.Segs...), Size: e.Size}, "", err
		}}
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Entry(h, tr); err != nil {
			t.Fatal(err)
		}
	}
	_, res, err := p.Result()
	if err != nil {
		t.Fatal(err)
	}
	return files, res
}

func unpackOptions() unpack.Options {
	return unpack.Options{Limits: unpack.DefaultLimits, PerFile: 1 << 20, Budget: unpack.DefaultLimits.Total}
}

// An archive unpacked on a worker gives the files and the result the
// in-process client gives, its tar streamed through.
func TestPool_UnpackMatchesInProcess(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	realWorker(t, target, "w1")
	waitIdle(t, d, 1)
	arc := unpackZip(t, map[string]string{"books/a.txt": "first file\n", "b.txt": "second file\n", "../out.txt": "climbs\n"})
	ctx := context.Background()
	var here, there bytes.Buffer
	if err := (&convert.InProcess{}).Unpack(ctx, bytes.NewReader(arc), int64(len(arc)), unpackOptions(), &here); err != nil {
		t.Fatal(err)
	}
	if err := d.Client().Unpack(ctx, bytes.NewReader(arc), int64(len(arc)), unpackOptions(), &there); err != nil {
		t.Fatal(err)
	}
	wantFiles, wantRes := replayed(t, here.Bytes())
	gotFiles, gotRes := replayed(t, there.Bytes())
	if len(gotFiles) != 2 || gotFiles["books/a.txt"] != "first file\n" || len(wantFiles) != 2 {
		t.Fatalf("files on the worker %v, in process %v", gotFiles, wantFiles)
	}
	if gotRes.Counts != wantRes.Counts || len(gotRes.Skipped) != 1 || gotRes.Skipped[0] != wantRes.Skipped[0] {
		t.Errorf("result on the worker %+v, in process %+v", gotRes, wantRes)
	}
}

// An archive refused whole comes back as the walk's own refusal, with its
// reason, not as a crash.
func TestPool_UnpackRefusalKeepsItsReason(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	realWorker(t, target, "w1")
	waitIdle(t, d, 1)
	data := []byte("\x1f\x8b\x08\x00 not a zip at all")
	var out bytes.Buffer
	err := d.Client().Unpack(context.Background(), bytes.NewReader(data), int64(len(data)), unpackOptions(), &out)
	var r *unpack.Refusal
	if !errors.As(err, &r) || r.Reason != "is not a ZIP or RAR archive (a gzip file)" || out.Len() != 0 {
		t.Fatalf("err %v, %d bytes out", err, out.Len())
	}
	if d.Stats().Quarantined != 0 {
		t.Error("a refused archive was quarantined")
	}
}

// A worker built before the unpack job carries no unpack label: it is never
// offered one, and with only such workers attached the job is refused at
// once rather than queued.
func TestPool_UnpackIsNotOfferedToAnOlderWorker(t *testing.T) {
	d, target := pool(t, dispatch.Options{QueueWait: time.Minute})
	s := attachFake(t, target, "old", wire.ProtocolVersion).openSlot(t)
	defer s.cancel()
	waitIdle(t, d, 1)
	arc := unpackZip(t, map[string]string{"a.txt": "x"})
	start := time.Now()
	err := d.Client().Unpack(context.Background(), bytes.NewReader(arc), int64(len(arc)), unpackOptions(), io.Discard)
	if !errors.Is(err, convert.ErrNoWorkers) {
		t.Fatalf("err = %v, want ErrNoWorkers", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the refusal waited %v; it must not queue", time.Since(start))
	}
	if st := d.Stats(); st.Idle != 1 || st.Jobs["archive_unpack/no_worker"] != 1 {
		t.Errorf("stats %+v: the older worker's slot must stay idle", st)
	}
}
