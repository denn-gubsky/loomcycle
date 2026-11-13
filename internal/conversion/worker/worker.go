// Package worker is `loomcycle worker` (RFC GO §5.1): a process that holds no
// volume, no credential and no run state, dials the dispatcher, and runs the
// conversions it is handed on bytes it spools to a scratch directory of its
// own.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
)

const (
	// DefaultMaxInput bounds one job's input. The core's inputs are bounded
	// already (64 MiB an inbox file by default, 256 MiB a PDF); this is the
	// worker's own floor under them.
	DefaultMaxInput = 1 << 30
	heartbeatEvery  = 5 * time.Second
	backoffMin      = time.Second
	backoffMax      = 30 * time.Second
	// endWait bounds how long a finished job's stream may take to close.
	endWait = 10 * time.Second
	// maxSlots is the dispatcher's own bound on a worker's slots.
	maxSlots = 64
)

// Options configure a worker.
type Options struct {
	// Target is the dispatchers to attach to, comma-separated: each one
	// unix:///path/to/socket, host:port, or dns:///name:port for every
	// address the name resolves to, re-resolved as it changes (one core per
	// address behind a headless service).
	Target string
	// Resolve looks a dns:/// name up; nil is the system resolver.
	Resolve func(ctx context.Context, host string) ([]string, error)
	// ResolveEvery is how often a dns:/// name is looked up again; zero is
	// 30 seconds.
	ResolveEvery time.Duration
	// Token is LOOMCYCLE_WORKER_TOKEN.
	Token string
	// Slots is the jobs run at once (LOOMCYCLE_WORKER_SLOTS); default 1.
	Slots int
	// Scratch is where inputs are spooled (LOOMCYCLE_WORKER_SCRATCH);
	// default the system temp directory.
	Scratch string
	// ID names the worker to the dispatcher; default host-pid-random.
	ID           string
	BuildVersion string
	// ParentPID, when set, ends the worker once its parent is gone: a child
	// the dispatcher started must not outlive it.
	ParentPID int
	MaxInput  int64
	// Rasterize, Media and Encode are the operator's helper commands; the
	// worker takes a helper's jobs only when its program resolves here. A
	// helper's Timeout is ignored: each job carries its own time bound.
	Rasterize, Media, Encode *convert.Helper
	// Images re-encodes images (vision.NewReencoder()); nil takes no image
	// jobs.
	Images convert.ImageReencoder
	Logger *slog.Logger
}

// ErrRefused is a dispatcher refusing this worker for good (another protocol
// version, a bad token): retrying cannot help.
var ErrRefused = errors.New("worker: the dispatcher refused this worker")

// Run serves the dispatcher until ctx ends, reconnecting with backoff.
func Run(ctx context.Context, opt Options) error {
	if opt.Token == "" {
		return errors.New("worker: LOOMCYCLE_WORKER_TOKEN is not set")
	}
	if opt.Target == "" {
		return errors.New("worker: no dispatcher address (--dispatcher)")
	}
	opt.Slots = min(max(opt.Slots, 1), maxSlots)
	if opt.Scratch == "" {
		opt.Scratch = os.TempDir()
	}
	if opt.MaxInput <= 0 {
		opt.MaxInput = DefaultMaxInput
	}
	if opt.ID == "" {
		opt.ID = defaultID()
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	log := opt.Logger.With("component", "conversion-worker", "worker_id", opt.ID)
	if opt.ParentPID > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		safego.Go(ctx, "conversion.worker.parent", func() { watchParent(ctx, opt.ParentPID, cancel) })
	}

	targets, err := parseTargets(opt.Target)
	if err != nil {
		return err
	}
	w := &worker{opt: opt, log: log, labels: labels(opt, log), freed: make(chan struct{})}
	return w.serveTargets(ctx, targets)
}

// attach serves one dispatcher until ctx ends, reconnecting with backoff. It
// returns ErrRefused when the dispatcher refuses this worker for good.
func (w *worker) attach(ctx context.Context, target string) error {
	log := w.log.With("dispatcher", target)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("worker: %s: %w", target, err)
	}
	defer conn.Close()
	client := workerpb.NewWorkerServiceClient(conn)
	backoff := backoffMin
	for {
		start := time.Now()
		err := w.session(ctx, client, log)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRefused) {
			log.Error("the dispatcher refused this worker", "err", err)
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = backoffMin
		}
		log.Warn("lost the dispatcher; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

type worker struct {
	opt    Options
	log    *slog.Logger
	labels []string

	// The worker's slots are shared by every dispatcher it attaches to: it
	// holds an idle stream to each, and runs at most Slots jobs in all.
	mu    sync.Mutex
	used  int
	freed chan struct{} // closed and replaced each time a slot frees
}

// take claims a slot for a job; false when every slot is in use.
func (w *worker) take() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.used >= w.opt.Slots {
		return false
	}
	w.used++
	return true
}

