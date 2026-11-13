package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
)

const token = "test-worker-token"

// recSink records what an extraction wrote, the way the File tool's sink
// does: Markdown, page starts, and each image with the ref it was given.
type recSink struct {
	mu     sync.Mutex
	md     strings.Builder
	starts map[int]int
	images []extract.Image
	// fullAfter refuses text once this many Text calls were taken (0: never).
	fullAfter, texts int
}

func (s *recSink) Text(md string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fullAfter > 0 && s.texts >= s.fullAfter {
		return extract.ErrSinkFull
	}
	s.texts++
	s.md.WriteString(md)
	return nil
}

func (s *recSink) Image(img extract.Image) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images = append(s.images, img)
	return fmt.Sprintf("volume:inbox:extracted/%s", img.Name), nil
}

func (s *recSink) StartPage(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.starts == nil {
		s.starts = map[int]int{}
	}
	s.starts[n] = s.md.Len()
}

// samplePDF is two pages of text with a picture on the first.
func samplePDF(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := range 16 {
		img.Set(x, x, color.RGBA{R: 200, A: 255})
	}
	var j bytes.Buffer
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	return pdftest.Doc{Pages: []pdftest.Page{
		{Lines: []pdftest.Line{{Text: "Minutes of the board", X: 72, Y: 760, Size: 18}, {Text: "The budget is approved.", X: 72, Y: 720, Size: 12}},
			Images: []pdftest.Image{{JPEG: j.Bytes(), W: 16, H: 16, X: 72, Y: 500, Width: 100, Height: 100}}},
		{Lines: []pdftest.Line{{Text: "Second page", X: 72, Y: 760, Size: 12}}},
	}}.Bytes()
}

// pool is a dispatcher on a loopback listener.
func pool(t *testing.T, opt dispatch.Options) (*dispatch.Dispatcher, string) {
	t.Helper()
	opt.Token = token
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(opt)
	go func() { _ = d.Serve(l) }()
	t.Cleanup(d.Stop)
	return d, l.Addr().String()
}

