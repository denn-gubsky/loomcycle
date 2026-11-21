package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// crashOnce is a conversion child that dies on every file holding "CRASH".
type crashOnce struct {
	convert.Client
	n atomic.Int32
}

func (c *crashOnce) calls() int { return int(c.n.Load()) }

func (c *crashOnce) Extract(ctx context.Context, mt string, r io.ReaderAt, size int64, sink extract.Sink, opts extract.Options) (extract.Report, error) {
	c.n.Add(1)
	b, _ := io.ReadAll(io.NewSectionReader(r, 0, size))
	if bytes.Contains(b, []byte("CRASH")) {
		return extract.Report{}, convert.ErrQuarantined
	}
	return c.Client.Extract(ctx, mt, r, size, sink, opts)
}

func convFixture(t *testing.T, l Limits) (*fixture, Owner, *crashOnce) {
	t.Helper()
	cv := &crashOnce{Client: &convert.InProcess{}}
	f := newFixture(t, l, worker.Options{Convert: cv, Labels: []string{wire.LabelExtract}})
	o := f.run(t, "alice")
	return f, o, cv
}

func (f *fixture) put(t *testing.T, o Owner, name, p, content string) workspace.FileInfo {
	t.Helper()
	if _, err := f.m.PutFile(context.Background(), o, name, workspace.PutArgs{Path: p, Size: int64(len(content))}, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	info, err := f.m.Stat(context.Background(), o, name, workspace.FStatArgs{Path: p, SHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func extractArgs(p, sha string) *workspace.ConvertExtractArgs {
	return &workspace.ConvertExtractArgs{ConvertSource: workspace.ConvertSource{Path: p, SHA256: sha}, MaxMarkdown: 1 << 20, To: workspace.ConvertTarget{Path: "out"}}
}

// A source that no longer hashes to the digest the core checked its
// quarantine with is refused on the worker before it is converted: a file
// swapped after the check cannot slip past the quarantine.
func TestConvert_ASourceChangedSinceItsDigestIsRefusedOnTheWorker(t *testing.T) {
	f, o, cv := convFixture(t, Limits{MaxPerRun: 4})
	f.open(t, o, "w")
	info := f.put(t, o, "w", "a.txt", "hello")
	f.put(t, o, "w", "a.txt.new", "CRASH")
	if err := os.Rename(filepath.Join(f.h.Dir(f.rowOf(t, o, "w")), "a.txt.new"), filepath.Join(f.h.Dir(f.rowOf(t, o, "w")), "a.txt")); err != nil {
		t.Fatal(err)
	}
	var ans workspace.ConvertExtractAnswer
	err := f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertExtract, Args: extractArgs("a.txt", info.SHA256), Source: "w", SHA256: info.SHA256, To: "w"}, nil, &ans)
	if classOf(err) != workspace.ClassSourceChanged || cv.calls() != 0 {
		t.Fatalf("err = %v, conversions %d; want source_changed and none", err, cv.calls())
	}
}

func (f *fixture) rowOf(t *testing.T, o Owner, name string) string {
	t.Helper()
	r, err := f.m.Get(context.Background(), o, name)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// A file that crashed the conversion child is refused for its workspace,
// by its digest, until the quarantine time passes; then it is tried again.
func TestConvert_TheQuarantineHoldsForItsTimeAndForItsWorkspace(t *testing.T) {
	f, o, cv := convFixture(t, Limits{MaxPerRun: 4, ConvertQuarantine: time.Minute})
	now := time.Now()
	f.m.now = func() time.Time { return now }
	f.open(t, o, "w")
	info := f.put(t, o, "w", "bad.txt", "CRASH")
	req := func() error {
		return f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertExtract, Args: extractArgs("bad.txt", info.SHA256), Source: "w", SHA256: info.SHA256, To: "w"}, nil, nil)
	}
	if err := req(); !errors.Is(err, ErrQuarantined) || classOf(err) != workspace.ClassConversionCrashed {
		t.Fatalf("first: %v", err)
	}
	if err := req(); !errors.Is(err, ErrQuarantined) || cv.calls() != 1 {
		t.Fatalf("within the time: %v, %d conversions", err, cv.calls())
	}
	now = now.Add(2 * time.Minute)
	if err := req(); cv.calls() != 2 || classOf(err) != workspace.ClassConversionCrashed {
		t.Fatalf("after the time: %v, %d conversions", err, cv.calls())
	}
}

// Into a workspace on another worker the conversion is not run there; the
// caller streams the result instead.
func TestConvert_IntoAWorkspaceOnAnotherWorkerIsNotSameWorker(t *testing.T) {
	cv := &crashOnce{Client: &convert.InProcess{}}
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 1, Convert: cv, Labels: []string{wire.LabelExtract}})
	f.h.AddWorker("w2", 1)
	o := f.run(t, "alice")
	f.open(t, o, "a")
	f.open(t, o, "b")
	info := f.put(t, o, "a", "x.txt", "hi")
	err := f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertExtract, Args: extractArgs("x.txt", info.SHA256), Source: "a", SHA256: info.SHA256, To: "b"}, nil, nil)
	if !errors.Is(err, ErrNotSameWorker) || cv.calls() != 0 {
		t.Fatalf("err = %v, conversions %d", err, cv.calls())
	}
}