func (w *worker) give() {
	w.mu.Lock()
	w.used--
	close(w.freed)
	w.freed = make(chan struct{})
	w.mu.Unlock()
}

func (w *worker) inFlight() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.used
}

// waitFree waits until a slot is free. A full worker that answered one
// dispatcher Busy opens no new stream to it until then, so the dispatcher's
// job waits in its queue for the slot instead of meeting Busy again.
func (w *worker) waitFree(ctx context.Context) error {
	for {
		w.mu.Lock()
		if w.used < w.opt.Slots {
			w.mu.Unlock()
			return nil
		}
		ch := w.freed
		w.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// labels are what this worker can run: extraction and page cuts always,
// images when it has a re-encoder, and each helper's jobs when the helper's
// program resolves on this host (RFC GO §5.3). A helper that does not is
// logged, not fatal: the worker still serves everything else.
func labels(opt Options, log *slog.Logger) []string {
	out := []string{wire.LabelExtract, wire.LabelPDF}
	if opt.Images != nil {
		out = append(out, wire.LabelImage)
	}
	for _, h := range []struct {
		label  string
		helper *convert.Helper
	}{{wire.LabelRender, opt.Rasterize}, {wire.LabelMedia, opt.Media}, {wire.LabelEncode, opt.Encode}} {
		if h.helper == nil {
			continue
		}
		prog, err := helper.Program(h.helper.Command)
		if err == nil {
			_, err = exec.LookPath(prog)
		}
		if err != nil {
			log.Warn("conversion worker cannot run a helper; it takes none of its jobs", "helper", h.helper.Key, "err", err)
			continue
		}
		out = append(out, h.label)
	}
	return out
}

func (w *worker) outgoing(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, wire.MDAuthorization, "Bearer "+w.opt.Token)
}

// session is one attachment: Hello, heartbeats, and a loop per slot.
func (w *worker) session(parent context.Context, client workerpb.WorkerServiceClient, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(w.outgoing(parent))
	defer cancel()
	att, err := client.Attach(ctx)
	if err != nil {
		return err
	}
	var limit uint64
	if l := debug.SetMemoryLimit(-1); l > 0 && l < 1<<62 {
		limit = uint64(l)
	}
	if err := att.Send(&workerpb.WorkerStatus{Msg: &workerpb.WorkerStatus_Hello{Hello: &workerpb.Hello{
		WorkerId: w.opt.ID, ProtocolVersion: wire.ProtocolVersion, BuildVersion: w.opt.BuildVersion,
		Labels: w.labels, Slots: uint32(w.opt.Slots), MemoryLimit: limit,
	}}}); err != nil {
		return err
	}
	ctl, err := att.Recv()
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition, codes.Unauthenticated, codes.InvalidArgument:
			return fmt.Errorf("%w: %v", ErrRefused, status.Convert(err).Message())
		}
		return err
	}
	if b := ctl.GetWelcome().GetBuildVersion(); b != w.opt.BuildVersion {
		log.Warn("the dispatcher runs another build", "dispatcher_build", b, "build", w.opt.BuildVersion)
	}
	log.Info("attached to the dispatcher", "slots", w.opt.Slots, "labels", w.labels)

	var wg sync.WaitGroup
	draining := make(chan struct{})
	for range w.opt.Slots {
		wg.Add(1)
		safego.Go(ctx, "conversion.worker.slot", func() {
			defer wg.Done()
			for ctx.Err() == nil {
				select {
				case <-draining:
					return
				default:
				}
				if err := w.work(ctx, client); err != nil && ctx.Err() == nil {
					log.Warn("a work slot ended", "err", err)
					select {
					case <-ctx.Done():
					case <-time.After(backoffMin):
					}
				}
			}
		})
	}
	errc := make(chan error, 1)
	safego.Go(ctx, "conversion.worker.control", func() {
		var drainOnce sync.Once
		for {
			m, err := att.Recv()
			if err != nil {
				errc <- err
				return
			}
			switch {
			case m.GetShutdown() != nil:
				errc <- errors.New("the dispatcher asked this worker to shut down")
				return
			case m.GetDrain() != nil:
				drainOnce.Do(func() { close(draining) })
			}
		}
	})
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case err := <-errc:
			cancel()
			wg.Wait()
			return err
		case <-t.C:
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if err := att.Send(&workerpb.WorkerStatus{Msg: &workerpb.WorkerStatus_Heartbeat{Heartbeat: &workerpb.Heartbeat{
				InFlight: uint32(w.inFlight()), Rss: ms.Sys,
			}}}); err != nil {
				cancel()
				wg.Wait()
				return err
			}
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
	}
}