// realWorker runs `loomcycle worker` in this process against target.
func realWorker(t *testing.T, target, id string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.Run(ctx, worker.Options{Target: target, Token: token, ID: id, Scratch: t.TempDir()})
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func waitIdle(t *testing.T, d *dispatch.Dispatcher, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for d.Stats().Idle < n {
		if time.Now().After(deadline) {
			t.Fatalf("pool has %d idle slots, want %d", d.Stats().Idle, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The same file extracted on a worker gives the bytes the in-process client
// gives: the Markdown, where each page starts, every image and its ref, and
// the report down to the per-page yields OCR reads.
func TestPool_ExtractMatchesInProcess(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	realWorker(t, target, "w1")
	waitIdle(t, d, 1)
	data := samplePDF(t)
	opts := extract.Options{Name: "minutes.pdf", Images: true}

	var here recSink
	wantRep, wantErr := (&convert.InProcess{}).Extract(context.Background(), "application/pdf", bytes.NewReader(data), int64(len(data)), &here, opts)
	var there recSink
	gotRep, gotErr := d.Client().Extract(context.Background(), "application/pdf", bytes.NewReader(data), int64(len(data)), &there, opts)

	if !errors.Is(gotErr, wantErr) && (gotErr != nil || wantErr != nil) {
		t.Fatalf("err: pool %v, in process %v", gotErr, wantErr)
	}
	if there.md.String() != here.md.String() {
		t.Errorf("markdown differs:\npool:\n%s\nin process:\n%s", there.md.String(), here.md.String())
	}
	if !reflect.DeepEqual(there.starts, here.starts) {
		t.Errorf("page starts: pool %v, in process %v", there.starts, here.starts)
	}
	if !reflect.DeepEqual(there.images, here.images) || len(here.images) == 0 {
		t.Errorf("images: pool %d, in process %d (and the fixture must have one)", len(there.images), len(here.images))
	}
	if !reflect.DeepEqual(gotRep, wantRep) {
		t.Errorf("report:\npool       %+v\nin process %+v", gotRep, wantRep)
	}
	if st := d.Stats(); st.Jobs["extract/ok"] != 1 {
		t.Errorf("jobs = %v, want one extract/ok", st.Jobs)
	}
}

func TestPool_SelectPagesMatchesInProcess(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	realWorker(t, target, "w1")
	waitIdle(t, d, 1)
	data := samplePDF(t)
	want, err := extract.SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Client().SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{2})
	if err != nil {
		t.Fatal(err)
	}
	// pdfcpu writes a random file /ID, so two cuts of the same pages differ
	// in bytes even in one process: compare what they hold.
	read := func(pdf []byte) (string, extract.Report) {
		var s recSink
		rep, err := (&convert.InProcess{}).Extract(context.Background(), "application/pdf", bytes.NewReader(pdf), int64(len(pdf)), &s, extract.Options{Name: "cut.pdf"})
		if err != nil {
			t.Fatal(err)
		}
		return s.md.String(), rep
	}
	gotMD, gotRep := read(got)
	wantMD, wantRep := read(want)
	if gotMD != wantMD || !reflect.DeepEqual(gotRep, wantRep) || gotRep.Pages != 1 || !strings.Contains(gotMD, "Second page") {
		t.Fatalf("the cut differs:\npool %+v\n%s\nin process %+v\n%s", gotRep, gotMD, wantRep, wantMD)
	}
}

// A sink that takes no more text stops the job on the worker, the caller gets
// ErrSinkFull with what was written, and the worker's slot comes back.
func TestPool_SinkFullStopsTheJobAndFreesTheSlot(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	realWorker(t, target, "w1")
	waitIdle(t, d, 1)
	data := samplePDF(t)
	sink := &recSink{fullAfter: 1}
	rep, err := d.Client().Extract(context.Background(), "application/pdf", bytes.NewReader(data), int64(len(data)), sink, extract.Options{Name: "m.pdf"})
	if !errors.Is(err, extract.ErrSinkFull) {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if rep.Format != "pdf" || sink.md.Len() == 0 {
		t.Fatalf("report %+v, markdown %q: want what was written before the sink filled", rep, sink.md.String())
	}
	waitIdle(t, d, 1)
}

func TestPool_RefusesWhenNoWorkerIsAttached(t *testing.T) {
	d, _ := pool(t, dispatch.Options{})
	data := []byte("a,b\n1,2\n")
	_, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(data), int64(len(data)), &recSink{}, extract.Options{Name: "a.csv"})
	if !errors.Is(err, convert.ErrNoWorkers) {
		t.Fatalf("err = %v, want ErrNoWorkers", err)
	}
}

// With every slot taken, a job waits QueueWait and is then answered as a
// busy server, the answer the in-process limiter gives.
func TestPool_FullPoolAnswersBusy(t *testing.T) {
	d, target := pool(t, dispatch.Options{QueueWait: 100 * time.Millisecond})
	f := attachFake(t, target, "fake", wire.ProtocolVersion)
	holder := f.openSlot(t)
	defer holder.cancel()
	waitIdle(t, d, 1)
	// Take the only slot with a job the fake never finishes.
	go func() {
		_, _ = d.Client().Extract(context.Background(), "text/csv", strings.NewReader("x"), 1, &recSink{}, extract.Options{Name: "x.csv"})
	}()
	holder.recvStart(t)
	data := []byte("a\n")
	_, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(data), int64(len(data)), &recSink{}, extract.Options{Name: "a.csv"})
	if !errors.Is(err, convert.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

// A worker that dies with a job on it fails that job, and the file is
// quarantined: sent again, it is refused without reaching any worker, while
// another file still converts.
func TestPool_AWorkerDeathQuarantinesTheFile(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	f := attachFake(t, target, "doomed", wire.ProtocolVersion)
	s := f.openSlot(t)
	waitIdle(t, d, 1)
	poison := []byte("a,b\n1,2\n")
	errc := make(chan error, 1)
	go func() {
		_, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(poison), int64(len(poison)), &recSink{}, extract.Options{Name: "p.csv"})
		errc <- err
	}()
	s.recvStart(t)
	s.send(t, &workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Accept{Accept: &workerpb.Accept{}}})
	s.readInput(t) // the dispatcher sends input only once it has the Accept
	s.cancel()     // the process is killed mid-job
	if err := <-errc; !errors.Is(err, convert.ErrQuarantined) {
		t.Fatalf("err = %v, want ErrQuarantined", err)
	}

	realWorker(t, target, "healthy")
	waitIdle(t, d, 1)
	if _, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(poison), int64(len(poison)), &recSink{}, extract.Options{Name: "p.csv"}); !errors.Is(err, convert.ErrQuarantined) {
		t.Fatalf("again: err = %v, want ErrQuarantined", err)
	}
	other := []byte("c,d\n3,4\n")
	if _, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(other), int64(len(other)), &recSink{}, extract.Options{Name: "o.csv"}); err != nil {
		t.Fatalf("another file: %v", err)
	}
	st := d.Stats()
	if st.Quarantined != 1 || st.Deaths != 1 || st.Jobs["extract/crashed"] != 1 || st.Jobs["extract/quarantined"] != 1 || st.Jobs["extract/ok"] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// A worker that answers Busy (it is full on another core) costs nothing: the
// job runs on the next slot.
func TestPool_BusyIsRetriedOnAnotherSlot(t *testing.T) {
	d, target := pool(t, dispatch.Options{})
	f := attachFake(t, target, "full", wire.ProtocolVersion)
	s := f.openSlot(t)
	waitIdle(t, d, 1)
	data := []byte("a,b\n1,2\n")
	errc := make(chan error, 1)
	sink := &recSink{}
	go func() {
		_, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(data), int64(len(data)), sink, extract.Options{Name: "a.csv"})
		errc <- err
	}()
	s.recvStart(t)
	realWorker(t, target, "free")
	s.send(t, &workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Busy{Busy: &workerpb.Busy{}}})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.md.String(), "1") {
		t.Fatalf("markdown %q", sink.md.String())
	}
	if st := d.Stats(); st.Quarantined != 0 || st.Deaths != 0 {
		t.Fatalf("a Busy was counted against the file: %+v", st)
	}
}

