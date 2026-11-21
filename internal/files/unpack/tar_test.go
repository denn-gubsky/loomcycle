package unpack

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing"
)

func opts() Options {
	return Options{Limits: DefaultLimits, PerFile: 1 << 20, Budget: DefaultLimits.Total}
}

// replayInto replays a tar into a map of path to bytes.
func replayInto(t *testing.T, tr io.Reader) (string, *Result, map[string]string, error) {
	t.Helper()
	got := map[string]string{}
	p := &Replayer{PerFile: 1 << 20, MaxWritten: DefaultLimits.Written,
		Planned: func(segs []string) string { return "to/" + path.Join(segs...) },
		Write: func(e Entry, head []byte, body io.Reader) (Written, string, error) {
			rest, err := io.ReadAll(body)
			if err != nil {
				return Written{}, "", err
			}
			rel := path.Join(e.Segs...)
			got[rel] = string(head) + string(rest)
			return Written{Ref: "to/" + rel, Name: e.Display, Size: e.Size}, "", nil
		}}
	r := tar.NewReader(tr)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, got, err
		}
		if err := p.Entry(h, r); err != nil {
			return "", nil, got, err
		}
	}
	kind, res, err := p.Result()
	return kind, res, got, err
}

// What a walk away from its destination sends is what the same walk in
// place writes: the files, the skips (a climbing, an absolute and a link
// entry, as File op=unpack skips them) and the counts.
func TestWriteTar_ReplaysAsTheWalkInPlaceWrites(t *testing.T) {
	arc := zipArchive(t,
		zipEnt{name: "books/a.txt", body: text},
		zipEnt{name: "../evil.txt", body: text},
		zipEnt{name: "/abs.txt", body: text},
		zipEnt{name: "link.txt", body: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777},
		zipEnt{name: "b.txt", body: []byte("second")},
	)
	var buf bytes.Buffer
	if err := WriteTar(context.Background(), bytes.NewReader(arc), int64(len(arc)), opts(), t.TempDir(), &buf); err != nil {
		t.Fatal(err)
	}
	kind, res, got, err := replayInto(t, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "zip" || got["books/a.txt"] != string(text) || got["b.txt"] != "second" || len(got) != 2 {
		t.Fatalf("kind %q, files %v", kind, got)
	}
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Name] = s.Reason
	}
	if !strings.Contains(reasons["../evil.txt"], "climbs") || reasons["/abs.txt"] != "an absolute path" || reasons["link.txt"] != "a symbolic link" {
		t.Errorf("skipped %v", res.Skipped)
	}
	if res.Counts.Written != 2 || res.Counts.Listed != 5 || res.Counts.Skipped != 3 || len(res.Written) != 2 || res.Written[0].Ref != "to/books/a.txt" {
		t.Errorf("result %+v", res)
	}
}

// A dry run sends no file; the planned paths come back as the receiver
// would name them.
func TestWriteTar_DryRunPlansThroughTheReceiver(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.txt", body: text})
	o := opts()
	o.DryRun = true
	var buf bytes.Buffer
	if err := WriteTar(context.Background(), bytes.NewReader(arc), int64(len(arc)), o, t.TempDir(), &buf); err != nil {
		t.Fatal(err)
	}
	_, res, got, err := replayInto(t, &buf)
	if err != nil || len(got) != 0 || !res.DryRun || len(res.Written) != 1 || res.Written[0].Ref != "to/a.txt" {
		t.Fatalf("err %v, files %v, result %+v", err, got, res)
	}
}

// Not an archive is refused before anything is sent.
func TestWriteTar_NotAnArchiveIsRefusedWithNothingSent(t *testing.T) {
	var buf bytes.Buffer
	data := []byte("\x1f\x8b\x08\x00 a gzip file")
	err := WriteTar(context.Background(), bytes.NewReader(data), int64(len(data)), opts(), t.TempDir(), &buf)
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "gzip") || buf.Len() != 0 {
		t.Fatalf("err %v, %d bytes sent", err, buf.Len())
	}
}

// A sender that is not the walk cannot place a file outside the receiver's
// folder or past its bounds: the replay refuses the entry before Write.
func TestReplayer_RefusesAnEntryTheWalkWouldNotMake(t *testing.T) {
	for name, ent := range map[string]struct {
		name string
		size int
	}{
		"climbing":  {"entry/../x.txt", 1},
		"absolute":  {"entry//etc/x", 1},
		"hidden":    {"entry/.ssh/x", 1},
		"unclean":   {"entry/a//b.txt", 1},
		"too large": {"entry/big.txt", 2 << 20},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = writeTarBytes(tw, tarFormat, []byte("zip"), nil)
		_ = writeTarBytes(tw, ent.name, make([]byte, ent.size), nil)
		_ = tw.Close()
		_, _, got, err := replayInto(t, &buf)
		if err == nil || len(got) != 0 {
			t.Errorf("%s: err %v, written %v", name, err, got)
		}
	}
}

// A write that fails stops the replay; what was written before it is the
// result, with the stop named.
func TestReplayer_AFailedWriteStopsWithWhatWasWritten(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.txt", body: text}, zipEnt{name: "b.txt", body: text}, zipEnt{name: "c.txt", body: text})
	var buf bytes.Buffer
	if err := WriteTar(context.Background(), bytes.NewReader(arc), int64(len(arc)), opts(), t.TempDir(), &buf); err != nil {
		t.Fatal(err)
	}
	n := 0
	p := &Replayer{PerFile: 1 << 20, Write: func(e Entry, head []byte, body io.Reader) (Written, string, error) {
		n++
		if n == 2 {
			return Written{}, "", errors.New("the inbox is full")
		}
		return Written{Ref: path.Join(e.Segs...), Size: e.Size}, "", nil
	}}
	r := tar.NewReader(&buf)
	var stopErr error
	for stopErr == nil {
		h, err := r.Next()
		if err != nil {
			break
		}
		stopErr = p.Entry(h, r)
	}
	_, res, err := p.Result()
	if !errors.Is(stopErr, ErrStopped) || err != nil || len(res.Written) != 1 || !strings.Contains(res.Stopped, "the inbox is full") {
		t.Fatalf("stop %v, err %v, result %+v", stopErr, err, res)
	}
}