// work holds one idle stream until a job arrives, runs it when a slot is
// free or answers Busy when none is, and ends the stream.
func (w *worker) work(ctx context.Context, client workerpb.WorkerServiceClient) error {
	if err := w.waitFree(ctx); err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.Work(sctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Ready{Ready: &workerpb.Ready{WorkerId: w.opt.ID}}}); err != nil {
		return err
	}
	f, err := stream.Recv()
	if err != nil {
		return err
	}
	start := f.GetStart()
	if start == nil {
		return errors.New("the first job frame was not JobStart")
	}
	var done <-chan struct{}
	if !w.take() {
		// Every slot is running a job another dispatcher handed over;
		// nothing of this one has been read, so it runs elsewhere.
		if err := stream.Send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Busy{Busy: &workerpb.Busy{}}}); err != nil {
			return err
		}
	} else {
		done, err = w.runJob(sctx, stream, start)
		w.give()
		if err != nil {
			return err
		}
	}
	// Let Done reach the dispatcher before the stream goes: close our side
	// and wait for the dispatcher to end it. Only one goroutine receives on
	// a stream: the job's own receiver when it ran, else this one.
	if err := stream.CloseSend(); err != nil {
		return err
	}
	if done == nil {
		ch := make(chan struct{})
		safego.Go(sctx, "conversion.worker.drain", func() {
			defer close(ch)
			for {
				if _, err := stream.Recv(); err != nil {
					return
				}
			}
		})
		done = ch
	}
	select {
	case <-done:
	case <-time.After(endWait):
	}
	return nil
}