func TestDispatcher_RefusesAnotherProtocolAndABadToken(t *testing.T) {
	_, target := pool(t, dispatch.Options{})
	conn := dial(t, target)
	_, err := attach(t, conn, "old", wire.ProtocolVersion+1, token)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("another protocol: %v, want FailedPrecondition", err)
	}
	_, err = attach(t, conn, "intruder", wire.ProtocolVersion, "wrong")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: %v, want Unauthenticated", err)
	}
}

func TestDispatcher_DropsAWorkerThatStopsHeartbeating(t *testing.T) {
	d, target := pool(t, dispatch.Options{HeartbeatTimeout: 150 * time.Millisecond})
	attachFake(t, target, "silent", wire.ProtocolVersion)
	deadline := time.Now().Add(5 * time.Second)
	for d.Stats().Workers != 0 || d.Stats().Deaths != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("stats = %+v, want the silent worker dropped", d.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- a worker driven frame by frame ----

func dial(t *testing.T, target string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func outgoing(ctx context.Context, tok, id string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, wire.MDAuthorization, "Bearer "+tok)
}

// attach sends Hello and returns once the dispatcher has answered.
func attach(t *testing.T, conn *grpc.ClientConn, id string, version uint32, tok string) (workerpb.WorkerService_AttachClient, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(outgoing(context.Background(), tok, id))
	t.Cleanup(cancel)
	st, err := workerpb.NewWorkerServiceClient(conn).Attach(ctx)
	if err != nil {
		return nil, err
	}
	if err := st.Send(&workerpb.WorkerStatus{Msg: &workerpb.WorkerStatus_Hello{Hello: &workerpb.Hello{
		WorkerId: id, ProtocolVersion: version, Labels: []string{wire.LabelExtract, wire.LabelPDF}, Slots: 1,
	}}}); err != nil {
		return nil, err
	}
	if _, err := st.Recv(); err != nil {
		return nil, err
	}
	return st, nil
}

type fake struct {
	id   string
	conn *grpc.ClientConn
}

func attachFake(t *testing.T, target, id string, version uint32) *fake {
	t.Helper()
	conn := dial(t, target)
	if _, err := attach(t, conn, id, version, token); err != nil {
		t.Fatal(err)
	}
	return &fake{id: id, conn: conn}
}

type fakeSlot struct {
	st     workerpb.WorkerService_WorkClient
	cancel context.CancelFunc
}

func (f *fake) openSlot(t *testing.T) *fakeSlot {
	t.Helper()
	ctx, cancel := context.WithCancel(outgoing(context.Background(), token, f.id))
	t.Cleanup(cancel)
	st, err := workerpb.NewWorkerServiceClient(f.conn).Work(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Ready{Ready: &workerpb.Ready{WorkerId: f.id}}}); err != nil {
		t.Fatal(err)
	}
	return &fakeSlot{st: st, cancel: cancel}
}

func (s *fakeSlot) recvStart(t *testing.T) *workerpb.JobStart {
	t.Helper()
	f, err := s.st.Recv()
	if err != nil || f.GetStart() == nil {
		t.Fatalf("want JobStart, got %v (%v)", f, err)
	}
	return f.GetStart()
}

// readInput reads the job's input to its end.
func (s *fakeSlot) readInput(t *testing.T) []byte {
	t.Helper()
	var in []byte
	for {
		f, err := s.st.Recv()
		if err != nil {
			t.Fatalf("reading input: %v", err)
		}
		if f.GetInputEnd() != nil {
			return in
		}
		in = append(in, f.GetInputChunk().GetData()...)
	}
}

func (s *fakeSlot) send(t *testing.T, f *workerpb.WorkerFrame) {
	t.Helper()
	if err := s.st.Send(f); err != nil {
		t.Fatal(err)
	}
}