// runJob accepts a job, spools its input, runs it and sends Done. recvDone,
// when not nil, closes once the job's receiver has seen the stream end.
func (w *worker) runJob(ctx context.Context, stream workerpb.WorkerService_WorkClient, start *workerpb.JobStart) (recvDone <-chan struct{}, err error) {
	log := w.log.With("job_id", start.GetJobId(), "kind", wire.KindName(start.GetKind()))
	send := func(f *workerpb.WorkerFrame) error { return stream.Send(f) }
	if err := send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Accept{Accept: &workerpb.Accept{}}}); err != nil {
		return nil, err
	}
	in, inErr, err := w.spool(stream, start.GetInputSize())
	if in != nil {
		defer func() {
			name := in.Name()
			in.Close()
			os.Remove(name)
		}()
	}
	if err != nil {
		return nil, err
	}
	if inErr != nil {
		return nil, sendDone(send, extract.Report{}, "", workerpb.ErrorClass_ERROR_CLASS_INPUT, inErr.Error())
	}

	timeout := time.Duration(start.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Minute
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// After the input, the dispatcher sends only ImageRef and Cancel.
	refs := make(chan string, 1)
	var sinkFull atomic.Bool
	received := make(chan struct{})
	safego.Go(ctx, "conversion.worker.recv", func() {
		defer close(received)
		for {
			f, err := stream.Recv()
			if err != nil {
				cancel()
				return
			}
			switch m := f.GetMsg().(type) {
			case *workerpb.JobFrame_ImageRef:
				select {
				case refs <- m.ImageRef.GetRef():
				default:
				}
			case *workerpb.JobFrame_Cancel:
				if m.Cancel.GetReason() == workerpb.CancelReason_CANCEL_REASON_SINK_FULL {
					sinkFull.Store(true)
				} else {
					cancel()
				}
			}
		}
	})

	var (
		rep       extract.Report
		out       []byte
		spool     convert.Spool
		mediaType string
		jerr      error
	)
	p := w.converter(timeout)
	func() {
		// A parser bug is this file's failure, not the worker's: the other
		// slots keep running.
		defer func() {
			if p := recover(); p != nil {
				log.Error("a conversion panicked", "panic", fmt.Sprint(p))
				jerr = fmt.Errorf("the converter failed on this file (%v)", p)
			}
		}()
		size := start.GetInputSize()
		switch start.GetKind() {
		case workerpb.JobKind_JOB_KIND_EXTRACT:
			o := start.GetExtract()
			sink := &remoteSink{ctx: jctx, send: send, refs: refs, full: &sinkFull}
			rep, jerr = p.Extract(jctx, start.GetMediaType(), in, size, sink,
				extract.Options{Name: start.GetName(), Images: o.GetImages(), MaxRows: int(o.GetMaxRows()), InferHeadings: o.GetInferHeadings()})
		case workerpb.JobKind_JOB_KIND_PDF_SELECT_PAGES:
			pages := make([]int, len(start.GetPages()))
			for i, p := range start.GetPages() {
				pages[i] = int(p)
			}
			out, jerr = p.SelectPages(jctx, in, size, pages)
		case workerpb.JobKind_JOB_KIND_PDF_RENDER:
			var data []byte
			if data, jerr = readAll(in, size); jerr == nil {
				out, jerr = p.RenderPage(jctx, data, start.GetLanguage())
			}
		case workerpb.JobKind_JOB_KIND_AUDIO_ENCODE:
			if !formatName.MatchString(start.GetFormat()) {
				jerr = fmt.Errorf("%q is not an audio format name", start.GetFormat())
				break
			}
			var data []byte
			if data, jerr = readAll(in, size); jerr == nil {
				out, jerr = p.EncodeAudio(jctx, data, start.GetFormat())
			}
		case workerpb.JobKind_JOB_KIND_IMAGE_REENCODE:
			var data []byte
			if data, jerr = readAll(in, size); jerr == nil {
				out, mediaType, jerr = p.ReencodeImage(jctx, data)
			}
		case workerpb.JobKind_JOB_KIND_MEDIA_CONVERT:
			spool, jerr = p.ConvertMedia(jctx, in, size, start.GetMediaType(), start.GetLanguage())
		default:
			jerr = fmt.Errorf("this worker does not run %s jobs", start.GetKind())
		}
	}()
	if spool != nil {
		defer spool.Close()
		if err := sendSpool(send, spool); err != nil {
			return received, err
		}
	}
	for len(out) > 0 {
		n := min(len(out), wire.ChunkSize)
		if err := send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_OutputChunk{OutputChunk: &workerpb.Chunk{Data: out[:n]}}}); err != nil {
			return received, err
		}
		out = out[n:]
	}
	class, msg := wire.ErrorToWire(jerr)
	return received, sendDone(send, rep, mediaType, class, msg)
}

// formatName is what an encode's format may be: it names the encoder's
// output file.
var formatName = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// converter runs this worker's jobs in its own process: its helpers under
// the job's time bound, in scratch folders of its own scratch directory.
func (w *worker) converter(timeout time.Duration) *convert.InProcess {
	bound := func(h *convert.Helper) *convert.Helper {
		if h == nil {
			return nil
		}
		c := *h
		c.Timeout = timeout
		return &c
	}
	return &convert.InProcess{
		Rasterize: bound(w.opt.Rasterize), Media: bound(w.opt.Media), Encode: bound(w.opt.Encode),
		Images:  w.opt.Images,
		Scratch: func(context.Context) (string, error) { return w.opt.Scratch, nil },
	}
}

// readAll reads a spooled input the job needs whole; it is bounded by
// MaxInput already.
func readAll(in io.ReaderAt, size int64) ([]byte, error) {
	data := make([]byte, size)
	if _, err := in.ReadAt(data, 0); err != nil && !(errors.Is(err, io.EOF) && size == 0) {
		return nil, fmt.Errorf("reading the input: %w", err)
	}
	return data, nil
}

// sendSpool sends a converted file as output chunks.
func sendSpool(send func(*workerpb.WorkerFrame) error, s convert.Spool) error {
	buf := make([]byte, wire.ChunkSize)
	for off := int64(0); off < s.Size(); {
		n, err := s.ReadAt(buf[:min(int64(len(buf)), s.Size()-off)], off)
		if n > 0 {
			if serr := send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_OutputChunk{OutputChunk: &workerpb.Chunk{Data: buf[:n]}}}); serr != nil {
				return serr
			}
			off += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func sendDone(send func(*workerpb.WorkerFrame) error, rep extract.Report, mediaType string, class workerpb.ErrorClass, msg string) error {
	return send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Done{Done: &workerpb.Done{
		Report: wire.ReportToWire(rep), ErrorClass: class, Message: msg, MediaType: mediaType,
	}}})
}

// spool reads the job's input into a scratch file. inErr is an input the
// worker will not run (over its bound, or not the size announced); the input
// is still read to its end so the stream stays in step.
func (w *worker) spool(stream workerpb.WorkerService_WorkClient, size int64) (f *os.File, inErr error, err error) {
	if size < 0 || size > w.opt.MaxInput {
		inErr = fmt.Errorf("the input is %d bytes; this worker takes at most %d", size, w.opt.MaxInput)
	} else if f, err = os.CreateTemp(w.opt.Scratch, "job-*"); err != nil {
		return nil, nil, fmt.Errorf("worker scratch: %w", err)
	}
	var got int64
	for {
		m, err := stream.Recv()
		if err != nil {
			return f, inErr, err
		}
		if m.GetInputEnd() != nil {
			break
		}
		c := m.GetInputChunk()
		if c == nil {
			return f, inErr, errors.New("an input frame was neither a chunk nor its end")
		}
		got += int64(len(c.GetData()))
		if inErr == nil && got > size {
			inErr = fmt.Errorf("the input ran past the %d bytes announced", size)
		}
		if inErr == nil {
			if _, err := f.Write(c.GetData()); err != nil {
				return f, inErr, fmt.Errorf("worker scratch: %w", err)
			}
		}
	}
	if inErr == nil && got != size {
		inErr = fmt.Errorf("the input was %d bytes, not the %d announced", got, size)
	}
	return f, inErr, nil
}

// remoteSink is the extractor's Sink on the worker side: everything it is
// given goes to the dispatcher, and an image waits for the reference the
// core's sink returned.
type remoteSink struct {
	ctx  context.Context
	send func(*workerpb.WorkerFrame) error
	refs <-chan string
	full *atomic.Bool
}

func (s *remoteSink) Text(md string) error {
	if s.full.Load() {
		return extract.ErrSinkFull
	}
	for len(md) > 0 {
		n := min(len(md), wire.ChunkSize)
		// Cut on a rune boundary.
		for n < len(md) && n > 0 && md[n]&0xC0 == 0x80 {
			n--
		}
		if err := s.send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_Text{Text: &workerpb.Text{Markdown: md[:n]}}}); err != nil {
			return err
		}
		md = md[n:]
	}
	return nil
}

func (s *remoteSink) StartPage(n int) {
	_ = s.send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_PageStart{PageStart: &workerpb.PageStart{Page: wire.I32(n)}}})
}

func (s *remoteSink) Image(img extract.Image) (string, error) {
	if err := s.send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_ImageBegin{ImageBegin: &workerpb.ImageBegin{
		Name: img.Name, MediaType: img.MediaType, Alt: img.Alt, Page: wire.I32(img.Page), Slide: wire.I32(img.Slide), Size: int64(len(img.Data)),
	}}}); err != nil {
		return "", err
	}
	for data := img.Data; len(data) > 0; {
		n := min(len(data), wire.ChunkSize)
		if err := s.send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_ImageChunk{ImageChunk: &workerpb.Chunk{Data: data[:n]}}}); err != nil {
			return "", err
		}
		data = data[n:]
	}
	if err := s.send(&workerpb.WorkerFrame{Msg: &workerpb.WorkerFrame_ImageEnd{ImageEnd: &workerpb.ImageEnd{}}}); err != nil {
		return "", err
	}
	select {
	case ref := <-s.refs:
		return ref, nil
	case <-s.ctx.Done():
		return "", s.ctx.Err()
	}
}

var _ extract.PageSink = (*remoteSink)(nil)

// watchParent ends the worker when its parent process is gone (its parent
// pid changes to the reaper's).
func watchParent(ctx context.Context, pid int, cancel context.CancelFunc) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if os.Getppid() != pid {
				cancel()
				return
			}
		}
	}
}

func defaultID() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

var _ io.ReaderAt = (*os.File)(nil)
